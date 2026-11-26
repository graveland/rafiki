// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// nativeRefusalCode is the connect.Code a native-child refusal must carry. It is
// named once so the three verbs that refuse cannot drift apart.
const nativeRefusalCode = connect.CodeInvalidArgument

// seededRecordingStore is recordingChildStore plus a fixed List: the boot walk's
// input. Splitting it this way keeps the shared fake in shutdown_test.go as it
// was.
type seededRecordingStore struct {
	recordingChildStore
	recs []childstore.ChildRecord
}

func (s *seededRecordingStore) List(context.Context) ([]childstore.ChildRecord, error) {
	return s.recs, nil
}

// TestEnsureThreadChildPersistsRow is the core of the change: a Task subagent
// used to live only in memory, so a restart lost it and the database knew
// nothing about it. The row must exist, at birth, carrying the thread's
// conversation and the lineage labels behind the generated columns.
func TestEnsureThreadChildPersistsRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := scratchPool(t)

	ctrl := newTestController(t)
	ctrl.children = childstoredb.New(pool)
	const parent = "c_nat_row_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusIdle, Cwd: t.TempDir(),
	})

	var convID string
	c.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by) VALUES ('test', 'client') RETURNING id::text`).
		Scan(&convID), "seed the thread's conversation")

	c.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-row", convID), "EnsureThreadChild")

	id := threadChildID(parent, "thread-row")
	var (
		gotConv, kind, name, status string
		parentCol, rootCol          string
		labels                      map[string]string
		pid                         *int
	)
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT COALESCE(conversation_id::text, ''), kind, COALESCE(name, ''), status,
		       COALESCE(parent_id, ''), COALESCE(root_id, ''), labels, pid
		  FROM conversations.child WHERE child_id = $1`, id).
		Scan(&gotConv, &kind, &name, &status, &parentCol, &rootCol, &labels, &pid),
		"the child row must exist: EnsureThreadChild persists it")
	c.Eq(convID, gotConv, "conversation_id is the conversationID argument")
	c.Eq(protocol.KindClaude, kind, "kind")
	c.Eq("task:thread-row", name, "name")
	c.Eq(string(protocol.StatusIdle), status, "a fresh thread child is idle")
	c.Eq(parent, parentCol, "parent_id (the generated column over the label)")
	c.Eq(parent, rootCol, "root_id (the parent is top level, so it is its own root)")
	c.True(pid == nil, "pid must be stored NULL: a native child has no process, got %v", pid)
	// writeRecord stamps the owning daemon's label onto the row it writes, so
	// the lineage set is what is asserted and the stamp is checked separately
	// (a whole-map comparison would have to know this daemon's generated id).
	c.NotEmpty(labels["rafiki/daemon"], "the row must carry the owning daemon label")
	delete(labels, "rafiki/daemon")
	c.EqDeep(map[string]string{
		childstore.LabelParent: parent,
		childstore.LabelRoot:   parent,
		labelNativeSubagent:    "1",
	}, labels, "the three lineage labels")
}

// TestEnsureThreadChildRefusedWhenRowCannotBeWritten pins "no row, no child": a
// persistence failure takes the session back out of the store, publishes nothing
// and returns the error, rather than leaving a child every other surface can see
// and the database cannot.
func TestEnsureThreadChildRefusedWhenRowCannotBeWritten(t *testing.T) {
	c := assert.NewCollecting(t)
	ctrl := newTestController(t)
	store := &failingChildStore{}
	ctrl.children = store

	const parent = "c_nat_fail_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})

	ck := assert.NewAborting(t)
	err := ctrl.EnsureThreadChild(parent, "thread-fail", "conv-fail")
	ck.Require().Error(err, "EnsureThreadChild must refuse a child whose row cannot be written")
	id := threadChildID(parent, "thread-fail")
	if _, ok := ctrl.st.Get(id); ok {
		t.Fatal("a thread child whose row could not be written stayed in the store")
	}
	c.ElementsMatch([]string{id}, store.failed, "the row write must have been attempted for the child")
	events, err := ctrl.evlog.Read(context.Background(), id, -1, 0)
	ck.Require().NoError(err, "read the child's event log")
	ck.Empty(events, "a child with no row must be announced to nobody")
}

