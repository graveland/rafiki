// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"

	"github.com/multigres/testkit/assert"
)

func status(id, state string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}}}
}

func turnEnd(id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}}
}

func toolStart(id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_ToolExecutionStart{
			ToolExecutionStart: &rafikiv1.ToolExecutionStart{ToolUseId: "tu", Name: "bash"}}}
}

func retryEv(id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{WillRetry: true}}}
}

// retryResolved is the will_retry=false half: a retry that resolved (fired,
// cleared by a success, abandoned) without any status transition to do it.
func retryResolved(id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Ordinal: &ord,
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{WillRetry: false}}}
}

func seeded(t *testing.T) *rail.Rail {
	t.Helper()
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{summary("c_1", "scout", "", "idle", 0)})
	return r
}

func attentionOf(t *testing.T, r *rail.Rail, id string) int {
	t.Helper()
	n, ok := r.Get(id)
	assert.NewAborting(t).True(ok, "no node %s", id)
	return n.Attention
}

func TestBadgeCountsOnlyNotableEvents(t *testing.T) {
	r := seeded(t)
	r.Apply(turnEnd("c_1", 1))        // notable
	r.Apply(toolStart("c_1", 2))      // not notable
	r.Apply(toolStart("c_1", 3))      // not notable
	r.Apply(status("c_1", "idle", 4)) // notable
	assert.NewAborting(t).Eq(2, attentionOf(t, r, "c_1"), "attention")
}

func TestStreamingAndToolRunningAreNotAttention(t *testing.T) {
	r := seeded(t)
	r.Apply(status("c_1", "streaming", 1))
	r.Apply(status("c_1", "tool_running", 2))
	assert.NewAborting(t).Eq(0, attentionOf(t, r, "c_1"), "attention")
}

func TestBlockedUIIsNotable(t *testing.T) {
	r := seeded(t)
	r.Apply(status("c_1", "blocked_ui", 1))
	assert.NewAborting(t).Eq(1, attentionOf(t, r, "c_1"), "attention")
}

func TestRetryIsNotNotable(t *testing.T) {
	c := assert.NewCollecting(t)
	r := seeded(t)
	r.Apply(retryEv("c_1", 1))
	c.Require().Eq(0, attentionOf(t, r, "c_1"), "retry must not badge: transient-error retry exists so a recoverable stream "+
		"error is NOT a human's problem, and badging it undoes that")
	n, _ := r.Get("c_1")
	c.True(n.Retrying, "retry must still set the Retrying flag for the glyph")
}

// A retry's resolution half (will_retry=false — the daemon's auto-resume
// fired, a success cleared it, or the attempts ran out) must CLEAR the flag:
// a retry that ends without a status transition would otherwise spin its ⟳
// forever.
func TestRetryResolutionClearsTheGlyph(t *testing.T) {
	r := seeded(t)
	r.Apply(retryEv("c_1", 1))
	r.Apply(retryResolved("c_1", 2))
	n, _ := r.Get("c_1")
	assert.NewCollecting(t).False(n.Retrying, "will_retry=false left the Retrying flag set")
}

func TestChildSpawnedIsNotNotable(t *testing.T) {
	r := seeded(t)
	r.Apply(spawned("c_kid", "c_1", "builder", 0))
	assert.NewAborting(t).Eq(0, attentionOf(t, r, "c_kid"), "child_spawned must not badge -- it already announces itself by adding a row")
}

func TestFocusedChildDoesNotAccumulate(t *testing.T) {
	r := seeded(t)
	r.SetFocus("c_1")
	r.Apply(turnEnd("c_1", 1))
	r.Apply(status("c_1", "idle", 2))
	assert.NewAborting(t).Eq(0, attentionOf(t, r, "c_1"), "attention")
}

func TestReconnectDoesNotDoubleCount(t *testing.T) {
	c := assert.NewAborting(t)
	r := seeded(t)
	r.Apply(turnEnd("c_1", 1))
	r.Apply(status("c_1", "idle", 2))
	c.Eq(2, attentionOf(t, r, "c_1"), "attention")

	// The stream drops and the client resumes. Even resuming from the WRONG
	// ordinal -- Seen rather than RailCursor, the easy confusion -- must not
	// count the replayed events twice.
	r.Apply(turnEnd("c_1", 1))
	r.Apply(status("c_1", "idle", 2))
	c.Eq(2, attentionOf(t, r, "c_1"), "attention")

	r.Apply(turnEnd("c_1", 3))
	c.Eq(3, attentionOf(t, r, "c_1"), "attention")
}

func TestNotableWithoutAnOrdinalIsNotCounted(t *testing.T) {
	r := seeded(t)
	ev := turnEnd("c_1", 0)
	ev.Ordinal = nil // publishEvent's log-append failure path does exactly this
	r.Apply(ev)
	assert.NewAborting(t).Eq(0, attentionOf(t, r, "c_1"), "an ordinal-less notable event cannot be deduplicated across a reconnect, "+
		"so it must not be counted -- see Controller.publishEvent's best-effort append")
}

