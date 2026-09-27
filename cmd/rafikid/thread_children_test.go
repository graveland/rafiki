package main

import (
	"context"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// EnsureThreadChild must publish child_spawned exactly like a real Spawn does
// (see TestSpawnPublishesNativeChildSpawned): the TUI rail only ever learns of
// a new row from the one-time Init seed or a reactive reseed triggered by an
// event for an unknown child id. A silent creation is invisible in the rail
// for the rest of the session unless something else happens to force a
// reseed -- `rafiki list`/`logs`/`tail` don't share the bug because they
// query ListChildren fresh every time.
func TestEnsureThreadChildPublishesNativeChildSpawned(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})

	ck.Require().NoError(c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"), "EnsureThreadChild")
	id := threadChildID("c_parent", "thread-a")

	recs, err := c.evlog.Read(context.Background(), id, -1, 0)
	ck.Require().NoError(err, "Read")
	var found *rafikiv1.ChildSpawned
	for _, r := range recs {
		if r.Type == "child_spawned" {
			var ev rafikiv1.Event
			ck.Require().NoError(protojson.Unmarshal(r.Payload, &ev), "unmarshal")
			found = ev.GetChildSpawned()
		}
	}
	ck.Require().NotNil(found, "no child_spawned event in the log")
	ck.Eq("c_parent", found.GetParentId(), "parent_id")
	ck.Eq(id, found.GetChildId(), "child_id")
}

func TestSyntheticChildIsParentedAndDoesNotConsumeTheChildBudget(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	// childstore.Store inserts a *Session, not a Snapshot: see the pattern at
	// cmd/rafikid/agent_runtime_test.go:715.
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
		MaxChildren: 2,
	})

	ck.Require().NoError(c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"), "EnsureThreadChild")
	id := threadChildID("c_parent", "thread-a")
	snap, ok := c.st.Get(id)
	ck.Require().True(ok, "no synthetic child for thread-a (looked for %q)", id)
	ck.Eq("c_parent", snap.Labels[childstore.LabelParent], "parent label")
	ck.Eq("1", snap.Labels[labelNativeSubagent], "missing the %s label; the rail must be able to tell a native subagent from a rafiki agent", labelNativeSubagent)
	ck.Eq(0, snap.PID, "PID")

	// A native subagent is not a budgeted cross-process agent.
	ck.Eq(0, c.st.LiveDescendantCount("c_parent"), "LiveDescendantCount")

	// Idempotent: the proxy calls this on every turn of the thread.
	ck.Require().NoError(c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"), "second EnsureThreadChild")
	// Descendants KEEPS the synthetic child (agent_list and the budget member
	// lists must see it); only the MaxChildren gate above excludes it.
	ck.Eq(1, len(c.st.Descendants("c_parent")), "Descendants")
}

func TestNoteSubagentNamesTheChildAfterItsToolCall(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	ck.Require().NoError(c.EnsureThreadChild("c_parent", "thread-a", "conv-uuid-a"), "EnsureThreadChild")
	id := threadChildID("c_parent", "thread-a")

	// The join: message msg_sub1 belongs to thread-a, and the frame carrying it
	// named toolu_ABC as its spawning call.
	c.noteSubagentToolCall("c_parent", "thread-a", "toolu_ABC")

	snap, ok := c.st.Get(id)
	ck.Require().True(ok, "no synthetic child %q", id)
	got := snap.Labels[labelSpawnedByTool]
	ck.Eq("toolu_ABC", got, "%s = %q, want", labelSpawnedByTool, got)
	ck.Eq("task:toolu_ABC", snap.Name, "name")

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
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	ck.Require().Nil(c.captureStore, "newTestController built a capture store without a pool; the nil path is not exercised")
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	// Must return, not panic, and must not touch the store: there is no store
	// to touch, and the synthetic child must stay unnamed.
	c.HandleSubagentObservation("c_parent", child.SubagentObservation{
		MessageID: "msg_whatever", ParentToolUseID: "toolu_XYZ",
	})
	_, ok := c.st.Get(threadChildID("c_parent", "thread-a"))
	ck.False(ok, "a nil capture store must never create or rename a synthetic child")
}

// TestHandleSubagentObservationJoinsTheThreadByMessageID pins the join end to
// end against the real capture store: a turn whose response_message_id is the
// frame's message id must resolve to the synthetic child the proxy created for
// that thread, which is only true because the daemon stamps
// X-Rafiki-Session: childID and ThreadOfPredecessorInSession scopes to that
// session family. Needs a database (skips without RAFIKI_TEST_DSN).
func TestHandleSubagentObservationJoinsTheThreadByMessageID(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	cs := capture.NewCaptureStore(pool)

	convID, err := cs.EnsureConversationByExternalRef(context.Background(), capture.ConversationRef{
		OriginEntrypoint: "test", DrivenBy: "client", ExternalRef: "c_joinparent",
	})
	ck.Require().NoError(err, "EnsureConversationByExternalRef")
	turnID, createdAt, err := cs.InsertTurnIntent(context.Background(), capture.TurnIntent{
		ConversationID: convID, Model: "m", Source: "rafiki-claude", AuthorKind: "agent",
	})
	ck.Require().NoError(err, "InsertTurnIntent")
	// The founding turn of a subagent thread: thread_id = its own id.
	ck.Require().NoError(cs.RecordThread(context.Background(), "c_joinparent", convID, turnID, createdAt, "", "msg_join_1", true), "RecordThread")

	c := newTestController(t)
	c.captureStore = cs
	c.st.Insert(&childstore.Session{
		ChildID: "c_joinparent", Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	ck.Require().NoError(c.EnsureThreadChild("c_joinparent", turnID, convID), "EnsureThreadChild")

	c.HandleSubagentObservation("c_joinparent", child.SubagentObservation{
		MessageID: "msg_join_1", ParentToolUseID: "toolu_JOIN",
	})
	snap, ok := c.st.Get(threadChildID("c_joinparent", turnID))
	ck.Require().True(ok, "no synthetic child for the joined thread %q", turnID)
	ck.Eq("toolu_JOIN", snap.Labels[labelSpawnedByTool], "label")
	ck.Eq("task:toolu_JOIN", snap.Name, "name")

	// A message id the store never saw resolves to nothing: the observation is
	// dropped silently and the child keeps its recorded identity.
	c.HandleSubagentObservation("c_joinparent", child.SubagentObservation{
		MessageID: "msg_never_seen", ParentToolUseID: "toolu_OTHER",
	})
	if snap2, _ := c.st.Get(threadChildID("c_joinparent", turnID)); snap2.Name != "task:toolu_JOIN" {
		t.Errorf("name changed on an unresolvable observation: %q", snap2.Name)
	}
}
