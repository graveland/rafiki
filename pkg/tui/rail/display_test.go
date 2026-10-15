// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"

	"github.com/multigres/testkit/assert"
)

// opState builds an operator_state event, the display twin of agent_status.
func opState(id, state, waitingOn, msg string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_OperatorState{OperatorState: &rafikiv1.OperatorState{
			DisplayState: state, WaitingOn: waitingOn, Msg: msg,
		}},
	}
}

// displaySummary is summary() with the display triple set.
func displaySummary(id, name, parent, status, state, waitingOn, msg string, latest int32) *rafikiv1.ChildSummary {
	s := summary(id, name, parent, status, latest)
	s.DisplayState = state
	s.WaitingOn = waitingOn
	s.DisplayMsg = msg
	return s
}

// Seeding a row from ListChildren carries the display triple across: the rail's
// first paint for a child the client was not streaming when it changed comes
// from the summary, not from an event it never saw.
func TestRailDisplaySeedsFromSummary(t *testing.T) {
	c := assert.NewAborting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		displaySummary("c_1", "scout", "", "idle", "waiting", "operator", "need a decision", 0),
	})
	n, ok := r.Get("c_1")
	c.Require().True(ok, "c_1 must be in the rail")
	c.Eq("waiting", n.DisplayState, "DisplayState")
	c.Eq("operator", n.WaitingOn, "WaitingOn")
	c.Eq("need a decision", n.DisplayMsg, "DisplayMsg")
}

// A re-seed refreshes the display triple the daemon is authoritative for,
// exactly as it refreshes Status.
func TestRailDisplayReseedRefreshesFields(t *testing.T) {
	c := assert.NewAborting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		displaySummary("c_1", "scout", "", "idle", "working", "", "", 0),
	})
	r.Seed([]*rafikiv1.ChildSummary{
		displaySummary("c_1", "scout", "", "idle", "failed", "", "turn failed", 0),
	})
	n, _ := r.Get("c_1")
	c.Eq("failed", n.DisplayState, "DisplayState after reseed")
	c.Eq("turn failed", n.DisplayMsg, "DisplayMsg after reseed")
}

// The operator_state fold is what moves the display triple between seeds.
func TestRailDisplayEventUpdates(t *testing.T) {
	c := assert.NewAborting(t)
	r := seeded(t)
	r.Apply(opState("c_1", "waiting", "operator", "what next?", 1))
	n, _ := r.Get("c_1")
	c.Eq("waiting", n.DisplayState, "DisplayState")
	c.Eq("operator", n.WaitingOn, "WaitingOn")
	c.Eq("what next?", n.DisplayMsg, "DisplayMsg")

	// The wait clears back to idle.
	r.Apply(opState("c_1", "idle", "", "", 2))
	n, _ = r.Get("c_1")
	c.Eq("idle", n.DisplayState, "DisplayState after clear")
	c.Eq("", n.WaitingOn, "WaitingOn after clear")
	c.Eq("", n.DisplayMsg, "DisplayMsg after clear")
}

// The same ordinal twice leaves the node unchanged AND does not double-count
// the badge: a reconnect replays the durable stream.
func TestRailDisplayEventReplayIsIdempotent(t *testing.T) {
	c := assert.NewAborting(t)
	r := seeded(t)
	ev := opState("c_1", "waiting", "operator", "what next?", 3)
	r.Apply(ev)
	n, _ := r.Get("c_1")
	c.Eq("waiting", n.DisplayState, "DisplayState")
	c.Eq(1, n.Attention, "attention after first delivery")

	r.Apply(ev)
	n, _ = r.Get("c_1")
	c.Eq("waiting", n.DisplayState, "DisplayState after replay")
	c.Eq("operator", n.WaitingOn, "WaitingOn after replay")
	c.Eq("what next?", n.DisplayMsg, "DisplayMsg after replay")
	c.Eq(1, n.Attention, "attention must not increment on a replay")
}

// Only a human-blocked wait or a failure is worth a badge; blocked_ui is
// retired as a notable status.
func TestRailDisplayNotable(t *testing.T) {
	c := assert.NewCollecting(t)
	notable := []*rafikiv1.Event{
		opState("c_1", "waiting", "operator", "q", 1),
		opState("c_1", "failed", "", "boom", 1),
	}
	for _, ev := range notable {
		c.True(rail.Notable(ev), "operator_state %v must be notable", ev.GetOperatorState())
	}
	quiet := []*rafikiv1.Event{
		opState("c_1", "working", "", "", 1),
		opState("c_1", "idle", "", "", 1),
		opState("c_1", "waiting", "schedule", "", 1),
		opState("c_1", "waiting", "job", "", 1),
		statusEvt("c_1", 1, "blocked_ui"),
	}
	for _, ev := range quiet {
		c.False(rail.Notable(ev), "event %v must not be notable", ev)
	}
}

// The rail subscribes to operator_state or it never sees a display change.
func TestRailDisplayTypesIncludesOperatorState(t *testing.T) {
	c := assert.NewCollecting(t)
	found := false
	for _, ty := range rail.Types() {
		if ty == "operator_state" {
			found = true
		}
	}
	c.True(found, "Types() = %v, want it to include operator_state", rail.Types())
}

// Only parentless rows are top-level; a worker rolls up into its coordinator
// and never emits a record of its own. Order matches display order.
func TestRailDisplayTopLevelDisplays(t *testing.T) {
	c := assert.NewAborting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		displaySummary("c_b", "bravo", "", "idle", "working", "", "", 0),
		displaySummary("c_a", "alpha", "", "idle", "waiting", "operator", "q", 0),
		displaySummary("c_a_kid", "alpha kid", "c_a", "idle", "idle", "", "", 0),
	})
	got := r.TopLevelDisplays()
	c.Require().Len(got, 2, "only the two parentless rows are top-level: %v", got)
	// Display order is siblings by name (alpha before bravo).
	c.Eq("c_a", got[0].ChildID, "first top-level in display order")
	c.Eq("c_b", got[1].ChildID, "second top-level in display order")
	c.Eq("alpha", got[0].Name, "Name")
	c.Eq("waiting", got[0].DisplayState, "DisplayState")
	c.Eq("operator", got[0].WaitingOn, "WaitingOn")
	c.Eq("q", got[0].DisplayMsg, "DisplayMsg")
	for _, d := range got {
		c.NotEq("c_a_kid", d.ChildID, "a child of c_a must not be top-level")
	}
}
