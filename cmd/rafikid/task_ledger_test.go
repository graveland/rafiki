// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// tasksChangedOn drains ch and returns every TasksChanged event it saw. The
// bus delivers into a buffered channel, so a publish made before this runs is
// still waiting in it.
func tasksChangedOn(ch <-chan *rafikiv1.Event) []*rafikiv1.Event {
	var out []*rafikiv1.Event
	for {
		select {
		case ev := <-ch:
			if ev.GetTasksChanged() != nil {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

// TestTaskLedgerFundiChildUsesItsSessionID pins the fundi half: the engine
// mints the conversation uuid, so the ledger is keyed by it and an engine that
// has not started yet yields the empty id rather than an invented one.
func TestTaskLedgerFundiChildUsesItsSessionID(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{}

	got, err := c.ensureLedgerConversation(context.Background(), childstore.Snapshot{
		ChildID: "c_fundi", Kind: protocol.KindFundi, SessionID: "0f8fad5b-d9cb-469f-a165-70867728950e",
	})
	ck.Require().NoError(err, "ensureLedgerConversation with a session id")
	ck.Eq("0f8fad5b-d9cb-469f-a165-70867728950e", got, "the ledger is keyed by the engine's conversation")

	got, err = c.ensureLedgerConversation(context.Background(), childstore.Snapshot{
		ChildID: "c_fundi", Kind: protocol.KindFundi,
	})
	ck.Require().NoError(err, "ensureLedgerConversation without a session id")
	ck.Eq("", got, "no engine conversation yet is not an error")
}

// TestTaskLedgerStorelessDaemonKeysByChildID pins the degradation: with no
// capture store there is no conversation table to key against, and the
// in-memory ledger accepts a synthetic id.
func TestTaskLedgerStorelessDaemonKeysByChildID(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{}

	got, err := c.ensureLedgerConversation(context.Background(), childstore.Snapshot{
		ChildID: "c_claude", Kind: protocol.KindClaude,
	})
	ck.Require().NoError(err, "ensureLedgerConversation on a store-less daemon")
	ck.Eq("child:c_claude", got, "synthetic ledger key")
}

// TestTaskLedgerClaudeChildEnsuresByChildID pins that a proxy-captured child's
// ledger row is the row the proxy's own traffic correlates to — external_ref =
// child id, client-driven — and that a second call lands on that same row
// rather than creating a second one.
func TestTaskLedgerClaudeChildEnsuresByChildID(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := context.Background()
	childID := fmt.Sprintf("c_ledger_%d", time.Now().UnixNano())

	// The whole suite shares this database, so the fixture must remove exactly
	// what it inserted; t.Context() is already canceled by cleanup time.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cctx, `DELETE FROM conversations.conversation WHERE external_ref = $1`, childID); err != nil {
			t.Logf("cleanup ledger conversation: %v", err)
		}
	})

	c := &Controller{st: childstore.New(), captureStore: capture.NewCaptureStore(pool)}
	snap := childstore.Snapshot{ChildID: childID, Kind: protocol.KindClaude}

	first, err := c.ensureLedgerConversation(ctx, snap)
	ck.NoError(err, "first ensureLedgerConversation")
	ck.False(first == "", "first call returned an empty conversation id")

	second, err := c.ensureLedgerConversation(ctx, snap)
	ck.NoError(err, "second ensureLedgerConversation")
	ck.Eq(first, second, "a repeated resolve must return the same conversation")

	var externalRef, drivenBy, entrypoint string
	var name, owner *string
	ck.NoError(pool.QueryRow(ctx,
		`SELECT coalesce(external_ref,''), driven_by, origin_entrypoint, name, owner_user_id::text
		   FROM conversations.conversation WHERE id = $1::uuid`, first).
		Scan(&externalRef, &drivenBy, &entrypoint, &name, &owner), "read the created conversation")

	ck.Eq(childID, externalRef, "external_ref is the child id")
	ck.Eq("client", drivenBy, "the proxy path is client-driven")
	ck.Eq(string(protocol.KindClaude), entrypoint, "origin_entrypoint is the kind the daemon stamps as X-Rafiki-Source")
	ck.True(name == nil, "name is left for Rename, as on the proxy path: %v", name)
	ck.True(owner == nil, "an unowned child is unattributed, as on the proxy path: %v", owner)
}

// TestTaskLedgerAddPublishesOnTheCallersBusAfterCommit pins the notice path a
// task_add through a child's ledger produces: the event names the OWNER child
// as its child_id, and the row it reports is already readable when it lands.
func TestTaskLedgerAddPublishesOnTheCallersBusAfterCommit(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := displayServiceController(t)
	c.tasks = tasks.NewMemoryStore()
	const owner = "c_owner"
	c.st.Insert(&childstore.Session{
		ChildID: owner, Kind: protocol.KindFundi, Status: protocol.StatusIdle,
		StartedAt: time.Now(), SessionID: "sess-owner",
	})

	snap, ok := c.st.Get(owner)
	ck.Require().True(ok, "owner child registered")
	convID, err := c.ensureLedgerConversation(t.Context(), snap)
	ck.Require().NoError(err, "ensureLedgerConversation")
	ck.Eq("sess-owner", convID, "fundi ledger key")

	ch, cancel := c.native.Subscribe(owner)
	defer cancel()

	_, err = c.tasksFor(owner).Add(t.Context(), convID, "", []tasks.NewTask{
		{Content: "write the thing", ActiveForm: "writing the thing"},
	})
	ck.Require().NoError(err, "Add through the notifying ledger")

	seen := tasksChangedOn(ch)
	ck.Len(seen, 1, "one Add is one TasksChanged event")
	if len(seen) != 1 {
		return
	}
	ck.Eq(owner, seen[0].GetChildId(), "the event names the ledger's owner as its child_id")
	ev := seen[0].GetTasksChanged()
	ck.Eq(tasks.NoticeAdded, ev.GetKind(), "kind")
	ck.Len(ev.GetTasks(), 1, "one row reported")
	row := ev.GetTasks()[0]
	ck.Eq("1", row.GetHandle(), "handle")
	ck.Eq("write the thing", row.GetContent(), "content")
	ck.Eq("writing the thing", row.GetActiveForm(), "active_form")
	ck.Eq(string(tasks.StatusPending), row.GetStatus(), "status")

	rows, err := c.tasks.List(t.Context(), tasks.ListFilter{ConversationID: convID})
	ck.Require().NoError(err, "List after the event")
	ck.Len(rows, 1, "the row the event reported is already committed")
}

// TestTaskLedgerSpawnAssignPublishesOnTheSpawner pins Controller.Spawn's
// post-admission Assign: the child's row is assigned in the spawner's ledger,
// and the notice goes on the SPAWNER's bus, not the new child's.
func TestTaskLedgerSpawnAssignPublishesOnTheSpawner(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.tasks = tasks.NewMemoryStore()
	const parent = "c_spawner"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindFundi, Status: protocol.StatusIdle,
		StartedAt: time.Now(), SessionID: "sess-spawner", Labels: map[string]string{},
		MaxDepth: 1, MaxChildren: 2,
	})
	if _, err := ctrl.tasks.Add(t.Context(), "sess-spawner", "", []tasks.NewTask{{Content: "delegate it"}}); err != nil {
		t.Fatalf("seed spawner ledger: %v", err)
	}

	ch, cancel := ctrl.native.Subscribe(parent)
	defer cancel()

	ctx, cancelSpawn := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelSpawn()
	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindClaude, PiBinary: fakePiBin(t), NoSession: true,
		ParentChildID: parent, Task: "1", SpawnerConversationID: "sess-spawner",
	}, users.Identity{})
	ck.NoError(err, "spawn a delegated child")

	seen := tasksChangedOn(ch)
	ck.Len(seen, 1, "the assignment is one TasksChanged event on the spawner's bus")
	if len(seen) == 1 {
		ck.Eq(parent, seen[0].GetChildId(), "the event names the spawner as its child_id")
		tc := seen[0].GetTasksChanged()
		ck.Eq(tasks.NoticeAssigned, tc.GetKind(), "kind")
		ck.Len(tc.GetTasks(), 1, "one row reported")
		ck.Eq(res.ChildID, tc.GetTasks()[0].GetAssignee(), "assignee is the spawned child")
		ck.Eq("1", tc.GetTasks()[0].GetHandle(), "handle")
	}
}

