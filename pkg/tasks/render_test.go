package tasks

import (
	"fmt"
	"slices"
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

func TestAssignHandlesSortsNumericallyNotLexically(t *testing.T) {
	all := make([]Task, 0, 11)
	for i := 1; i <= 11; i++ {
		all = append(all, Task{ID: fmt.Sprintf("t%d", i), Ordinal: i})
	}
	got := AssignHandles(all)
	want := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}
	for i, w := range want {
		if got[i].Handle != w {
			t.Fatalf("position %d: handle = %q, want %q (full order: %v)", i, got[i].Handle, w, handlesOf(got))
		}
	}
}

func TestAssignHandlesOrdersChildrenUnderTheirParent(t *testing.T) {
	all := []Task{
		{ID: "a", Ordinal: 2},
		{ID: "b", Ordinal: 10},
		{ID: "c", Ordinal: 1, ParentID: "a"},
	}
	got := AssignHandles(all)
	want := []string{"2", "2.1", "10"}
	if h := handlesOf(got); !slices.Equal(h, want) {
		t.Fatalf("order = %v, want %v", h, want)
	}
}

func TestFilterTasksKeepsHandlesFromTheFullSet(t *testing.T) {
	// Task 1 is pending; its child 1.1 is in_progress. Filtering to
	// in_progress must NOT renumber 1.1 to "1" — a model that then calls
	// task_update handle=1 would silently address the parent.
	all := AssignHandles([]Task{
		{ID: "p", Ordinal: 1, Status: StatusPending},
		{ID: "c", Ordinal: 1, ParentID: "p", Status: StatusInProgress},
	})
	got := FilterTasks(all, ListFilter{Status: StatusInProgress})
	if len(got) != 1 {
		t.Fatalf("filtered to %d tasks, want 1", len(got))
	}
	if got[0].Handle != "1.1" {
		t.Fatalf("handle = %q, want %q — filtering must not renumber", got[0].Handle, "1.1")
	}
}

func TestFilterTasksAppliesLimitLast(t *testing.T) {
	all := AssignHandles([]Task{
		{ID: "a", Ordinal: 1, Status: StatusPending},
		{ID: "b", Ordinal: 2, Status: StatusPending},
		{ID: "c", Ordinal: 3, Status: StatusPending},
	})
	got := FilterTasks(all, ListFilter{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit 2 returned %d tasks", len(got))
	}
	if got[0].Handle != "1" || got[1].Handle != "2" {
		t.Fatalf("limit must keep the first rows in order, got %v", handlesOf(got))
	}
}

func handlesOf(ts []Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Handle
	}
	return out
}