// TestNativeChildExitsWithParentSpawnedChildKeepsRunning is the scope boundary
// in executable form: a native child is a thread inside its parent's process and
// ends with it, while a SPAWNED child of the same parent keeps running — killing
// a parent has never stopped its spawned children and this change must not start.
func TestNativeChildExitsWithParentSpawnedChildKeepsRunning(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctrl := newTestController(t)
	store := &recordingChildStore{}
	ctrl.children = store
	parent := spawnTestChild(t, ctrl, nil)

	// A spawned (non-native) descendant that is running, with no process behind
	// it: only its store entry matters here.
	const spawned = "c_nat_spawned_running"
	ctrl.st.Insert(&childstore.Session{
		ChildID: spawned, Kind: protocol.KindClaude, Status: protocol.StatusIdle, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: parent, childstore.LabelRoot: parent},
	})
	c.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-live", "conv-live"), "EnsureThreadChild")
	nativeID := threadChildID(parent, "thread-live")

	_, err := ctrl.Kill(ctx, parent, 5*time.Second, 2*time.Second)
	c.NoError(err, "Kill parent")
	waitForExited(t, ctrl.st, parent, 5*time.Second)
	waitForExited(t, ctrl.st, nativeID, 5*time.Second)

	parentSnap, _ := ctrl.st.Get(parent)
	nativeSnap, ok := ctrl.st.Get(nativeID)
	c.Require().True(ok, "the native child must still be in the store, exited — not forgotten")
	c.Eq(protocol.StatusExited, nativeSnap.Status, "native child status")
	c.True(nativeSnap.ExitedAt.Equal(parentSnap.ExitedAt),
		"the native child must exit at its parent's exit time: native=%v parent=%v",
		nativeSnap.ExitedAt, parentSnap.ExitedAt)

	spawnedSnap, ok := ctrl.st.Get(spawned)
	c.Require().True(ok, "a spawned child must survive its parent's exit")
	c.Eq(protocol.StatusIdle, spawnedSnap.Status, "a spawned child keeps running when its parent dies")

	// And the exit reached the ROW, not only the store: a native row left idle is
	// a subagent the next boot claims is still running.
	last, _ := store.lastUpsertFor(nativeID)
	c.True(store.hasUpsertWithStatus(nativeID, string(protocol.StatusExited)),
		"the native child's exit must be persisted; its last write said %q", last.Status)
}

// TestNativeChildIdleAgainOnResumedThread pins the resume arm: a resumed parent
// re-enters a thread it already ran, so the same (parent, thread) lands on the
// same child — which must read idle again, in memory AND in the row.
func TestNativeChildIdleAgainOnResumedThread(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	store := &recordingChildStore{}
	ctrl.children = store

	const parent = "c_nat_revive_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	c.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-revive", "conv-first"), "EnsureThreadChild")
	id := threadChildID(parent, "thread-revive")

	// The parent exited once: the thread ended with it.
	c.Require().True(ctrl.exitNativeChild(id, time.Now()), "exit the native child")
	c.Require().NoError(ctrl.st.Update(id, func(s *childstore.Session) {
		s.ExitSignal = ""
	}), "keep the snapshot readable")
	exited, _ := ctrl.st.Get(id)
	c.Require().Eq(protocol.StatusExited, exited.Status, "the child must be exited before the revive")

	// The parent is resumed and runs the same thread again, this time landing on
	// a conversation id the daemon had not seen.
	c.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-revive", "conv-second"), "EnsureThreadChild on a resumed thread")

	snap, ok := ctrl.st.Get(id)
	c.Require().True(ok, "the resumed thread's child must still be there")
	c.Eq(protocol.StatusIdle, snap.Status, "a resumed thread's child reads idle again")
	c.True(snap.ExitedAt.IsZero(), "the exit instant must be cleared, got %v", snap.ExitedAt)
	c.True(snap.ExitCode == nil, "the exit code must be cleared, got %v", snap.ExitCode)
	c.Eq("conv-second", snap.SessionID, "the conversation id follows the thread's current branch")

	rec, ok := store.lastUpsertFor(id)
	c.Require().True(ok, "the revive must be persisted, not only held in memory")
	c.Eq(string(protocol.StatusIdle), rec.Status, "the persisted status")
	c.True(rec.ExitedAt.IsZero(), "the persisted exit instant must be cleared, got %v", rec.ExitedAt)

	// The rail hears about it too: agent_status is the only event that moves a
	// row's glyph, and nothing else announces a native child's revival.
	events, err := ctrl.evlog.Read(context.Background(), id, -1, 0)
	c.Require().NoError(err, "read the child's event log")
	var revived bool
	for _, ev := range events {
		if ev.Type != "agent_status" {
			continue
		}
		var decoded rafikiv1.Event
		c.Require().NoError(protojson.Unmarshal(ev.Payload, &decoded), "unmarshal an agent_status event")
		if decoded.GetAgentStatus().GetState() == string(protocol.StatusIdle) {
			revived = true
		}
	}
	c.True(revived, "the revive must publish an agent_status idle event")
}