// TestTaskLedgerOrphanSweepPublishesOnTheParent pins handleChildExit's sweep:
// a dead child's unfinished work is orphaned in the parent's ledger and the
// notice lands on the parent's bus, with the assignee retained.
func TestTaskLedgerOrphanSweepPublishesOnTheParent(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.tasks = tasks.NewMemoryStore()
	const parent = "c_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindFundi, Status: protocol.StatusIdle,
		StartedAt: time.Now(), SessionID: "sess-parent", Labels: map[string]string{},
		MaxDepth: 1, MaxChildren: 2,
	})

	ctx, cancelSpawn := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelSpawn()
	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindClaude, PiBinary: fakePiBin(t), NoSession: true,
		ParentChildID: parent,
	}, users.Identity{})
	ck.NoError(err, "spawn the child whose work will be orphaned")

	const conv = "sess-dead"
	if _, err := ctrl.tasks.Add(t.Context(), conv, "", []tasks.NewTask{{Content: "unfinished"}}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if _, err := ctrl.tasks.Assign(t.Context(), conv, "1", res.ChildID); err != nil {
		t.Fatalf("assign to the child: %v", err)
	}

	ch, cancel := ctrl.native.Subscribe(parent)
	defer cancel()
	endChild(t, ctrl, res.ChildID)

	seen := tasksChangedOn(ch)
	ck.Len(seen, 1, "the sweep is one TasksChanged event on the parent's bus")
	if len(seen) != 1 {
		return
	}
	ck.Eq(parent, seen[0].GetChildId(), "the event names the parent, whose ledger the rows live in")
	tc := seen[0].GetTasksChanged()
	ck.Eq(tasks.NoticeOrphaned, tc.GetKind(), "kind")
	ck.Len(tc.GetTasks(), 1, "one row reported")
	row := tc.GetTasks()[0]
	ck.Eq("1", row.GetHandle(), "handle")
	ck.Eq(string(tasks.StatusOrphaned), row.GetStatus(), "status")
	ck.Eq(res.ChildID, row.GetAssignee(), "the assignee is retained: a coordinator needs to know who held it")
}

