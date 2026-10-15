// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"bytes"
	"log/slog"
	"sync"
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

// The stale-timer guard pinned through the clock itself, not by calling run
// directly: within one Advance batch, an earlier callback that invalidates a
// later-due job cannot stop the clock from invoking the stale timer's fn —
// fireElapsed has already marked it due — so run's identity check is the only
// thing keeping the invalidated job from firing.
func TestCancelFromAnEarlierCallbackSilencesTheStaleTimer(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var aCalls, bCalls atomic.Int32

	s.At("a", time.Unix(0, 0).Add(time.Second), func() {
		aCalls.Add(1)
		s.Cancel("b")
	})
	s.At("b", time.Unix(0, 0).Add(2*time.Second), func() { bCalls.Add(1) })

	clk.Advance(10 * time.Second) // both deadlines in one batch; a runs first
	ck.Eq(1, aCalls.Load(), "a must fire once")
	ck.Eq(0, bCalls.Load(), "b was cancelled by a's callback, but its timer was already marked due: only the identity guard keeps it from firing")
}

// The replace flavour of the same path: re-Atting the later job's key from
// inside the earlier callback must not let the replaced job's timer fire.
func TestReplaceFromAnEarlierCallbackSilencesTheStaleTimer(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, time.Second)
	var aCalls, staleCalls, newCalls atomic.Int32

	s.At("a", time.Unix(0, 0).Add(time.Second), func() {
		aCalls.Add(1)
		s.At("b", clk.Now().Add(6*time.Second), func() { newCalls.Add(1) })
	})
	s.At("b", time.Unix(0, 0).Add(2*time.Second), func() { staleCalls.Add(1) })

	clk.Advance(10 * time.Second) // a runs first and replaces b; the stale b timer is already due
	ck.Eq(1, aCalls.Load(), "a must fire once")
	ck.Eq(0, staleCalls.Load(), "the replaced b's timer was already marked due: only the identity guard keeps it from firing")
	clk.Advance(6 * time.Second)
	ck.Eq(1, newCalls.Load(), "the replacement must fire exactly once")
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

// Stop closes registration FIRST, then drains. A periodic job that re-arms
// itself from inside its own callback while Stop is draining must have that
// At refused: the successor must never be registered, let alone fire.
//
// Two properties pin the ordering. The witness's pending entry makes the gate
// below a synchronizer — it empties exactly when Stop's registration-closing
// critical section has run, so the release lands inside the drain — and under
// a Stop whose cleanup moved after the drain the witness never clears at all,
// so the gate fails. Empty pending after Stop pins the refusal itself: the
// fire-time closed guard in run would otherwise mask a registration-order
// bug, because it refuses a closed scheduler's jobs just as well.
func TestStopDropsASuccessorRegisteredDuringTheDrain(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, 5*time.Second)

	started := make(chan struct{})
	release := make(chan struct{})
	advanceDone := make(chan struct{})
	stopDone := make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	var fn func()
	fn = func() {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		s.At("k", clk.Now().Add(time.Second), fn) // must be dropped: closed
	}

	s.At("k", time.Unix(0, 0).Add(time.Second), fn)
	// The witness's pending entry is what makes s.Pending() a usable signal:
	// it empties exactly when Stop's registration-closing critical section has
	// run, so releasing the callback deterministically lands inside the drain
	// rather than before Stop started.
	s.At("w", time.Unix(0, 0).Add(time.Hour), func() {})

	go func() { defer close(advanceDone); clk.Advance(time.Second) }() // fires k; fn hangs on release
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the callback never started")
	}

	go func() { defer close(stopDone); s.Stop() }()
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Pending()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Stop never emptied pending")
		}
		time.Sleep(time.Millisecond)
	}
	close(release) // the callback re-arms itself inside the drain

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; the drain did not complete")
	}
	select {
	case <-advanceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Advance never returned")
	}

	clk.Advance(time.Hour) // the successor, had it survived, fires here
	ck.Eq(1, calls.Load(), "the successor registered during the drain must never fire")
	// The fire-time guard alone cannot pin the ordering: run also refuses a
	// job whose scheduler is closed, so a Stop that drained before closing
	// registration would still never fire the successor — it would just leak
	// it into pending. Empty pending after Stop is what pins that the At was
	// refused at REGISTRATION time, because registration closed first.
	ck.Empty(s.Pending(), "Stop must leave no pending jobs: a successor registered during the drain means registration was still open while the drain ran")
}

// The sharp end of the same ordering: a registration that lands during the
// drain with an ALREADY-REACHED deadline must not merely be refused at fire
// time — where run's closed guard would hide the ordering bug — it must never
// run at all. The hung callback keeps the drain (and with it, in a reordered
// Stop, `closed`) parked, so if registration were still open this job would
// run the moment the clock looks at it, inside Advance(0).
func TestStopRefusesAReachedDeadlineRegisteredDuringTheDrain(t *testing.T) {
	ck := assert.NewAborting(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	s := New(clk, 5*time.Second)

	started := make(chan struct{})
	holdA := make(chan struct{})
	advanceDone := make(chan struct{})
	stopDone := make(chan struct{})
	var aCalls, bCalls atomic.Int32
	var closeHold sync.Once
	t.Cleanup(func() { closeHold.Do(func() { close(holdA) }) })

	s.At("a", time.Unix(0, 0).Add(time.Second), func() {
		aCalls.Add(1)
		close(started)
		<-holdA // keeps the drain parked for the whole window below
	})
	s.At("w", time.Unix(0, 0).Add(time.Hour), func() {}) // gate witness, as above

	go func() { defer close(advanceDone); clk.Advance(time.Second) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the callback never started")
	}

	go func() { defer close(stopDone); s.Stop() }()
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Pending()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Stop never emptied pending")
		}
		time.Sleep(time.Millisecond)
	}

	// The drain is now parked on a. In a Stop that drained before closing
	// registration, this At — an already-reached deadline, the shape of a
	// periodic job's immediate re-arm — would register and RUN below.
	s.At("b", clk.Now().Add(-time.Second), func() { bCalls.Add(1) })
	clk.Advance(0)
	ck.Eq(0, bCalls.Load(), "a job registered during the drain must never run, even with an already-reached deadline")

	closeHold.Do(func() { close(holdA) })
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; the drain did not complete")
	}
	select {
	case <-advanceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Advance never returned")
	}
	ck.Eq(1, aCalls.Load(), "a must have fired exactly once")
	ck.Empty(s.Pending(), "Stop must leave no pending jobs")
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
