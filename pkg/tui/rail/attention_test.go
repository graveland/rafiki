// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
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
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{}}}
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
	if !ok {
		t.Fatalf("no node %s", id)
	}
	return n.Attention
}

func TestBadgeCountsOnlyNotableEvents(t *testing.T) {
	r := seeded(t)
	r.Apply(turnEnd("c_1", 1))        // notable
	r.Apply(toolStart("c_1", 2))      // not notable
	r.Apply(toolStart("c_1", 3))      // not notable
	r.Apply(status("c_1", "idle", 4)) // notable
	if got := attentionOf(t, r, "c_1"); got != 2 {
		t.Fatalf("attention = %d, want 2 (turn_end + idle; the tool events are activity)", got)
	}
}

func TestStreamingAndToolRunningAreNotAttention(t *testing.T) {
	r := seeded(t)
	r.Apply(status("c_1", "streaming", 1))
	r.Apply(status("c_1", "tool_running", 2))
	if got := attentionOf(t, r, "c_1"); got != 0 {
		t.Fatalf("attention = %d, want 0 -- working is activity, not attention", got)
	}
}

func TestBlockedUIIsNotable(t *testing.T) {
	r := seeded(t)
	r.Apply(status("c_1", "blocked_ui", 1))
	if got := attentionOf(t, r, "c_1"); got != 1 {
		t.Fatalf("attention = %d, want 1 -- blocked_ui is the strongest signal there is", got)
	}
}

func TestRetryIsNotNotable(t *testing.T) {
	r := seeded(t)
	r.Apply(retryEv("c_1", 1))
	if got := attentionOf(t, r, "c_1"); got != 0 {
		t.Fatal("retry must not badge: transient-error retry exists so a recoverable stream " +
			"error is NOT a human's problem, and badging it undoes that")
	}
	if n, _ := r.Get("c_1"); !n.Retrying {
		t.Error("retry must still set the Retrying flag for the glyph")
	}
}

func TestChildSpawnedIsNotNotable(t *testing.T) {
	r := seeded(t)
	r.Apply(spawned("c_kid", "c_1", "builder", 0))
	if got := attentionOf(t, r, "c_kid"); got != 0 {
		t.Fatal("child_spawned must not badge -- it already announces itself by adding a row")
	}
}

func TestFocusedChildDoesNotAccumulate(t *testing.T) {
	r := seeded(t)
	r.SetFocus("c_1")
	r.Apply(turnEnd("c_1", 1))
	r.Apply(status("c_1", "idle", 2))
	if got := attentionOf(t, r, "c_1"); got != 0 {
		t.Fatalf("attention = %d, want 0 -- you are looking at it", got)
	}
}

func TestReconnectDoesNotDoubleCount(t *testing.T) {
	r := seeded(t)
	r.Apply(turnEnd("c_1", 1))
	r.Apply(status("c_1", "idle", 2))
	if got := attentionOf(t, r, "c_1"); got != 2 {
		t.Fatalf("attention = %d, want 2 before the reconnect", got)
	}

	// The stream drops and the client resumes. Even resuming from the WRONG
	// ordinal -- Seen rather than RailCursor, the easy confusion -- must not
	// count the replayed events twice.
	r.Apply(turnEnd("c_1", 1))
	r.Apply(status("c_1", "idle", 2))
	if got := attentionOf(t, r, "c_1"); got != 2 {
		t.Fatalf("attention = %d, want 2 -- a replayed notable event must not re-badge", got)
	}

	r.Apply(turnEnd("c_1", 3))
	if got := attentionOf(t, r, "c_1"); got != 3 {
		t.Fatalf("attention = %d, want 3 -- a genuinely new event still counts", got)
	}
}

func TestNotableWithoutAnOrdinalIsNotCounted(t *testing.T) {
	r := seeded(t)
	ev := turnEnd("c_1", 0)
	ev.Ordinal = nil // publishEvent's log-append failure path does exactly this
	r.Apply(ev)
	if got := attentionOf(t, r, "c_1"); got != 0 {
		t.Fatal("an ordinal-less notable event cannot be deduplicated across a reconnect, " +
			"so it must not be counted -- see Controller.publishEvent's best-effort append")
	}
}

