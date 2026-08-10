package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestRenderList_Table(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{
		{ChildID: "c_01HXABC", Name: "afk-impl", Status: "streaming", Model: "anthropic/claude-sonnet-4", StartedAt: 1716636789},
	}
	if err := renderList(&buf, children, outputTable, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"c_01HXABC", "afk-impl", "streaming", "claude-sonnet-4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderList_JSON(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{{ChildID: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputJSON, false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Pretty-printed JSON has a space after the colon.
	if !strings.Contains(out, `"childId": "c_1"`) {
		t.Fatalf("JSON output: %s", out)
	}
}

func TestColorEnabled_AlwaysFlag(t *testing.T) {
	if !colorEnabled("always", false) {
		t.Fatal("always should be true")
	}
	if colorEnabled("never", true) {
		t.Fatal("never should be false")
	}
}

func TestSortChildrenAsTree(t *testing.T) {
	mk := func(id, parent, root string) protocol.ChildSummary {
		labels := map[string]string{}
		if parent != "" {
			labels["rafiki/parent"] = parent
			labels["rafiki/root"] = root
		}
		return protocol.ChildSummary{ChildID: id, Labels: labels}
	}
	// Deliberately out of order, and a second root, to prove ordering is
	// derived rather than incidental.
	in := []protocol.ChildSummary{
		mk("c", "b", "a"),
		mk("z", "", ""),
		mk("a", "", ""),
		mk("b", "a", "a"),
	}
	rows := sortChildrenAsTree(in)

	var gotIDs []string
	depth := map[string]int{}
	for _, r := range rows {
		gotIDs = append(gotIDs, r.Child.ChildID)
		depth[r.Child.ChildID] = r.Depth
	}

	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 — every child must appear exactly once", len(rows))
	}
	// a's subtree must be contiguous and in depth order.
	want := []string{"a", "b", "c", "z"}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("order = %v; want %v", gotIDs, want)
		}
	}
	if depth["a"] != 0 || depth["b"] != 1 || depth["c"] != 2 || depth["z"] != 0 {
		t.Fatalf("depths wrong: %v", depth)
	}
}

func TestSortChildrenAsTreeOrphanedParent(t *testing.T) {
	in := []protocol.ChildSummary{
		{ChildID: "kid", Labels: map[string]string{"rafiki/parent": "gone", "rafiki/root": "gone"}},
	}
	rows := sortChildrenAsTree(in)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Depth != 0 {
		t.Fatalf("depth = %d; want 0 when the parent is absent from the set", rows[0].Depth)
	}
}

func TestSortChildrenAsTreeCycleTerminates(t *testing.T) {
	in := []protocol.ChildSummary{
		{ChildID: "x", Labels: map[string]string{"rafiki/parent": "y"}},
		{ChildID: "y", Labels: map[string]string{"rafiki/parent": "x"}},
	}
	done := make(chan int, 1)
	go func() { done <- len(sortChildrenAsTree(in)) }()
	select {
	case n := <-done:
		if n != 2 {
			t.Fatalf("got %d rows, want 2 — a cycle must not drop children", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sortChildrenAsTree did not terminate on a cyclic parent chain")
	}
}
