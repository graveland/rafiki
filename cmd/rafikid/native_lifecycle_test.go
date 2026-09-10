package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// insertNativeParent inserts an exited claude parent plus one synthetic thread
// child of it, returning the synthetic child's id.
func insertNativeParent(t *testing.T, c *Controller, parentID string, parentStatus protocol.Status) string {
	t.Helper()
	c.st.Insert(&childstore.Session{
		ChildID: parentID, Kind: protocol.KindClaude, Status: parentStatus,
	})
	if err := c.EnsureThreadChild(parentID, "thread-a", "conv-a"); err != nil {
		t.Fatalf("EnsureThreadChild: %v", err)
	}
	return threadChildID(parentID, "thread-a")
}

// TestKillEndsANativeChild is the bug this whole change exists for: a synthetic
// child has no process, so Kill's c.cm lookup missed and answered "child not
// found" for a child the operator could see in the rail.
func TestKillEndsANativeChild(t *testing.T) {
	c := newTestController(t)
	id := insertNativeParent(t, c, "c_parent", protocol.StatusIdle)

	res, err := c.Kill(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatalf("Kill on a native child: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0: a thread ended, it did not die", res.ExitCode)
	}
	snap, ok := c.st.Get(id)
	if !ok {
		t.Fatal("Kill removed the row; it must mark it exited so Close can still reach it")
	}
	if snap.Status != protocol.StatusExited {
		t.Errorf("status = %q, want %q", snap.Status, protocol.StatusExited)
	}

	// A second kill is the ordinary already-exited answer, not not-found.
	_, err = c.Kill(context.Background(), id, 0, 0)
	var ce *control.ControllerError
	if !errors.As(err, &ce) || ce.Code != protocol.ErrChildExited {
		t.Errorf("second Kill error = %v, want %s", err, protocol.ErrChildExited)
	}
}

// TestCloseEndsANativeChild pins the other half: Close demands exited, and
// nothing could put a native child there before Kill learned to.
func TestCloseEndsANativeChild(t *testing.T) {
	c := newTestController(t)
	id := insertNativeParent(t, c, "c_parent", protocol.StatusIdle)

	if err := c.Close(id); err == nil {
		t.Error("Close on a live native child must refuse; it is not exited")
	}
	if _, err := c.Kill(context.Background(), id, 0, 0); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := c.Close(id); err != nil {
		t.Fatalf("Close on an exited native child: %v", err)
	}
	if _, ok := c.st.Get(id); ok {
		t.Error("Close left the row in the store")
	}
}

// TestParentExitEndsItsNativeChildren: a Task subagent runs inside its parent's
// process, so it cannot outlive it. Driven through a real spawn and kill rather
// than calling exitNativeChildrenOf directly, because the wiring into
// handleChildExit is the part that regresses.
func TestParentExitEndsItsNativeChildren(t *testing.T) {
	t.Parallel()

	c := newTestController(t)
	parent := spawnTestChild(t, c, nil)
	if err := c.EnsureThreadChild(parent, "thread-a", "conv-a"); err != nil {
		t.Fatalf("EnsureThreadChild: %v", err)
	}
	id := threadChildID(parent, "thread-a")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := c.Kill(ctx, parent, 5000, 2000); err != nil {
		t.Fatalf("Kill parent: %v", err)
	}
	waitForExited(t, c.st, parent, 5*time.Second)
	waitForExited(t, c.st, id, 5*time.Second)
}

// TestParentCloseDeletesItsNativeChildren: exit follows exit, close follows
// close. A native child left behind carries a parent label pointing at a
// session that no longer exists, which strands it at the top of the rail.
func TestParentCloseDeletesItsNativeChildren(t *testing.T) {
	c := newTestController(t)
	id := insertNativeParent(t, c, "c_parent", protocol.StatusExited)

	if err := c.Close("c_parent"); err != nil {
		t.Fatalf("Close parent: %v", err)
	}
	if _, ok := c.st.Get(id); ok {
		t.Error("the parent's synthetic child survived its close")
	}
}

// TestCloseAllExitedReportsANativeChildOnce guards the double-report: snaps is
// read before the loop, so an exited native child appears in it AND is deleted
// by its parent's cascade.
func TestCloseAllExitedReportsANativeChildOnce(t *testing.T) {
	c := newTestController(t)
	id := insertNativeParent(t, c, "c_parent", protocol.StatusExited)
	if _, err := c.Kill(context.Background(), id, 0, 0); err != nil {
		t.Fatalf("Kill native child: %v", err)
	}

	closed, err := c.CloseAllExited(0)
	if err != nil {
		t.Fatalf("CloseAllExited: %v", err)
	}
	seen := map[string]int{}
	for _, cid := range closed {
		seen[cid]++
	}
	if seen[id] != 1 {
		t.Errorf("native child reported %d times, want 1 (closed = %v)", seen[id], closed)
	}
	if seen["c_parent"] != 1 {
		t.Errorf("parent reported %d times, want 1 (closed = %v)", seen["c_parent"], closed)
	}
	if _, ok := c.st.Get(id); ok {
		t.Error("the native child survived CloseAllExited")
	}
}

// TestConversationIDResolvesAClaudeChild pins the transcript fix at the seam it
// broke: ConversationID gated on KindFundi, so Connect GetHistory answered
// NotFound for every claude child while GetRecent's own resolver served the
// same child. A native subagent has NO event log rows at all, so GetHistory is
// its only transcript source. Needs a database.
func TestConversationIDResolvesAClaudeChild(t *testing.T) {
	pool := openTestPool(t)
	c := newTestController(t)
	c.pool = pool

	// The branch conversation a thread's founding turn lands on: its
	// external_ref is "<parentChildID>:<threadID>", byte-identical to the
	// synthetic child's id (capture.ResolveThreadConversation).
	id := insertNativeParent(t, c, "c_convparent", protocol.StatusIdle)
	convID, err := capture.NewCaptureStore(pool).EnsureConversationByExternalRef(
		context.Background(), capture.ConversationRef{
			OriginEntrypoint: "test", DrivenBy: "client", ExternalRef: id,
		})
	if err != nil {
		t.Fatalf("EnsureConversationByExternalRef: %v", err)
	}

	got, ok := c.ConversationID(id)
	if !ok || got != convID {
		t.Errorf("ConversationID = (%q, %v), want (%q, true)", got, ok, convID)
	}

	// A child with no conversation row is not found rather than an empty hit.
	if got, ok := c.ConversationID("c_convparent"); ok {
		t.Errorf("ConversationID for an unresolvable child = (%q, true), want false", got)
	}
}