// TestCloseParentClosesNativeChildren pins the close half of the scope boundary:
// a native child is closed WITH its parent, through the same
// persist-before-tombstone step, so nothing is left pointing at a session that
// no longer exists. The transcripts survive, exactly as the parent's does.
func TestCloseParentClosesNativeChildren(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := scratchPool(t)

	ctrl := newTestController(t)
	ctrl.children = childstoredb.New(pool)
	const parent = "c_nat_close_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusExited,
		Cwd: t.TempDir(), StartedAt: time.Now(), ExitedAt: time.Now(),
	})
	var convID string
	c.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by) VALUES ('test', 'client') RETURNING id::text`).
		Scan(&convID), "seed the thread's conversation")
	c.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-close", convID), "EnsureThreadChild")
	id := threadChildID(parent, "thread-close")
	c.Require().True(ctrl.exitNativeChild(id, time.Now()), "exit the native child")

	c.Require().NoError(ctrl.Close(parent), "Close parent")

	for _, childID := range []string{parent, id} {
		if _, ok := ctrl.st.Get(childID); ok {
			t.Fatalf("%s survived the close in the store", childID)
		}
		var closedAt *time.Time
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT closed_at FROM conversations.child WHERE child_id = $1`, childID).Scan(&closedAt),
			"the %s row must still exist (a close is a tombstone, not a delete)", childID)
		c.Require().True(closedAt != nil, "%s's row must be tombstoned by the close", childID)
	}
}

// TestCloseParentFailsClosedWhenNativePersistFails pins the failure rule: a
// native child whose row cannot be written fails the PARENT's close before the
// parent is persisted or tombstoned, so a closed parent can never leave a live
// subagent row behind it.
func TestCloseParentFailsClosedWhenNativePersistFails(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)

	const parent = "c_nat_closefail_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusExited,
		Cwd: t.TempDir(), StartedAt: time.Now(), ExitedAt: time.Now(),
	})
	// The native child's write is refused; the parent's would succeed. failIDs
	// narrows the failure so the assertion can tell which write happened.
	nativeID := threadChildID(parent, "thread-closefail")
	ctrl.children = nil
	c.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-closefail", "conv-closefail"), "EnsureThreadChild")
	store := &failingChildStore{failIDs: map[string]bool{nativeID: true}}
	ctrl.children = store

	err := ctrl.Close(parent)
	c.Require().Error(err, "Close must fail closed when a native child's row cannot be written")
	c.StrContains(err.Error(), nativeID, "the refusal must name the native child: %v", err)
	c.ElementsMatch([]string{nativeID}, store.failed,
		"the native child's write must be the only one attempted: the parent must not be persisted after it")

	if _, ok := ctrl.st.Get(nativeID); !ok {
		t.Fatal("a native child whose row could not be written must stay in the store, not be forgotten")
	}
	if _, ok := ctrl.st.Get(parent); !ok {
		t.Fatal("the parent must stay in the store when its close failed closed")
	}
}

// TestCloseAllExitedLeavesAnOrphanedNativeRow pins the sweep's native skip: a
// native row whose parent is not in the sweep at all (already closed, or never
// loaded) must be left for an operator's `rafiki close <child>` rather than
// swept on its own — it is not a process the sweep can reason about.
func TestCloseAllExitedLeavesAnOrphanedNativeRow(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	const orphan = "c_nat_orphan"
	now := time.Now()
	ctrl.st.Insert(&childstore.Session{
		ChildID: orphan, Kind: protocol.KindClaude, Native: true, Status: protocol.StatusExited,
		StartedAt: now, ExitedAt: now,
		Labels: map[string]string{childstore.LabelParent: "c_nat_orphan_parent", labelNativeSubagent: "1"},
	})

	closed, err := ctrl.CloseAllExited(0, nil)
	ck.NoError(err, "CloseAllExited")
	ck.Empty(closed, "a native row with no parent in the sweep must not be reported closed")
	if _, ok := ctrl.st.Get(orphan); !ok {
		t.Fatal("CloseAllExited closed a native child on its own; only its parent's close may")
	}
}