func TestMarkReadClearsTheBadge(t *testing.T) {
	r := seeded(t)
	r.Apply(turnEnd("c_1", 1))
	r.Apply(turnEnd("c_1", 2))
	r.MarkRead("c_1", 2)
	if got := attentionOf(t, r, "c_1"); got != 0 {
		t.Fatalf("attention = %d, want 0 after MarkRead", got)
	}
	r.Apply(turnEnd("c_1", 3))
	if got := attentionOf(t, r, "c_1"); got != 1 {
		t.Fatalf("attention = %d, want 1 -- events after the watermark still count", got)
	}
}

func TestNextAttentionSkipsQuietChildren(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
		summary("c_c", "charlie", "", "idle", 0),
	})
	r.Apply(turnEnd("c_c", 1))
	if got := r.NextAttention(); got != "c_c" {
		t.Fatalf("NextAttention = %q, want c_c", got)
	}
	r.MarkRead("c_c", 1)
	if got := r.NextAttention(); got != "" {
		t.Fatalf("NextAttention = %q, want empty when nothing needs you", got)
	}
}

func TestNextAttentionWrapsPastTheFocusedRow(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
	})
	r.SetFocus("c_b") // last in display order
	r.Apply(turnEnd("c_a", 1))
	if got := r.NextAttention(); got != "c_a" {
		t.Fatalf("NextAttention = %q, want c_a -- the search must wrap", got)
	}
}

func TestCursorUsesRailCursorNotSeen(t *testing.T) {
	r := seeded(t)
	r.Apply(turnEnd("c_1", 7))
	c := r.Cursor()
	if c == nil {
		t.Fatal("Cursor returned nil")
	}
	if got := c.GetOrdinals()["c_1"]; got != 7 {
		t.Fatalf("cursor ordinal = %d, want 7 (RailCursor). Resuming from Seen instead is "+
			"what makes a reconnect re-deliver events the rail already counted", got)
	}
	if c.GetFloorUnixMs() == 0 {
		t.Error("the cursor needs a floor: without it a child that spawned AND exited " +
			"entirely inside a disconnect is indistinguishable from a brand new one")
	}
}

func TestTypesIsTheSixSmallTypes(t *testing.T) {
	got := rail.Types()
	want := map[string]bool{
		"turn_end": true, "agent_status": true, "error": true,
		"retry": true, "child_spawned": true, "child_exited": true,
	}
	if len(got) != len(want) {
		t.Fatalf("Types() = %v, want the six in %v", got, want)
	}
	for _, ty := range got {
		if !want[ty] {
			t.Errorf("Types() contains %q, which is not one of the six", ty)
		}
		if ty == "assistant_message" || ty == "user_message" {
			t.Error("the rail must not carry message content to a pane a few glyphs wide")
		}
		if ty == "content_block_delta" {
			t.Error("content_block_delta is ephemeral and excluded by tier, not by filter")
		}
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
		if !consumed[ty] {
			t.Errorf("rail asks for %q but nothing in the rail consumes it", ty)
		}
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
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
		summary("c_c", "charlie", "", "idle", 0),
	})
	r.SetFocus("c_b")
	r.Apply(turnEnd("c_a", 1))
	r.Apply(turnEnd("c_c", 1))

	if got := r.PrevAttention(); got != "c_a" {
		t.Errorf("PrevAttention = %q, want c_a", got)
	}
	if got := r.NextAttention(); got != "c_c" {
		t.Errorf("NextAttention = %q, want c_c", got)
	}
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
	if got := r.PrevAttention(); got != "c_b" {
		t.Errorf("PrevAttention = %q, want c_b -- the search must wrap", got)
	}
}

// TestPrevAttentionEmptyWhenNothingNeedsYou.
func TestPrevAttentionEmptyWhenNothingNeedsYou(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("c_a", "alpha", "", "idle", 0),
		summary("c_b", "bravo", "", "idle", 0),
	})
	if got := r.PrevAttention(); got != "" {
		t.Errorf("PrevAttention = %q, want empty", got)
	}
}
