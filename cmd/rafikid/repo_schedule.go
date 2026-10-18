// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/repos"
)

// repoRefreshTimeout bounds ONE scheduled refresh. A fetch plus a venv build
// can legitimately take minutes, but a fire that never gives up would hold the
// source's coalescing slot — and therefore every other caller's refresh of it
// — for as long as the network takes to fail.
const repoRefreshTimeout = 10 * time.Minute

// repoScheduler keeps one timer per repo with an opt-in refresh interval, and
// nothing else: no ticker and no poll loop. Each repo's next fire is
// ResolvedAt + RefreshInterval, and a fire re-arms from when it finished, so a
// slow refresh cannot stack up behind itself.
//
// Refresh is never on by default, for any repo: a row whose interval is NULL
// (Record.RefreshInterval == 0) has no timer at all, and clearing the interval
// disarms the one it had.
//
// The scheduler is the one place a refresh failure is LOGGED rather than
// returned — there is no caller to return it to. It logs once per outage (a
// repo that fails every five minutes must not write a warning every five
// minutes) and once on recovery.
type repoScheduler struct {
	pusher *repoPusher
	store  repos.Store

	// now and after are the clock and timer factory; production uses the real
	// ones, tests substitute a clock they advance and a factory that hands
	// back the fire function to call.
	now   func() time.Time
	after func(time.Duration, func()) *time.Timer
	log   *slog.Logger

	fireTimeout time.Duration

	// ctx is the scheduler's lifetime, cancelled by stop: an in-flight fire is
	// cancelled with it rather than outliving the daemon.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	timers  map[string]*time.Timer // keyed by repoKey(owner, name)
	gen     map[string]int         // bumped on every arm/disarm; a stale fire is dropped
	lastErr map[string]string      // the last reported refresh error text, "" when healthy
	// interval remembers the last known schedule per source, so a fire whose
	// store read failed can still re-arm instead of ending the schedule on a
	// transient outage.
	interval map[string]time.Duration
	stopped  bool
}

// newRepoScheduler builds a scheduler over pusher and store, with the real
// clock and time.AfterFunc.
func newRepoScheduler(pusher *repoPusher, store repos.Store) *repoScheduler {
	return &repoScheduler{
		pusher:      pusher,
		store:       store,
		now:         time.Now,
		after:       time.AfterFunc,
		log:         slog.Default(),
		fireTimeout: repoRefreshTimeout,
		timers:      make(map[string]*time.Timer),
		gen:         make(map[string]int),
		lastErr:     make(map[string]string),
		interval:    make(map[string]time.Duration),
	}
}

// start arms every registered source that has an interval, and gives the
// scheduler its lifetime: cancelling parent (or calling stop) ends it.
//
// The read is ListAll because the schedule belongs to the daemon, not to a
// caller — every owner's sources are armed, exactly as the boot walk finds
// them. A store that cannot be read leaves the scheduler idle with a warning
// rather than failing the daemon's startup: repos then refresh on demand.
func (s *repoScheduler) start(parent context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	ctx := s.ctx
	s.mu.Unlock()

	records, err := s.store.ListAll(ctx)
	if err != nil {
		s.log.Warn("repo scheduler: cannot list repos; no scheduled refreshes armed", "error", err)
		return
	}
	for _, rec := range records {
		s.scheduleRecord(rec)
	}
}