// nameCollisionStore refuses any name it was told is taken, the way the
// child_sibling_name_live index does, and records every name it was asked to
// write so a test can see which attempt succeeded.
type nameCollisionStore struct {
	stubChildStore
	taken map[string]bool
	names []string
}

func (s *nameCollisionStore) Upsert(_ context.Context, rec childstore.ChildRecord) error {
	s.names = append(s.names, rec.Name)
	if s.taken[rec.Name] {
		return fmt.Errorf("childstoredb: upsert %s: %w", rec.ChildID, childstore.ErrNameTaken)
	}
	return nil
}

// TestEnsureThreadChildSuffixesATakenName pins the retry: "task:<thread>" is a
// name a live sibling can already hold, so the write is retried with -2 through
// -9 — the SAME row, since the child id is deterministic — before giving up.
func TestEnsureThreadChildSuffixesATakenName(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	store := &nameCollisionStore{taken: map[string]bool{"task:thread-taken": true}}
	ctrl.children = store

	const parent = "c_nat_name_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	ck.Require().NoError(ctrl.EnsureThreadChild(parent, "thread-taken", "conv-name"),
		"a taken name must be survived, not refused")

	ck.EqDeep([]string{"task:thread-taken", "task:thread-taken-2"}, store.names,
		"the retry names, in order")
	snap, ok := ctrl.st.Get(threadChildID(parent, "thread-taken"))
	ck.Require().True(ok, "the child must exist under its deterministic id")
	ck.Eq("task:thread-taken-2", snap.Name, "the in-memory name follows the row that landed")
}

// TestEnsureThreadChildRefusedAfterEverySuffixIsTaken pins the bound: nine taken
// names exhaust the retries, and the child is then refused exactly like any
// other write failure — no row, no child.
func TestEnsureThreadChildRefusedAfterEverySuffixIsTaken(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	taken := map[string]bool{"task:thread-full": true}
	for n := 2; n <= 9; n++ {
		taken[fmt.Sprintf("task:thread-full-%d", n)] = true
	}
	store := &nameCollisionStore{taken: taken}
	ctrl.children = store

	const parent = "c_nat_namefull_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindClaude, Status: protocol.StatusIdle,
	})
	err := ctrl.EnsureThreadChild(parent, "thread-full", "conv-full")
	ck.Require().Error(err, "a child whose every candidate name is taken must be refused")
	ck.True(errors.Is(err, childstore.ErrNameTaken), "the refusal must keep the sentinel: %v", err)
	ck.Len(store.names, 9, "one attempt for the base name and one per suffix; got %v", store.names)
	if _, ok := ctrl.st.Get(threadChildID(parent, "thread-full")); ok {
		t.Fatal("a refused thread child stayed in the store")
	}
}

// TestKillNativeChildRefused pins the refusal a native child answers: it has no
// process to stop, so Kill names the parent to kill instead and changes nothing.
func TestKillNativeChildRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	id := insertNativeParent(t, ctrl, "c_nat_killrefuse_parent", protocol.StatusIdle)

	_, err := ctrl.Kill(context.Background(), id, 0, 0)
	assertNativeRefusal(ck, err, id, "c_nat_killrefuse_parent")

	snap, ok := ctrl.st.Get(id)
	ck.Require().True(ok, "a refused Kill must not remove the child")
	ck.Eq(protocol.StatusIdle, snap.Status, "a refused Kill must not change the status")
}

// TestSendToNativeChildRefused pins the same refusal on the frame path: a
// prompt, steer or abort for a native child has nowhere to go.
func TestSendToNativeChildRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	id := insertNativeParent(t, ctrl, "c_nat_sendrefuse_parent", protocol.StatusIdle)

	err := ctrl.Send(id, []byte(`{"type":"prompt","id":"p1","message":"hello"}`))
	assertNativeRefusal(ck, err, id, "c_nat_sendrefuse_parent")
	if snap, ok := ctrl.st.Get(id); !ok || snap.Status != protocol.StatusIdle {
		t.Fatalf("a refused Send changed the child: ok=%v status=%v", ok, snap.Status)
	}
}

