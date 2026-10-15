package eventbuf

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/inbox"

	"github.com/multigres/testkit/assert"
)

// Pending is the daemon's window into "queued news not yet delivered": true
// from the moment a fragment is accepted until the flush clears it, so a child
// idle inside its debounce can be shown as waiting on events rather than idle.
func TestPendingTrueBetweenPushAndFlush(t *testing.T) {
	c := assert.NewAborting(t)
	b, clk, rec, _ := newTestBuffer(t)

	c.False(b.Pending("c_w"), "Pending before any push")
	b.Push("c_w", "subagents", "", "worker finished")
	c.True(b.Pending("c_w"), "Pending after a push, before the debounce elapses")

	clk.Advance(5 * time.Second)
	c.Eq(1, rec.n(), "flushes")
	c.False(b.Pending("c_w"), "Pending after the debounce flush cleared the batch")
}

// A batch deferred because the child was mid-turn is still queued news: it
// must read pending for as long as it waits for the idle transition, and
// clear only when DrainIdle actually delivers it.
func TestPendingTrueWhileDeferredBusy(t *testing.T) {
	c := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	st := inbox.NewMemory()
	rec := &flushRecorder{st: st}
	var busy atomic.Bool
	busy.Store(true)
	b := New(Config{Debounce: 5 * time.Second, MaxWait: 60 * time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return busy.Load() })

	b.Push("c_w", "subagents", "", "worker finished")
	clk.Advance(10 * time.Second) // timer fires, defers on busy
	c.Eq(0, rec.n(), "flushed while busy")
	c.True(b.Pending("c_w"), "a deferred batch must still read pending")

	busy.Store(false)
	b.DrainIdle("c_w")
	c.Eq(1, rec.n(), "flushes after idle")
	c.False(b.Pending("c_w"), "Pending after DrainIdle delivered the deferred batch")
}

// News for one child must not mark another child as waiting on events.
func TestPendingIsPerChild(t *testing.T) {
	c := assert.NewAborting(t)
	b, _, _, _ := newTestBuffer(t)
	b.Push("c_a", "subagents", "", "a finished")
	c.True(b.Pending("c_a"), "Pending for the pushed child")
	c.False(b.Pending("c_b"), "Pending for an untouched child")
}

// An orphan never reached the store, so it is news the child has not been told
// about: Pending must stay true until the orphan is flushed.
func TestPendingCountsOrphans(t *testing.T) {
	c := assert.NewAborting(t)
	quietLogs(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	rec := &flushRecorder{}
	st := failingStore{inbox.NewMemory()}
	var busy atomic.Bool
	busy.Store(true)
	b := New(Config{Debounce: 5 * time.Second}, clk)
	b.SetAccepter(inbox.NewQueue(inbox.QueueConfig{Store: st}))
	b.SetFlush(rec.record)
	b.SetBusy(func(string) bool { return busy.Load() })

	b.Push("c_w", "subagents", "", "worker finished")
	c.True(b.Pending("c_w"), "an undurable fragment must read pending")

	clk.Advance(5 * time.Second) // fires, defers on busy -- orphan stays queued
	c.True(b.Pending("c_w"), "a deferred orphan must still read pending")

	busy.Store(false)
	b.DrainIdle("c_w")
	c.Eq(1, rec.n(), "flushes")
	c.False(b.Pending("c_w"), "Pending after the orphan flushed")
}

// Forget drops the child's scheduling state, so there is nothing left to
// report as queued.
func TestPendingClearedByForget(t *testing.T) {
	c := assert.NewAborting(t)
	b, _, _, _ := newTestBuffer(t)
	b.Push("c_dead", "subagents", "", "worker finished")
	c.True(b.Pending("c_dead"), "Pending before Forget")
	b.Forget("c_dead")
	c.False(b.Pending("c_dead"), "Pending after Forget")
}

// The callback fires on the transitions Pending can observe: once when a
// fragment is accepted, once when a flush clears the batch. It must run with
// b.mu released, so a callback that re-enters the buffer (here, Pending) does
// not deadlock — asserted under -race.
func TestPendingOnChangeFiresOnAcceptAndFlush(t *testing.T) {
	c := assert.NewAborting(t)
	var mu sync.Mutex
	var seen []string
	b, clk, _, _ := newTestBuffer(t)
	b.SetOnChange(func(childID string) {
		// Re-entering the buffer from inside the callback must not deadlock.
		_ = b.Pending(childID)
		mu.Lock()
		seen = append(seen, childID)
		mu.Unlock()
	})

	b.Push("c_w", "subagents", "", "worker finished")
	mu.Lock()
	c.Eq(1, len(seen), "callback calls after accept")
	c.False(len(seen) != 1 || seen[0] != "c_w", "callback args = %v; want [c_w]", seen)
	mu.Unlock()

	clk.Advance(5 * time.Second)
	mu.Lock()
	c.Eq(2, len(seen), "callback calls after accept and flush")
	c.False(len(seen) != 2 || seen[0] != "c_w" || seen[1] != "c_w", "callback args = %v; want [c_w c_w]", seen)
	mu.Unlock()
}

// A fire with nothing dirty clears nothing and so changes nothing: the
// callback must not be called for it.
func TestPendingOnChangeNotCalledForEmptyFire(t *testing.T) {
	c := assert.NewAborting(t)
	var calls atomic.Int32
	b, _, _, _ := newTestBuffer(t)
	b.SetOnChange(func(string) { calls.Add(1) })

	b.fire(bufKey{"c_w", "subagents"}, false)
	c.Eq(int32(0), calls.Load(), "the callback fired for an empty fire")
}
