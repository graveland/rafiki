package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// heartbeatBatches counts only the batches pushed by the heartbeat sweep,
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

// A child continuously working past the interval gets exactly one heartbeat,
// naming elapsed time and cost — not the settle-notification's message shape.
func TestHeartbeatFiresOnceThePastTheInterval(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 2.50}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	c.sweepHeartbeats(context.Background(), clk.Now())
	if len(cap.batches()) != 0 {
		t.Fatal("heartbeat fired before the interval elapsed")
	}

	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(context.Background(), clk.Now())
	clk.Advance(6 * time.Second) // let the debounced push flush

	batches := cap.batches()
	if len(batches) != 1 {
		t.Fatalf("want 1 heartbeat, got %d: %+v", len(batches), batches)
	}
	if batches[0].childID != "c_coord" {
		t.Errorf("heartbeat went to %s, not the coordinator", batches[0].childID)
	}
	if !strings.Contains(batches[0].fragments[0], "$2.50") {
		t.Errorf("heartbeat fragment must name running cost: %q", batches[0].fragments[0])
	}
}

// A second sweep before the interval elapses again must not re-push.
//
// The first sweepHeartbeats call for any (re)tracked child only lazy-seeds
// its baseline and never fires (see heartbeatState.due) — so this test forces
// a GENUINE fire first (mirroring TestHeartbeatFiresOnceThePastTheInterval's
// proven two-call shape) before checking that a sweep still inside the
// interval doesn't push again. Without the genuine fire, "does not repeat"
// would be checking nothing: both counts would trivially be zero.
func TestHeartbeatDoesNotRepeatWithinTheInterval(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 1.0}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	c.sweepHeartbeats(context.Background(), clk.Now()) // first observation: lazy-seeds, no fire
	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(context.Background(), clk.Now()) // genuine fire
	clk.Advance(6 * time.Second)
	first := heartbeatBatches(cap)
	if first != 1 {
		t.Fatalf("setup: want 1 genuine heartbeat before the repeat check, got %d", first)
	}

	clk.Advance(1 * time.Minute) // still under a second full interval
	c.sweepHeartbeats(context.Background(), clk.Now())
	clk.Advance(6 * time.Second)

	if got := heartbeatBatches(cap); got != first {
		t.Errorf("a second sweep inside the interval pushed again: %d heartbeat batches, want %d", got, first)
	}
}

// A child with no parent (top-level) has nobody to heartbeat to.
func TestHeartbeatSkipsTopLevelChildren(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 1.0}

	c.handleStatusChange("c_coord", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(context.Background(), clk.Now())
	clk.Advance(6 * time.Second)

	if got := len(cap.batches()); got != 0 {
		t.Errorf("a top-level child got a heartbeat: %d batches", got)
	}
}

// Going idle clears the working-since window, so a later working spell
// starts its own interval rather than inheriting the old one.
//
// This must exercise a REAL leak, not just an absence of firing: a naive
// version of this test that never forces a genuine fire during the first
// spell is vacuous, because heartbeatState.due's first observation of any
// (re)tracked child always lazy-seeds and returns false regardless of
// whether stopWorking actually reset anything — a completely broken (no-op)
// stopWorking would produce the identical "0 heartbeats" result. So: force a
// genuine fire in the first spell (leaving a real lastSent entry behind),
// reset via the idle/re-working transition, then advance the clock by MORE
// than the interval measured from that first spell's fire. A correct reset
// makes this the new spell's first observation (lazy-seed, no fire); a
// broken stopWorking would see the stale lastSent as "interval elapsed" and
// fire immediately — that's what distinguishes the two.
func TestHeartbeatWindowResetsOnGoingIdle(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 1.0}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	c.sweepHeartbeats(context.Background(), clk.Now()) // first observation: lazy-seeds, no fire
	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(context.Background(), clk.Now()) // genuine fire, leaves lastSent behind
	clk.Advance(6 * time.Second)
	firstSpell := heartbeatBatches(cap)
	if firstSpell != 1 {
		t.Fatalf("setup: want 1 genuine heartbeat from the first spell, got %d", firstSpell)
	}

	// The transition below is a genuine settle (working -> idle), so it also
	// fires the pre-existing settle notification (subagentEventSource) —
	// unrelated to heartbeats, and not what this test is about.
	// heartbeatBatches filters it out.
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)

	// More than the interval since the FIRST spell's fire: enough for a
	// leaked, un-reset lastSent to look overdue.
	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(context.Background(), clk.Now())
	clk.Advance(6 * time.Second)

	if got := heartbeatBatches(cap); got != firstSpell {
		t.Errorf("a leaked pre-reset heartbeat state fired on the new spell: %d heartbeat batches, want %d", got, firstSpell)
	}
}