// TestTaskLedgerOrphanSweepFollowsTheLegacyParentLabel pins that the sweep
// resolves the parent through the childstore's resolver rather than the label
// directly: a live parent recorded under the pre-rename parent label is the
// child's parent to every other parent walk in the daemon, so it receives the
// orphaned notice as well.
func TestTaskLedgerOrphanSweepFollowsTheLegacyParentLabel(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.tasks = tasks.NewMemoryStore()
	const parent = "c_legacy_parent"
	ctrl.st.Insert(&childstore.Session{
		ChildID: parent, Kind: protocol.KindFundi, Status: protocol.StatusIdle,
		StartedAt: time.Now(), SessionID: "sess-legacy-parent", Labels: map[string]string{},
		MaxDepth: 1, MaxChildren: 2,
	})

	ctx, cancelSpawn := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelSpawn()
	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindClaude, PiBinary: fakePiBin(t), NoSession: true,
		ParentChildID: parent,
	}, users.Identity{})
	ck.NoError(err, "spawn the child whose work will be orphaned")

	// The same lineage a record persisted before the rename carries: only the
	// legacy parent label remains.
	_, err = ctrl.st.SetLabels(res.ChildID,
		map[string]string{"fundi/parent": parent}, []string{childstore.LabelParent})
	ck.NoError(err, "move the child's lineage to the pre-rename label")
	snap, ok := ctrl.st.Get(res.ChildID)
	ck.Require().True(ok, "the child is registered")
	ck.Eq("", snap.Labels[childstore.LabelParent], "the current label is gone")
	ck.Eq(parent, snap.Labels["fundi/parent"], "the legacy label carries the lineage")

	const conv = "sess-legacy"
	if _, err := ctrl.tasks.Add(t.Context(), conv, "", []tasks.NewTask{{Content: "unfinished"}}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if _, err := ctrl.tasks.Assign(t.Context(), conv, "1", res.ChildID); err != nil {
		t.Fatalf("assign to the child: %v", err)
	}

	ch, cancel := ctrl.native.Subscribe(parent)
	defer cancel()
	endChild(t, ctrl, res.ChildID)

	seen := tasksChangedOn(ch)
	ck.Len(seen, 1, "a legacy-labelled parent's bus gets the sweep's event")
	if len(seen) != 1 {
		return
	}
	ck.Eq(parent, seen[0].GetChildId(), "the event names the parent, whose ledger the rows live in")
	tc := seen[0].GetTasksChanged()
	ck.Eq(tasks.NoticeOrphaned, tc.GetKind(), "kind")
	ck.Len(tc.GetTasks(), 1, "one row reported")
	row := tc.GetTasks()[0]
	ck.Eq("1", row.GetHandle(), "handle")
	ck.Eq(string(tasks.StatusOrphaned), row.GetStatus(), "status")
	ck.Eq(res.ChildID, row.GetAssignee(), "the assignee is retained")
}

