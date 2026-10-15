package main

import (
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sched"

	"github.com/multigres/testkit/assert"
)

// heartbeatFixture builds the settleFixture with a fake-clock scheduler wired
// in, so heartbeat deadlines can be driven deterministically. The four-minute
// interval matches defaultHeartbeatInterval.
func heartbeatFixture(t *testing.T) (*Controller, *clock.FakeClock, *capturedFlush) {
	t.Helper()
	c, clk, cap := settleFixture(t)
	c.clk = clk
	c.sched = sched.New(clk, time.Second)
	c.heartbeatInterval = 4 * time.Minute
	return c, clk, cap
}

// heartbeatBatches counts only the batches pushed by heartbeat check-ins,
// filtering out unrelated traffic on the same evbuf (chiefly the pre-existing
// settle notification, which a working->idle transition fires regardless of
// anything heartbeat-related).
func heartbeatBatches(cap *capturedFlush) int {
	var n int
	for _, b := range cap.batches() {
		if b.source == heartbeatEventSource {
			n++
		}
	}
	return n
}

// The first check-in of a working spell lands at EXACTLY the interval after
// the spell started. Fails if arming ever falls back to the old lazy seed,
// which pushed the first check-in out by a whole extra interval.
func TestHeartbeatFirstCheckInLandsAtExactlyTheInterval(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)

	// Just short of the interval: nothing has fired, and no debounce can flush
	// what was never pushed.
	clk.Advance(4*time.Minute - time.Second)
	ck.Require().Empty(cap.batches(), "a check-in fired before the interval elapsed")

	// Cross the deadline, then let the 5s debounce flush.
	clk.Advance(time.Second)
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Require().Len(batches, 1, "want exactly 1 check-in at the interval, got %d", len(batches))
	ck.Eq("c_coord", batches[0].childID, "check-in went to")
	ck.StrContains(batches[0].fragments[0], "c_w1", "check-in must name the working child")
}

// A continuously working child gets one check-in per interval.
func TestHeartbeatRepeatsEachInterval(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)

	clk.Advance(4 * time.Minute)
	clk.Advance(6 * time.Second)
	ck.Require().Eq(1, heartbeatBatches(cap), "setup: want 1 check-in after the first interval, got")

	clk.Advance(4 * time.Minute)
	clk.Advance(6 * time.Second)
	ck.Eq(2, heartbeatBatches(cap), "want a second check-in one interval later")
}

// Going idle cancels the pending check-in deadline AND the fire-time status
// re-check refuses to push. Both halves are asserted: the batch count proves
// the re-check, and Pending emptiness proves the cancellation.
func TestHeartbeatStopsWhenChildGoesIdle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(1 * time.Minute)
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)

	ck.Require().Empty(c.sched.Pending(), "the idle transition must cancel the child's deadline; pending=%v", c.sched.Pending())

	clk.Advance(10 * time.Minute)
	ck.Eq(0, heartbeatBatches(cap), "an idle child must not get a check-in")
}

// A child with no parent (top-level) has nobody to heartbeat to, so no
// deadline is ever armed under it.
func TestHeartbeatSkipsTopLevelChildren(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)

	c.handleStatusChange("c_coord", protocol.StatusStreaming, protocol.StatusIdle)

	ck.Require().Empty(c.sched.Pending(), "a top-level child must never arm a check-in deadline")
	clk.Advance(10 * time.Minute)
	ck.Eq(0, heartbeatBatches(cap), "a top-level child got a check-in")
}

// A child that vanishes before its deadline fires must not re-arm: the timer
// is one-shot, and a subject that is gone must not reschedule itself.
func TestHeartbeatVanishedChildDoesNotRearm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	c.st.Delete("c_w1")

	clk.Advance(10 * time.Minute)
	ck.Eq(0, heartbeatBatches(cap), "a vanished child got a check-in")
	ck.Require().Empty(c.sched.Pending(), "a vanished child's deadline must not re-arm")
}

// With no coster configured, the fragment must not assert a false "$0.00" —
// it omits the cost clause and still names elapsed time.
func TestHeartbeatOmitsCostClauseWithNoCoster(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)
	c.coster = nil

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(4 * time.Minute)
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Require().Len(batches, 1, "want 1 check-in, got %d", len(batches))
	frag := batches[0].fragments[0]
	ck.NotStrContains(frag, "$", "check-in asserted a cost figure with no coster configured")
	ck.StrContains(frag, "so far", "check-in must still name elapsed time")
}

// A coster that errors degrades the same way as a missing one: the cost clause
// is omitted, not asserted as $0.00.
func TestHeartbeatOmitsCostClauseOnQueryError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)
	c.coster = fakeCoster{err: errors.New("db unreachable")}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(4 * time.Minute)
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Require().Len(batches, 1, "want 1 check-in, got %d", len(batches))
	frag := batches[0].fragments[0]
	ck.NotStrContains(frag, "$0.00", "check-in asserted $0.00 on a failed cost query")
	ck.StrContains(frag, "so far", "check-in must still fire and name elapsed time")
}

// A working→working transition (streaming -> tool_running) is not a new spell:
// it must not reset the deadline, so the check-in still lands one interval
// after the spell STARTED, not after the transition.
func TestHeartbeatWorkingToWorkingDoesNotReset(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := heartbeatFixture(t)

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(3 * time.Minute)
	c.handleStatusChange("c_w1", protocol.StatusToolRunning, protocol.StatusStreaming)

	clk.Advance(1 * time.Minute) // reaches the ORIGINAL 4m deadline
	clk.Advance(6 * time.Second)

	ck.Eq(1, heartbeatBatches(cap), "working->working must not move the deadline; want 1 check-in at 4m, got")
}
