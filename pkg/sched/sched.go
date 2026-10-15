// SPDX-License-Identifier: Apache-2.0

// Package sched is the daemon's timer service: a Scheduler fires a callback
// at a deadline, keyed by string. Jobs are replaced by key rather than
// duplicated, cancelled by key, introspectable while pending, and shutdown is
// bounded — Stop closes registration first, then waits a bounded time for
// callbacks that are already running before abandoning them. It is built on
// pkg/clock so tests can drive it deterministically.
package sched

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/clock"
)

// Entry is one pending job, for introspection.
type Entry struct {
	Key      string
	Deadline time.Time
}

// job is one scheduled callback. A *job's pointer identity is what run uses
// to tell a live timer from a stale one: At replaces the map entry by key, so
// a timer that fires after its job was replaced or cancelled no longer
// matches and its callback never runs.
type job struct {
	key      string
	deadline time.Time
	fn       func()
	timer    clock.Timer
}

// Scheduler fires callbacks at deadlines on the clock it was built with.
//
// Every method is safe on a nil *Scheduler: it is a no-op (Pending returns
// nil), so a test-constructed Controller{} with no scheduler never panics.
type Scheduler struct {
	mu           sync.Mutex
	clk          clock.Clock
	pending      map[string]*job
	running      map[string]time.Time // key → when its callback started
	wg           sync.WaitGroup       // counts running callbacks
	closed       bool
	abandonAfter time.Duration
}

// New returns a Scheduler on clk. abandonAfter bounds how long Stop waits for
// running callbacks; an abandonAfter <= 0 abandons them immediately, with no
// grace period.
func New(clk clock.Clock, abandonAfter time.Duration) *Scheduler {
	return &Scheduler{
		clk:          clk,
		pending:      make(map[string]*job),
		running:      make(map[string]time.Time),
		abandonAfter: abandonAfter,
	}
}

// At schedules fn to run at deadline, replacing any pending job already
// registered under key. A past deadline fires promptly — it is passed to the
// clock as a negative duration, never dropped. At on a closed scheduler (one
// that has been Stopped) is a no-op, so a callback that fires during the
// shutdown drain cannot schedule a successor.
func (s *Scheduler) At(key string, deadline time.Time, fn func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if old, ok := s.pending[key]; ok {
		old.timer.Stop()
		delete(s.pending, key)
	}
	j := &job{key: key, deadline: deadline, fn: fn}
	j.timer = s.clk.AfterFunc(deadline.Sub(s.clk.Now()), func() { s.run(j) })
	s.pending[key] = j
}

// Cancel removes the pending job registered under key, if any. Cancelling an
// unknown or already-fired key is a no-op, not an error.
func (s *Scheduler) Cancel(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.pending[key]; ok {
		j.timer.Stop()
		delete(s.pending, key)
	}
}

// Pending reports the jobs currently waiting to fire, sorted by Deadline then
// Key. It is introspection only.
func (s *Scheduler) Pending() []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.pending))
	for _, j := range s.pending {
		out = append(out, Entry{Key: j.key, Deadline: j.deadline})
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].Deadline.Equal(out[k].Deadline) {
			return out[i].Deadline.Before(out[k].Deadline)
		}
		return out[i].Key < out[k].Key
	})
	return out
}

// Stop shuts the scheduler down: every pending timer is stopped, At and
// Cancel become no-ops, and Stop waits up to abandonAfter for callbacks that
// are already running before abandoning them. An abandoned callback's
// goroutine leaks until process exit — intended. Stop is idempotent.
//
// Registration is closed BEFORE the drain: a periodic callback that fires
// during the drain cannot schedule a successor, because its At is a no-op.
//
// The abandon timeout deliberately uses the real clock, not the injected one:
// shutdown must not depend on a FakeClock being advanced.
//
// A Stop called from inside a running callback waits on itself for the full
// abandonAfter and then logs its own key as abandoned.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, j := range s.pending {
		j.timer.Stop()
	}
	s.pending = nil
	// run's wg.Add happens under this same lock and only for a job still in
	// pending, so no Add can race the Wait below: every callback that is
	// running at this point is already counted.
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.abandonAfter):
		s.mu.Lock()
		for k, start := range s.running {
			slog.Warn("sched: abandoning running job at shutdown", "key", k, "runningFor", time.Since(start).String())
		}
		s.mu.Unlock()
	}
}

// run fires one job's callback — in the timer's own goroutine, with no
// scheduler lock held — after checking that j is still the job registered
// under its key. The pointer-identity guard is what makes replace-by-key safe
// even when the old timer already fired and its callback is racing the
// replacement: only the job currently in pending may run.
func (s *Scheduler) run(j *job) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.pending[j.key] != j || s.closed {
		// Stale: the job was replaced or cancelled, or the scheduler is
		// closed. Its timer fired, but the callback must not.
		s.mu.Unlock()
		return
	}
	delete(s.pending, j.key)
	start := s.clk.Now()
	s.running[j.key] = start
	s.wg.Add(1)
	s.mu.Unlock()

	// fn runs in the CURRENT goroutine — never another one, so FakeClock
	// tests observe callbacks inside Advance — and with no scheduler lock
	// held, so a callback may call back into the scheduler (At, Cancel,
	// Pending) without deadlocking.
	defer func() {
		if r := recover(); r != nil {
			// A panicking callback must not take the daemon down; the
			// running/wg cleanup below still runs.
			slog.Error("sched: callback panicked", "key", j.key, "panic", r)
		}
		s.mu.Lock()
		// A replacement under the same key may have started since and
		// overwritten the start time; clear only our own entry.
		if t, ok := s.running[j.key]; ok && t.Equal(start) {
			delete(s.running, j.key)
		}
		s.mu.Unlock()
		s.wg.Done()
	}()
	j.fn()
}