// stop disarms every timer and cancels an in-flight fire. Idempotent: a second
// call (the shutdown path's, after an early one) is a no-op.
func (s *repoScheduler) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	for key, t := range s.timers {
		t.Stop()
		delete(s.timers, key)
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// arm (re)reads the source's registration and schedules it: ResolvedAt +
// RefreshInterval, or immediately when it has never been resolved. A source
// with no interval, or no row at all, is disarmed — that is how AddRepo,
// SetRepo and RemoveRepo keep the timer in step with the store.
func (s *repoScheduler) arm(ownerUserID, name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	ctx := s.ctx
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rec, err := s.pusher.recordFor(ctx, ownerUserID, name)
	if err != nil {
		// No row (removed) or no store answer: nothing to arm against. The
		// store's own state is authoritative, so the next arm/disarm fixes it.
		s.disarm(ownerUserID, name)
		return
	}
	s.scheduleRecord(rec)
}

// disarm stops the source's timer and forgets everything remembered about it.
// Called by RemoveRepo, and by arm when the row says "on demand".
func (s *repoScheduler) disarm(ownerUserID, name string) {
	if s == nil {
		return
	}
	key := repoKey(ownerUserID, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen[key]++
	if t, ok := s.timers[key]; ok {
		t.Stop()
		delete(s.timers, key)
	}
	delete(s.lastErr, key)
	delete(s.interval, key)
}

// scheduleRecord arms rec at ResolvedAt + RefreshInterval, or disarms it when
// it has no interval.
func (s *repoScheduler) scheduleRecord(rec repos.Record) {
	if rec.RefreshInterval <= 0 {
		s.disarm(rec.OwnerUserID, rec.Name)
		return
	}
	var delay time.Duration
	if !rec.ResolvedAt.IsZero() {
		delay = rec.ResolvedAt.Add(rec.RefreshInterval).Sub(s.now())
		if delay < 0 {
			// The schedule came due while the daemon was not running (or the
			// source was resolved long ago): fire now rather than in the past.
			delay = 0
		}
	}
	s.schedule(rec.OwnerUserID, rec.Name, rec.RefreshInterval, delay)
}

// schedule arms (or re-arms) one source's timer for delay from now, replacing
// any timer it already had.
func (s *repoScheduler) schedule(ownerUserID, name string, interval, delay time.Duration) {
	s.install(ownerUserID, name, interval, delay, nil)
}

// rearm is a FIRE's own re-arm: it installs the next timer only if the source
// has not been armed or disarmed since that fire was scheduled. Without the
// check the fire would overwrite a `repo remove` (leaving a timer for a repo
// that no longer exists) or a `repo set --refresh off` (resurrecting the
// schedule the operator just cleared) that landed while it ran.
func (s *repoScheduler) rearm(ownerUserID, name string, interval, delay time.Duration, gen int) bool {
	return s.install(ownerUserID, name, interval, delay, &gen)
}

// install arms one source's timer, optionally only when the generation still
// matches expectGen. It reports whether a timer was installed.
func (s *repoScheduler) install(ownerUserID, name string, interval, delay time.Duration, expectGen *int) bool {
	key := repoKey(ownerUserID, name)
	s.mu.Lock()
	if s.stopped || (expectGen != nil && s.gen[key] != *expectGen) {
		s.mu.Unlock()
		return false
	}
	s.gen[key]++
	gen := s.gen[key]
	s.interval[key] = interval
	if t, ok := s.timers[key]; ok {
		t.Stop()
		delete(s.timers, key)
	}
	s.mu.Unlock()

	timer := s.after(delay, func() { s.fire(ownerUserID, name, gen) })

	s.mu.Lock()
	defer s.mu.Unlock()
	// The timer may already have fired (and re-armed itself) while we were
	// outside the lock; a stale generation means exactly that, and storing
	// this handle would forget the newer one.
	if s.stopped || s.gen[key] != gen {
		timer.Stop()
		return false
	}
	s.timers[key] = timer
	return true
}

// fire runs one scheduled refresh and re-arms it. gen is the generation the
// timer was armed with: if the source was re-armed or disarmed in the
// meantime, this fire is stale and does nothing.
func (s *repoScheduler) fire(ownerUserID, name string, gen int) {
	key := repoKey(ownerUserID, name)
	s.mu.Lock()
	if s.stopped || s.gen[key] != gen {
		s.mu.Unlock()
		return
	}
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, s.fireTimeout)
	s.mu.Unlock()
	defer cancel()

	rec, err := s.pusher.recordFor(ctx, ownerUserID, name)
	if err != nil {
		s.report(ownerUserID, name, err)
		if errors.Is(err, repos.ErrNotFound) {
			s.disarm(ownerUserID, name)
			return
		}
		// A store outage says nothing about the source: keep the schedule
		// alive on the interval we last knew, rather than ending it silently.
		if interval, ok := s.intervalFor(key); ok {
			s.schedule(ownerUserID, name, interval, interval)
		}
		return
	}

	_, err = s.pusher.refresh(ctx, ownerUserID, name, rec.URL, rec.Ref)
	s.report(ownerUserID, name, err)

	// Re-arm from NOW, not from the schedule's ideal instant: a refresh that
	// took longer than its interval must not fire again immediately.
	//
	// The interval is re-read rather than taken from the record this fire
	// started with, and the generation is rechecked: the store and the
	// scheduler both move while a refresh runs, and `repo set --refresh off`
	// or `repo remove` landing in that window must win. rearm refuses when
	// either changed, so the operator's write is never overwritten.
	next, nerr := s.pusher.recordFor(ctx, ownerUserID, name)
	if nerr != nil {
		if errors.Is(nerr, repos.ErrNotFound) {
			// The repo is gone; so is its schedule.
			s.disarm(ownerUserID, name)
			return
		}
		// A store outage says nothing about the source: keep the schedule
		// alive on the interval we last knew rather than ending it silently.
		if interval, ok := s.intervalFor(repoKey(ownerUserID, name)); ok {
			s.rearm(ownerUserID, name, interval, interval, gen)
		}
		return
	}
	if next.RefreshInterval <= 0 {
		s.disarm(ownerUserID, name)
		return
	}
	s.rearm(ownerUserID, name, next.RefreshInterval, next.RefreshInterval, gen)
}

// intervalFor returns the last interval the scheduler knew for key.
func (s *repoScheduler) intervalFor(key string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.interval[key]
	return d, ok
}

// report logs a fire's outcome once per outage: a repeated identical error is
// the same outage, and a success after one is the recovery. Both are logged
// exactly once, so a source that fails every five minutes produces two lines
// per outage rather than one per fire.
func (s *repoScheduler) report(ownerUserID, name string, err error) {
	key := repoKey(ownerUserID, name)
	text := ""
	if err != nil {
		text = err.Error()
	}
	s.mu.Lock()
	prev := s.lastErr[key]
	_, seen := s.lastErr[key]
	s.lastErr[key] = text
	s.mu.Unlock()

	switch {
	case err == nil && seen && prev != "":
		s.log.Info("scheduled repo refresh recovered",
			"source", name, "owner", repoOwnerDisplay(ownerUserID))
	case err != nil && text != prev:
		s.log.Warn("scheduled repo refresh failed",
			"source", name, "owner", repoOwnerDisplay(ownerUserID), "error", err)
	}
}