func TestMarkReadClearsTheBadge(t *testing.T) {
	c := assert.NewAborting(t)
	r := seeded(t)
	r.Apply(turnEnd("c_1", 1))
	r.Apply(turnEnd("c_1", 2))
	r.MarkRead("c_1", 2)
	c.Eq(0, attentionOf(t, r, "c_1"), "attention")
	r.Apply(turnEnd("c_1", 3))
	c.Eq(1, attentionOf(t, r, "c_1"), "attention")
}

func TestNextAttentionSkipsQuietChildren(t *testing.T) {
	c := assert.NewAborting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
		summary("c_c", "charlie", "", "idle", 0),
	})
	r.Apply(turnEnd("c_c", 1))
	c.Eq("c_c", r.NextAttention(), "NextAttention")
	r.MarkRead("c_c", 1)
	c.Eq("", r.NextAttention(), "NextAttention")
}

func TestNextAttentionWrapsPastTheFocusedRow(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
	})
	r.SetFocus("c_b") // last in display order
	r.Apply(turnEnd("c_a", 1))
	assert.NewAborting(t).Eq("c_a", r.NextAttention(), "NextAttention")
}

func TestCursorUsesRailCursorNotSeen(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := seeded(t)
	r.Apply(turnEnd("c_1", 7))
	c := r.Cursor()
	ck.Require().NotNil(c, "Cursor returned nil")
	got := c.GetOrdinals()["c_1"]
	ck.Require().Eq(7, got, "cursor ordinal = %d, want 7 (RailCursor). Resuming from Seen instead is "+
		"what makes a reconnect re-deliver events the rail already counted", got)
	ck.NotNil(c.GetFloor(), "the cursor needs a floor: without it a child that spawned AND exited "+
		"entirely inside a disconnect is indistinguishable from a brand new one")
}

func TestTypesIsTheSixSmallTypes(t *testing.T) {
	c := assert.NewCollecting(t)
	got := rail.Types()
	want := map[string]bool{
		"turn_end": true, "agent_status": true, "error": true,
		"retry": true, "child_spawned": true, "child_exited": true,
	}
	c.Require().Len(got, len(want), "Types() = %v, want the six in %v", got, want)
	for _, ty := range got {
		c.False(!want[ty], "Types() contains %q, which is not one of the six", ty)
		c.False(ty == "assistant_message" || ty == "user_message", "the rail must not carry message content to a pane a few glyphs wide")
		c.NotEq("content_block_delta", ty, "content_block_delta is ephemeral and excluded by tier, not by filter")
	}
}

// Every type the rail asks for must be one Apply or Notable actually consumes,
// or the filter is paying for bytes nobody reads.
func TestEveryRequestedTypeIsConsumed(t *testing.T) {
	consumed := map[string]bool{
		"turn_end":      true, // Notable
		"agent_status":  true, // Node.Status + Notable
		"error":         true, // Notable
		"retry":         true, // Node.Retrying
		"child_spawned": true, // introduces a row
		"child_exited":  true, // Node.Exited + Notable
	}
	for _, ty := range rail.Types() {
		assert.NewCollecting(t).False(!consumed[ty], "rail asks for %q but nothing in the rail consumes it", ty)
	}
}

// Cursor is handed to the rail stream as a callback and runs on that
// goroutine while Apply runs on bubbletea's. Guarding it is what makes the
// cockpit's two goroutines safe; -race is the only thing that proves it.
func TestConcurrentApplyAndCursorAreSafe(t *testing.T) {
	r := seeded(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int32(1); i < 500; i++ {
			r.Apply(turnEnd("c_1", i))
		}
	}()
	for i := 0; i < 500; i++ {
		_ = r.Cursor()
		_ = r.Nodes()
		_ = r.NextAttention()
	}
	<-done
}

// TestPrevAttentionScansBackwards pairs with the NextAttention tests. With
// attention on both sides of the focused row, next and prev must disagree —
// otherwise alt+p is just alt+n with a different name.
func TestPrevAttentionScansBackwards(t *testing.T) {
	c := assert.NewCollecting(t)
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
		summary("c_c", "charlie", "", "idle", 0),
	})
	r.SetFocus("c_b")
	r.Apply(turnEnd("c_a", 1))
	r.Apply(turnEnd("c_c", 1))

	c.Eq("c_a", r.PrevAttention(), "PrevAttention")
	c.Eq("c_c", r.NextAttention(), "NextAttention")
}

// TestPrevAttentionWrapsPastTheFocusedRow mirrors
// TestNextAttentionWrapsPastTheFocusedRow.
func TestPrevAttentionWrapsPastTheFocusedRow(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
	})
	r.SetFocus("c_a") // first in display order
	r.Apply(turnEnd("c_b", 1))
	assert.NewCollecting(t).Eq("c_b", r.PrevAttention(), "PrevAttention")
}

// TestPrevAttentionEmptyWhenNothingNeedsYou.
func TestPrevAttentionEmptyWhenNothingNeedsYou(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
	})
	assert.NewCollecting(t).Eq("", r.PrevAttention(), "PrevAttention")
}