// TestTaskLedgerOrphanSweepWithoutParentIsSilent pins the other half: a
// top-level child's work is still orphaned, but nothing is published, because
// there is no owning ledger to announce it on.
func TestTaskLedgerOrphanSweepWithoutParentIsSilent(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.tasks = tasks.NewMemoryStore()

	ctx, cancelSpawn := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelSpawn()
	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Cwd: t.TempDir(), Kind: protocol.KindClaude, PiBinary: fakePiBin(t), NoSession: true,
	}, users.Identity{})
	ck.NoError(err, "spawn a top-level child")

	const conv = "sess-top"
	if _, err := ctrl.tasks.Add(t.Context(), conv, "", []tasks.NewTask{{Content: "unfinished"}}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if _, err := ctrl.tasks.Assign(t.Context(), conv, "1", res.ChildID); err != nil {
		t.Fatalf("assign to the child: %v", err)
	}

	// SubscribeAll, not the child's own bus: that bus is forgotten as the child
	// is torn down, so a wrongly-routed publish would vanish rather than fail
	// the assertion.
	ch, cancel := ctrl.native.SubscribeAll()
	defer cancel()
	endChild(t, ctrl, res.ChildID)

	ck.Len(tasksChangedOn(ch), 0, "no parent means no bus to announce the sweep on")

	rows, err := ctrl.tasks.List(t.Context(), tasks.ListFilter{ConversationID: conv})
	ck.NoError(err, "List after the exit")
	ck.Len(rows, 1, "the sweep still ran")
	if len(rows) == 1 {
		ck.Eq(tasks.StatusOrphaned, rows[0].Status, "status")
	}
}

// TestTaskLedgerNilStoreYieldsNilNotifier pins that a daemon with no ledger
// hands back nil rather than a wrapper around a nil store.
func TestTaskLedgerNilStoreYieldsNilNotifier(t *testing.T) {
	c := &Controller{}
	assert.NewAborting(t).True(c.tasksFor("c_any") == nil, "tasksFor on a store-less controller must be nil")
}

// endChild shuts childID down and waits for handleChildExit — which runs the
// orphan sweep — to finish before returning.
func endChild(t *testing.T, ctrl *Controller, childID string) {
	t.Helper()
	ck := assert.NewAborting(t)
	ch, ok := ctrl.cm.Get(childID)
	ck.Require().True(ok, "child %q is in the manager", childID)
	_, _ = ch.Shutdown(5*time.Second, time.Second)
	ck.Require().True(waitForChildRemoval(ctrl.cm, childID, 30*time.Second),
		"child %q not removed after shutdown", childID)
}
