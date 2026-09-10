package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestSyntheticChildIsParentedAndDoesNotConsumeTheChildBudget(t *testing.T) {
	c := newTestController(t)
	// childstore.Store inserts a *Session, not a Snapshot: see the pattern at
	// cmd/rafikid/agent_runtime_test.go:715.
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
		MaxChildren: 2,
	})

	if err := c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"); err != nil {
		t.Fatalf("EnsureThreadChild: %v", err)
	}
	id := threadChildID("c_parent", "thread-a")
	snap, ok := c.st.Get(id)
	if !ok {
		t.Fatalf("no synthetic child for thread-a (looked for %q)", id)
	}
	if snap.Labels[childstore.LabelParent] != "c_parent" {
		t.Errorf("parent label = %q, want %q", snap.Labels[childstore.LabelParent], "c_parent")
	}
	if snap.Labels[labelNativeSubagent] != "1" {
		t.Errorf("missing the %s label; the rail must be able to tell a native subagent from a rafiki agent", labelNativeSubagent)
	}
	if snap.PID != 0 {
		t.Errorf("PID = %d, want 0: a native subagent has no process", snap.PID)
	}

	// A native subagent is not a budgeted cross-process agent.
	if got := c.st.LiveDescendantCount("c_parent"); got != 0 {
		t.Errorf("LiveDescendantCount = %d, want 0: a synthetic child must not consume MaxChildren", got)
	}

	// Idempotent: the proxy calls this on every turn of the thread.
	if err := c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"); err != nil {
		t.Fatalf("second EnsureThreadChild: %v", err)
	}
	// Descendants KEEPS the synthetic child (agent_list and the budget member
	// lists must see it); only the MaxChildren gate above excludes it.
	if n := len(c.st.Descendants("c_parent")); n != 1 {
		t.Errorf("Descendants = %d, want 1: the synthetic child is a real member of the tree", n)
	}
}

func TestNoteSubagentNamesTheChildAfterItsToolCall(t *testing.T) {
	c := newTestController(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	if err := c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"); err != nil {
		t.Fatalf("EnsureThreadChild: %v", err)
	}
	id := threadChildID("c_parent", "thread-a")

	// The join: message msg_sub1 belongs to thread-a, and the frame carrying it
	// named toolu_ABC as its spawning call.
	c.noteSubagentToolCall("c_parent", "thread-a", "toolu_ABC")

	snap, ok := c.st.Get(id)
	if !ok {
		t.Fatalf("no synthetic child %q", id)
	}
	if got := snap.Labels[labelSpawnedByTool]; got != "toolu_ABC" {
		t.Errorf("%s = %q, want %q", labelSpawnedByTool, got, "toolu_ABC")
	}
	if snap.Name != "task:toolu_ABC" {
		t.Errorf("name = %q, want %q: an opaque thread uuid is not findable in the parent's transcript",
			snap.Name, "task:toolu_ABC")
	}

	// Idempotent: the hook fires on every frame of the subagent's output.
	c.noteSubagentToolCall("c_parent", "thread-a", "toolu_ABC")
	if snap2, _ := c.st.Get(id); snap2.Name != "task:toolu_ABC" {
		t.Errorf("name changed on a repeat call: %q", snap2.Name)
	}
}
