package tasks

import (
	"strings"
	"testing"
)

func TestAssignHandles(t *testing.T) {
	in := []Task{
		{ID: "1", ParentID: "", Ordinal: 1, Content: "first"},
		{ID: "2", ParentID: "", Ordinal: 2, Content: "second"},
		{ID: "3", ParentID: "2", Ordinal: 1, Content: "second-a"},
		{ID: "4", ParentID: "3", Ordinal: 7, Content: "deep"},
	}
	got := map[string]string{}
	for _, task := range AssignHandles(in) {
		got[task.ID] = task.Handle
	}
	want := map[string]string{"1": "1", "2": "2", "3": "2.1", "4": "2.1.7"}
	for id, h := range want {
		if got[id] != h {
			t.Errorf("task %s handle = %q; want %q", id, got[id], h)
		}
	}
}

// Ordinals are monotonic, not positional: a gap left by a dropped sibling
// must be visible in the handle, never closed up.
func TestAssignHandlesPreservesGaps(t *testing.T) {
	in := []Task{
		{ID: "a", Ordinal: 1, Content: "kept"},
		{ID: "c", Ordinal: 3, Content: "after a drop"},
	}
	got := AssignHandles(in)
	if got[1].Handle != "3" {
		t.Fatalf("handle = %q; want 3 — a gap from a dropped sibling must not be renumbered", got[1].Handle)
	}
}

func TestRenderHidesDropped(t *testing.T) {
	in := []Task{
		{ID: "a", Ordinal: 1, Content: "live", Status: StatusPending},
		{ID: "b", Ordinal: 2, Content: "gone", Status: StatusDropped, DropReason: "unnecessary"},
	}
	out := Render(AssignHandles(in), false)
	if strings.Contains(out, "gone") {
		t.Error("dropped row must be hidden when includeDropped is false")
	}
	out = Render(AssignHandles(in), true)
	if !strings.Contains(out, "gone") || !strings.Contains(out, "unnecessary") {
		t.Error("includeDropped must show the row AND its reason")
	}
}
