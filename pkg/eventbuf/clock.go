// Package eventbuf coalesces externally-injected agent events — subagent
// transitions, budget warnings, executor loss — into debounced, batched frames
// so N events cost one model turn instead of N.
package eventbuf

import (
	"sort"
	"sync"
	"time"
)

// Clock is a time source with a timer factory, abstracted so the event buffer
// can be tested with deterministic time rather than real timers.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a cancellable one-shot timer. Stop returns true when the timer was
// pending and is now cancelled; false when it has already fired or was already
// stopped.
type Timer interface {
	Stop() bool
}

// realClock implements Clock with the wall clock.
type realClock struct{}

func (realClock) Now() time.Time                                 { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer      { return time.AfterFunc(d, f) }

// RealClock returns a Clock that uses the wall clock.
func RealClock() Clock { return realClock{} }

// fakeEntry is one pending timer within FakeClock.
type fakeEntry struct {
	deadline time.Time
	fn       func()
	stopped  bool
}

// FakeClock is a deterministic clock for tests. Time only advances via
// Advance; timer callbacks fire during Advance when the clock passes their
// deadline. Callbacks run outside FakeClock's internal mutex so a callback
// that pushes into a buffer (which calls back into the clock) does not
// deadlock.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []*fakeEntry
}

// NewFakeClock returns a FakeClock whose Now() returns t.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f to run after d has elapsed from the current fake time.
func (c *FakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &fakeEntry{deadline: c.now.Add(d), fn: f}
	c.pending = append(c.pending, e)
	return &fakeTimer{entry: e, clock: c}
}

// Advance moves the clock forward by d and invokes every pending callback
// whose deadline has passed, in deadline order. Callbacks run outside the lock
// so they may safely interact with the clock.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	c.fireElapsed()
}

func (c *FakeClock) fireElapsed() {
	for {
		c.mu.Lock()
		// Collect every entry whose deadline has passed and is not stopped.
		var due []*fakeEntry
		for _, e := range c.pending {
			if !c.now.Before(e.deadline) && !e.stopped {
				e.stopped = true // mark fired so Stop returns false
				due = append(due, e)
			}
		}
		// Remove fired entries from the pending slice.
		kept := c.pending[:0]
		for _, e := range c.pending {
			if !e.stopped {
				kept = append(kept, e)
			}
		}
		c.pending = kept
		c.mu.Unlock()

		if len(due) == 0 {
			return
		}
		// Sort by deadline for deterministic firing order.
		sort.Slice(due, func(i, j int) bool {
			return due[i].deadline.Before(due[j].deadline)
		})
		for _, e := range due {
			e.fn()
		}
	}
}

// fakeTimer implements Timer for FakeClock.
type fakeTimer struct {
	entry *fakeEntry
	clock *FakeClock
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.entry.stopped {
		return false
	}
	t.entry.stopped = true
	return true
}