// TestResumeNativeChildRefused pins the same refusal on resume: a native child
// comes back when its parent does, never by itself.
func TestResumeNativeChildRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	id := insertNativeParent(t, ctrl, "c_nat_resumerefuse_parent", protocol.StatusIdle)
	if _, err := ctrl.Kill(context.Background(), id, 0, 0); err == nil {
		t.Fatal("Kill must refuse a native child, so this test needs another way to exit it")
	}
	ck.Require().True(ctrl.exitNativeChild(id, time.Now()), "exit the native child")

	_, err := ctrl.Resume(context.Background(), id, "")
	assertNativeRefusal(ck, err, id, "c_nat_resumerefuse_parent")
}

// assertNativeRefusal checks the whole refusal surface of one verb: the domain
// sentinel, the Connect code a client sees, and a message naming the child and
// the parent to address instead.
func assertNativeRefusal(ck *assert.C, err error, childID, parentID string) {
	ck.Require().Error(err, "the verb must refuse a native subagent child")
	var ce *connectapi.ControllerError
	ck.Require().True(errors.As(err, &ce), "the refusal must be a ControllerError, got %T: %v", err, err)
	ck.Eq(protocol.ErrNativeSubagent, ce.Code, "protocol code")
	ck.Eq(nativeRefusalCode, connect.CodeOf(connectapi.ConnectErr(err)),
		"a native-child refusal must reach the client as invalid_argument")
	ck.StrContains(ce.Message, childID, "the message must name the child: %s", ce.Message)
	ck.StrContains(ce.Message, parentID, "the message must name the parent to address: %s", ce.Message)
}

// TestRecoveryLoadsNativeRowWithoutResuming pins recovery's native branch: the
// row is loaded as a Native session with the status the row recorded, and no
// resume, adoption, lease or executor logic runs for it — a thread inside a
// parent's process has nothing to restart. It also keeps the admission
// invariant: LiveDescendantCount does not count it.
func TestRecoveryLoadsNativeRowWithoutResuming(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	logs := captureLogs(t)

	ctrl := newTestController(t)
	ctrl.daemonID = "me"
	store := &seededRecordingStore{}
	ctrl.children = store

	const (
		parent = "c_nat_rec_parent"
		conv   = "11111111-1111-1111-1111-111111111111"
	)
	nativeID := threadChildID(parent, "t1")
	store.recs = []childstore.ChildRecord{
		// The parent: its own exited row, so the walk has a container to hang the
		// thread off.
		{
			ChildID: parent, Kind: protocol.KindClaude, Status: string(protocol.StatusExited),
			DaemonID: "me", Labels: map[string]string{"rafiki/daemon": "me"},
		},
		// The thread: another daemon's row, still reading idle, which is exactly
		// the shape that would be adopted and resumed if the native branch were
		// missing.
		{
			ChildID: nativeID, Kind: protocol.KindClaude, Status: string(protocol.StatusIdle),
			ConversationID: conv, DaemonID: "old-daemon",
			Labels: map[string]string{
				labelNativeSubagent:    "1",
				childstore.LabelParent: parent,
				childstore.LabelRoot:   parent,
				"rafiki/daemon":        "old-daemon",
			},
		},
	}

	ctrl.loadChildren(ctx)

	snap, ok := ctrl.st.Get(nativeID)
	ck.Require().True(ok, "the native row must be loaded into the store")
	ck.True(snap.Native, "the reloaded child must be marked Native, or LiveDescendantCount starts counting subagents")
	ck.Eq(protocol.StatusIdle, snap.Status, "the status comes from the row, not a forced exited")

	// Nothing was attempted on its behalf.
	if _, live := ctrl.cm.Get(nativeID); live {
		t.Fatal("a native child was launched or adopted into the child manager")
	}
	ck.Eq(0, ctrl.pendingCount(), "a native child must not be pended for an executor-connect resume")
	for _, a := range store.adoptions {
		if a[0] == nativeID {
			t.Fatalf("recovery adopted a native child's row: %v", a)
		}
	}
	ck.NotStrContains(logs.String(), "auto-resuming child", "a native child must never be resumed; log:")

	// The admission invariant, non-vacuously: it is in the store, and it still
	// does not consume the parent's MaxChildren grant — while a spawned child of
	// the same parent does.
	ck.Eq(0, ctrl.st.LiveDescendantCount(parent), "LiveDescendantCount must not count a native subagent")
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_nat_rec_spawned", Kind: protocol.KindFundi, Status: protocol.StatusIdle,
		Labels: map[string]string{childstore.LabelParent: parent, childstore.LabelRoot: parent},
	})
	ck.Eq(1, ctrl.st.LiveDescendantCount(parent), "a spawned child still counts, so the exclusion is specific")
}
