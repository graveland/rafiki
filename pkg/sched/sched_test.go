// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"bytes"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/clock"

	"github.com/multigres/testkit/assert"
)

// 1. At replaces by key, and the replacement must never double-fire — the
// +5s job's timer is stopped the moment the +10s job takes its place.
func TestAtReplaceResetsRatherThanDoubleFires(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32
	fn := func() { calls.Add(1) }

	s.At("k", time.Unix(0, 0).Add(5*time.Second), fn)
	s.At("k", time.Unix(0, 0).Add(10*time.Second), fn)

	clk.Advance(6 * time.Second)
	ck.Eq(0, calls.Load(), "the replaced +5s job fired even though a +10s job had taken its key")

	clk.Advance(5 * time.Second)
	ck.Eq(1, calls.Load(), "the replacement must fire exactly once, not twice")
}

// The stale path by pointer identity: a job whose timer already fired while it
// was being replaced must be refused by run, not just by the stopped timer.
func TestStaleTimerOfReplacedJobDoesNotRun(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32
	fn := func() { calls.Add(1) }

	s.At("k", time.Unix(0, 0).Add(5*time.Second), fn)
	s.mu.Lock()
	first := s.pending["k"] // capture the first job's pointer
	s.mu.Unlock()
	s.At("k", time.Unix(0, 0).Add(10*time.Second), fn)

	// Advance past both deadlines: only the replacement's timer is live.
	clk.Advance(11 * time.Second)
	ck.Eq(1, calls.Load(), "advancing past both deadlines must fire exactly once")

	// The stale path itself — the real-clock race where the old timer fires
	// while the replacement takes over. run must refuse it by identity.
	s.run(first)
	ck.Eq(1, calls.Load(), "a stale job's run must not invoke its callback")
}

// 2. Callbacks run with no scheduler lock held: a callback that re-arms
// itself must not deadlock, and fires once per re-armed period.
func TestCallbackMayReArmItself(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32
	var fn func()
	fn = func() {
		calls.Add(1)
		s.At("k", clk.Now().Add(time.Second), fn)
	}
	s.At("k", clk.Now().Add(time.Second), fn)

	clk.Advance(time.Second)
	clk.Advance(time.Second)
	clk.Advance(time.Second)
	ck.Eq(3, calls.Load(), "a self re-arming job must fire once per Advance; a deadlock here means fn ran under the lock")
}

// 3. A past deadline fires promptly and is never dropped — Advance(0) is
// enough, the negative duration was passed to the clock as-is.
func TestPastDeadlineFires(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32

	s.At("late", time.Unix(0, 0).Add(-time.Hour), func() { calls.Add(1) })
	clk.Advance(0)
	ck.Eq(1, calls.Load(), "a past-deadline job must fire on the first tick, not be dropped")
}

// 4. A panicking callback is absorbed and logged; the scheduler keeps
// working and Stop returns promptly.
func TestPanickingCallbackDoesNotKillTheRunner(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, 50*time.Millisecond)
	var survived atomic.Int32

	s.At("boom", time.Unix(0, 0).Add(time.Second), func() { panic("boom") })
	s.At("later", time.Unix(0, 0).Add(2*time.Second), func() { survived.Add(1) })

	clk.Advance(5 * time.Second) // must not panic out of Advance
	ck.Eq(1, survived.Load(), "the job after the panicking one must still fire")

	begin := time.Now()
	s.Stop()
	ck.True(time.Since(begin) < 2*time.Second, "Stop took %v after a panicking callback; want it to return promptly", time.Since(begin))
}

// 5. Cancel stops the timer, is idempotent, and unknown keys are not errors.
func TestCancelPreventsFiring(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32

	s.At("k", time.Unix(0, 0).Add(5*time.Second), func() { calls.Add(1) })
	s.Cancel("k")
	s.Cancel("k")    // idempotent
	s.Cancel("nope") // unknown keys are not errors

	clk.Advance(10 * time.Second)
	ck.Eq(0, calls.Load(), "a cancelled job must not fire")
}

// Shutdown: registration closes first, so At after Stop is dropped even for a
// past deadline that would fire on the next tick.
func TestAtAfterStopIsDropped(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32

	s.Stop()
	s.At("k", time.Unix(0, 0).Add(-time.Hour), func() { calls.Add(1) })
	clk.Advance(time.Hour)
	ck.Eq(0, calls.Load(), "At after Stop must be a no-op, even for a past deadline")
	ck.Empty(s.Pending(), "a closed scheduler must report no pending jobs")
}

func TestStopIsIdempotent(t *testing.T) {
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var calls atomic.Int32
	s.At("k", time.Unix(0, 0).Add(time.Second), func() { calls.Add(1) })

	s.Stop()
	s.Stop() // must not panic, restart timers, or un-close the scheduler

	clk.Advance(time.Hour)
	assert.NewAborting(t).Eq(0, calls.Load(), "a job whose timer the first Stop stopped must stay stopped")
}

// A hung callback is abandoned after abandonAfter, Stop still returns, and
// the abandon log names the hung job's key.
func TestStopAbandonsAHungCallbackAndLogsItsKey(t *testing.T) {
	ck := assert.NewAborting(t)
	s := New(clock.RealClock(), 50*time.Millisecond)

	started := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s.At("hung", time.Now(), func() {
		close(started)
		<-block
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the callback never started")
	}

	begin := time.Now()
	s.Stop()
	elapsed := time.Since(begin)
	ck.True(elapsed < 2*time.Second, "Stop took %v; it must abandon the hung callback (bound 50ms), not block on it", elapsed)
	ck.StrContains(logBuf.String(), "hung", "the abandon log must name the hung job's key; got %q", logBuf.String())
}

// With a generous abandonAfter, Stop waits for a running callback instead of
// abandoning it.
func TestStopWaitsForARunningCallback(t *testing.T) {
	ck := assert.NewAborting(t)
	s := New(clock.RealClock(), 5*time.Second)

	started := make(chan struct{})
	var finished atomic.Bool
	s.At("quick", time.Now(), func() {
		close(started)
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the callback never started")
	}

	s.Stop()
	ck.True(finished.Load(), "Stop returned before the running callback finished")
}

// Pending is introspection: sorted by deadline then key, reflecting
// cancellation. The behavioural tests above never consult it.
func TestPendingIsSortedAndReflectsCancel(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)

	s.At("b", time.Unix(0, 0).Add(5*time.Second), func() {})
	s.At("a", time.Unix(0, 0).Add(5*time.Second), func() {}) // same deadline: Key breaks the tie
	s.At("z", time.Unix(0, 0).Add(2*time.Second), func() {})
	s.At("gone", time.Unix(0, 0).Add(9*time.Second), func() {})
	s.Cancel("gone")

	want := []Entry{
		{Key: "z", Deadline: time.Unix(0, 0).Add(2 * time.Second)},
		{Key: "a", Deadline: time.Unix(0, 0).Add(5 * time.Second)},
		{Key: "b", Deadline: time.Unix(0, 0).Add(5 * time.Second)},
	}
	ck.EqDeep(s.Pending(), want, "Pending = %+v; want sorted by deadline then key, with the cancelled job gone", s.Pending())
}

// A nil *Scheduler is a no-op everywhere, so a test-constructed Controller{}
// with no scheduler never panics.
func TestNilSchedulerIsANoOp(t *testing.T) {
	ck := assert.NewAborting(t)
	var s *Scheduler
	s.At("k", time.Now(), func() {})
	s.Cancel("k")
	s.Stop()
	ck.Nil(s.Pending(), "Pending on a nil scheduler must be nil, got %v", s.Pending())
}
