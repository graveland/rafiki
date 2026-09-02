package main

import (
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// A child continuously working past the interval gets exactly one heartbeat,
// naming elapsed time and cost — not the settle-notification's message shape.
func TestHeartbeatFiresOnceThePastTheInterval(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 2.50}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	c.sweepHeartbeats(clk.Now())
	if len(cap.batches()) != 0 {
		t.Fatal("heartbeat fired before the interval elapsed")
	}

	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(clk.Now())
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
func TestHeartbeatDoesNotRepeatWithinTheInterval(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 1.0}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(clk.Now())
	clk.Advance(6 * time.Second)
	first := len(cap.batches())

	clk.Advance(1 * time.Minute) // still under a second full interval
	c.sweepHeartbeats(clk.Now())
	clk.Advance(6 * time.Second)

	if got := len(cap.batches()); got != first {
		t.Errorf("a second sweep inside the interval pushed again: %d batches, want %d", got, first)
	}
}

// A child with no parent (top-level) has nobody to heartbeat to.
func TestHeartbeatSkipsTopLevelChildren(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 1.0}

	c.handleStatusChange("c_coord", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(6 * time.Minute)
	c.sweepHeartbeats(clk.Now())
	clk.Advance(6 * time.Second)

	if got := len(cap.batches()); got != 0 {
		t.Errorf("a top-level child got a heartbeat: %d batches", got)
	}
}

// Going idle clears the working-since window, so a later working spell
// starts its own interval rather than inheriting the old one.
func TestHeartbeatWindowResetsOnGoingIdle(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.heartbeatInterval = 5 * time.Minute
	c.coster = fakeCoster{spend: 1.0}

	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(4 * time.Minute)
	// The transition below is a genuine settle (working -> idle), so it also
	// fires the pre-existing settle notification (subagentEventSource) —
	// unrelated to heartbeats, and not what this test is about. Count only
	// heartbeatEventSource batches below.
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	c.handleStatusChange("c_w1", protocol.StatusStreaming, protocol.StatusIdle)
	clk.Advance(4 * time.Minute) // 8 total minutes of wall clock, but only 4 in the CURRENT spell
	c.sweepHeartbeats(clk.Now())
	clk.Advance(6 * time.Second)

	var heartbeats int
	for _, b := range cap.batches() {
		if b.source == heartbeatEventSource {
			heartbeats++
		}
	}
	if heartbeats != 0 {
		t.Errorf("heartbeat fired on a fresh 4-minute working spell: %d heartbeat batches", heartbeats)
	}
}
