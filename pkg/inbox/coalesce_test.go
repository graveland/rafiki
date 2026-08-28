// SPDX-License-Identifier: Apache-2.0

package inbox_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/inbox"
)

func row(id, source, key, text string, mode inbox.Mode) inbox.Inbound {
	return inbox.Inbound{ID: id, ChildID: "c_1", Mode: mode, Source: source, Key: key, Text: text}
}

func TestCoalesceDirectMessagesAreOneBatchEach(t *testing.T) {
	got := inbox.Coalesce([]inbox.Inbound{
		row("m1", "", "", "first", inbox.ModePrompt),
		row("m2", "", "", "second", inbox.ModePrompt),
	}, inbox.BatchConfig{})

	if len(got) != 2 {
		t.Fatalf("want 2 batches, got %d: %+v", len(got), got)
	}
	if got[0].Frags[0] != "first" || got[1].Frags[0] != "second" {
		t.Errorf("direct messages must keep arrival order, got %+v", got)
	}
	if len(got[0].IDs) != 1 || got[0].IDs[0] != "m1" {
		t.Errorf("batch must carry its own row id, got %+v", got[0].IDs)
	}
}

func TestCoalesceFragmentsLastWriteWinsPerKey(t *testing.T) {
	got := inbox.Coalesce([]inbox.Inbound{
		row("f1", "subagents", "c_a", "agent c_a working", inbox.ModePrompt),
		row("f2", "subagents", "c_b", "agent c_b working", inbox.ModePrompt),
		row("f3", "subagents", "c_a", "agent c_a settled", inbox.ModePrompt),
	}, inbox.BatchConfig{})

	if len(got) != 1 {
		t.Fatalf("one source is one batch, got %d", len(got))
	}
	want := []string{"agent c_a settled", "agent c_b working"}
	if strings.Join(got[0].Frags, "|") != strings.Join(want, "|") {
		t.Errorf("frags = %v, want %v (keyed order is FIRST appearance, text is LAST write)", got[0].Frags, want)
	}
	// f1 was superseded but is still accounted for: a row left pending
	// because its text lost a last-write-wins race is redelivered forever.
	if strings.Join(got[0].IDs, ",") != "f1,f2,f3" {
		t.Errorf("IDs = %v, want every row in the group including superseded ones", got[0].IDs)
	}
}

func TestCoalesceAnySteerMakesTheBatchASteer(t *testing.T) {
	got := inbox.Coalesce([]inbox.Inbound{
		row("f1", "executor", "", "note", inbox.ModePrompt),
		row("f2", "executor", "", "EXECUTOR LOST", inbox.ModeSteer),
	}, inbox.BatchConfig{})

	if len(got) != 1 || got[0].Mode != inbox.ModeSteer {
		t.Fatalf("a group containing a steer delivers as a steer, got %+v", got)
	}
}

func TestCoalesceAppliesCapsWithAVisibleMarker(t *testing.T) {
	var rows []inbox.Inbound
	for i := range 5 {
		rows = append(rows, row(string(rune('a'+i)), "s", "", strings.Repeat("x", 10), inbox.ModePrompt))
	}
	got := inbox.Coalesce(rows, inbox.BatchConfig{MaxFragments: 3})
	if len(got) != 1 {
		t.Fatalf("want 1 batch, got %d", len(got))
	}
	last := got[0].Frags[len(got[0].Frags)-1]
	if !strings.Contains(last, "omitted") {
		t.Errorf("capped batch must end with an omission marker, got %q", last)
	}
	if len(got[0].IDs) != 5 {
		t.Errorf("omitted rows are still acked, IDs = %v", got[0].IDs)
	}
}

func TestCoalesceAbortIsItsOwnBatchWithNoBody(t *testing.T) {
	got := inbox.Coalesce([]inbox.Inbound{row("m1", "", "", "", inbox.ModeAbort)}, inbox.BatchConfig{})
	if len(got) != 1 || got[0].Mode != inbox.ModeAbort || len(got[0].Frags) != 0 {
		t.Fatalf("abort batch = %+v, want one empty-bodied abort batch", got)
	}
}
