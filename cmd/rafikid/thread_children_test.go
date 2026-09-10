package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/child"
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

// TestHandleSubagentObservationNilStoreIsInert pins the only guard standing
// between the async supervisor hook and a nil-captureStore panic: a Controller
// built without a pool has no capture store, and the observation must be
// dropped silently rather than dereference nil in the request path.
func TestHandleSubagentObservationNilStoreIsInert(t *testing.T) {
	c := newTestController(t)
	if c.captureStore != nil {
		t.Fatalf("newTestController built a capture store without a pool; the nil path is not exercised")
	}
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	// Must return, not panic, and must not touch the store: there is no store
	// to touch, and the synthetic child must stay unnamed.
	c.HandleSubagentObservation("c_parent", child.SubagentObservation{
		MessageID: "msg_whatever", ParentToolUseID: "toolu_XYZ",
	})
	if _, ok := c.st.Get(threadChildID("c_parent", "thread-a")); ok {
		t.Error("a nil capture store must never create or rename a synthetic child")
	}
}

// TestHandleSubagentObservationJoinsTheThreadByMessageID pins the join end to
// end against the real capture store: a turn whose response_message_id is the
// frame's message id must resolve to the synthetic child the proxy created for
// that thread, which is only true because the daemon stamps
// X-Rafiki-Session: childID and ThreadOfPredecessorInSession scopes to that
// session family. Needs a database (skips without RAFIKI_TEST_DSN).
func TestHandleSubagentObservationJoinsTheThreadByMessageID(t *testing.T) {
	pool := openTestPool(t)
	cs := capture.NewCaptureStore(pool)

	convID, err := cs.EnsureConversationByExternalRef(context.Background(), capture.ConversationRef{
		OriginEntrypoint: "test", DrivenBy: "client", ExternalRef: "c_joinparent",
	})
	if err != nil {
		t.Fatalf("EnsureConversationByExternalRef: %v", err)
	}
	turnID, createdAt, err := cs.InsertTurnIntent(context.Background(), capture.TurnIntent{
		ConversationID: convID, Model: "m", Source: "rafiki-claude", AuthorKind: "agent",
	})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	// The founding turn of a subagent thread: thread_id = its own id.
	if err := cs.RecordThread(context.Background(), "c_joinparent", convID, turnID, createdAt, "", "msg_join_1", true); err != nil {
		t.Fatalf("RecordThread: %v", err)
	}

	c := newTestController(t)
	c.captureStore = cs
	c.st.Insert(&childstore.Session{
		ChildID: "c_joinparent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	if err := c.EnsureThreadChild("c_joinparent", turnID, convID); err != nil {
		t.Fatalf("EnsureThreadChild: %v", err)
	}

	c.HandleSubagentObservation("c_joinparent", child.SubagentObservation{
		MessageID: "msg_join_1", ParentToolUseID: "toolu_JOIN",
	})
	snap, ok := c.st.Get(threadChildID("c_joinparent", turnID))
	if !ok {
		t.Fatalf("no synthetic child for the joined thread %q", turnID)
	}
	if snap.Labels[labelSpawnedByTool] != "toolu_JOIN" {
		t.Errorf("label = %q, want toolu_JOIN", snap.Labels[labelSpawnedByTool])
	}
	if snap.Name != "task:toolu_JOIN" {
		t.Errorf("name = %q, want task:toolu_JOIN", snap.Name)
	}

	// A message id the store never saw resolves to nothing: the observation is
	// dropped silently and the child keeps its recorded identity.
	c.HandleSubagentObservation("c_joinparent", child.SubagentObservation{
		MessageID: "msg_never_seen", ParentToolUseID: "toolu_OTHER",
	})
	if snap2, _ := c.st.Get(threadChildID("c_joinparent", turnID)); snap2.Name != "task:toolu_JOIN" {
		t.Errorf("name changed on an unresolvable observation: %q", snap2.Name)
	}
}
