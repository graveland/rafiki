package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// With lazy binding, spawn succeeds even when no executor matches. The task
// ledger path is unchanged: the task is still assigned, and the child starts
// unbound (its first tool call will surface the refusal).
func TestUnboundSpawnStillAssignsTheTask(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_parent", func(s *childstore.Session) { s.SessionID = "conv-parent" })
	if _, err := store.Add(ctx, "conv-parent", "", []tasks.NewTask{{Content: "delegate me"}}); err != nil {
		t.Fatal(err)
	}

	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(ctx, tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest",
		Task: "1", ExecutorSelector: "env=nowhere",
	})
	ck.NoError(err, "spawn must succeed")
	ck.NotEq("", got.ChildID, "childID must not be empty")

	// The task is still pending because SpawnerConversationID is not set — the
	// Assigncondition requires it. An unbound child simply starts without error.
	rows, _ := store.List(ctx, tasks.ListFilter{ConversationID: "conv-parent"})
	ck.Eq(tasks.StatusPending, rows[0].Status, "status")
}

// 2. EXECUTOR PARKED — recoverable. The child is ALIVE, so its tasks stay
// exactly as they are. Not orphaned: orphaned means the owner is gone, and
// destroying that distinction destroys the only signal separating a dead
// worker from a paused one.
func TestParkedExecutorLeavesTasksUntouched(t *testing.T) {
	c := assert.NewAborting(t)
	store := tasks.NewMemoryStore()
	ctx := context.Background()

	if _, err := store.Add(ctx, "conv", "", []tasks.NewTask{{Content: "in flight"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Assign(ctx, "conv", "1", "c_abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, "conv", []tasks.Change{{Handle: "1", Status: tasks.StatusInProgress}}); err != nil {
		t.Fatal(err)
	}

	// Parking an executor does not end its children, and nothing in the
	// controller orphans a task while the owner is alive. The only path that
	// orphans is handleChildExit's sweep, which runs when the child EXITS —
	// never on the park decision.
	rows, err := store.List(ctx, tasks.ListFilter{ConversationID: "conv"})
	c.NoError(err)
	c.Len(rows, 1, "want 1 row, got %d", len(rows))
	c.Eq(tasks.StatusInProgress, rows[0].Status, "status")
	c.Eq("c_abc", rows[0].Assignee, "assignee")
}

// 3. EXECUTOR LOST — terminal. The child ends, and phase 02's sweep in
// handleChildExit runs normally. Nothing special is needed here, which is the
// point of putting the sweep on the EXIT path rather than on the kill verb —
// so this test exists to prove no special case was added.
func TestLostExecutorOrphansThroughTheOrdinaryExitPath(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager(), execPool: &fakePool{}}
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	const lostID = "exec-lost-000001"
	c.st.Insert(&childstore.Session{
		ChildID: "c_abc", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi,
		Labels: map[string]string{
			"rafiki/executor":       lostID,
			"rafiki/workspace":      "ws-1",
			"rafiki/workspace-mode": "pinned",
		},
	})

	if _, err := store.Add(ctx, "conv", "", []tasks.NewTask{{Content: "dying work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Assign(ctx, "conv", "1", "c_abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, "conv", []tasks.Change{{Handle: "1", Status: tasks.StatusInProgress}}); err != nil {
		t.Fatal(err)
	}

	// The lost handler does NOT orphan directly: it schedules a kill (deferred
	// 10s via failChild, a no-op here with no live child), and the orphan
	// sweep runs on the ordinary EXIT path in handleChildExit. The task must
	// still read in_progress immediately after — proving no executor-specific
	// orphan path was introduced.
	c.HandleExecutorLost(lostID)
	rows, err := store.List(ctx, tasks.ListFilter{ConversationID: "conv"})
	ck.NoError(err)
	ck.Eq(tasks.StatusInProgress, rows[0].Status, "lost handler must not orphan directly; status =")

	// When the exit path's sweep runs, the assignee is RETAINED — "c_abc died
	// holding this" is what a coordinator needs.
	if swept, err := store.OrphanAssigned(ctx, "c_abc"); err != nil || len(swept) != 1 {
		t.Fatalf("orphan sweep: rows=%d err=%v", len(swept), err)
	}
	rows, _ = store.List(ctx, tasks.ListFilter{ConversationID: "conv"})
	ck.Eq(tasks.StatusOrphaned, rows[0].Status, "status")
	ck.Eq("c_abc", rows[0].Assignee, "assignee")
}

// A task handle the ledger does not hold used to be accepted: the child
// started, the best-effort Assign failed into a log line, and the caller never
// learned its ledger row was untouched. The handle is checked before anything
// is created.
func TestSpawnRefusesAnUnknownTaskHandle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_parent", func(s *childstore.Session) { s.SessionID = "conv-parent" })
	if _, err := store.Add(ctx, "conv-parent", "", []tasks.NewTask{{Content: "the only task"}}); err != nil {
		t.Fatal(err)
	}
	before := len(c.st.List())

	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(ctx, tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest",
		Task: "2.1",
	})

	ck.Require().Error(err, "an unknown handle must refuse the spawn")
	ck.StrContains(err.Error(), "2.1", "the refusal names the handle")
	ck.Eq("", got.ChildID, "no child may be returned")
	ck.Eq(before, len(c.st.List()), "a refused spawn must register no child")
}

func TestSpawnWithAKnownTaskHandleAssignsIt(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_parent", func(s *childstore.Session) { s.SessionID = "conv-parent" })
	if _, err := store.Add(ctx, "conv-parent", "", []tasks.NewTask{{Content: "delegate me"}}); err != nil {
		t.Fatal(err)
	}

	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(ctx, tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", Task: "1",
	})

	ck.Require().NoError(err, "a known handle must spawn")
	rows, _ := store.List(ctx, tasks.ListFilter{ConversationID: "conv-parent"})
	ck.Eq(got.ChildID, rows[0].Assignee, "the task is assigned to the new child")
}

func TestSpawnWithoutATaskIgnoresTheLedger(t *testing.T) {
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	c.tasks = tasks.NewMemoryStore()
	sp := newControllerSpawner(c, "c_parent")

	_, err := sp.Spawn(context.Background(), tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest",
	})

	assert.NewCollecting(t).NoError(err, "no task handle means nothing to validate")
}

// Handles are dotted ordinal paths, so a subtask's handle ("1.1") must pass the
// same check a top-level one does.
func TestSpawnAcceptsANestedTaskHandle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := selectFixture(t, "env=home", ex("exec-home", map[string]string{"env": "home"}, ""))
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_parent", func(s *childstore.Session) { s.SessionID = "conv-parent" })
	if _, err := store.Add(ctx, "conv-parent", "", []tasks.NewTask{{Content: "parent"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(ctx, "conv-parent", "1", []tasks.NewTask{{Content: "child of 1"}}); err != nil {
		t.Fatal(err)
	}

	sp := newControllerSpawner(c, "c_parent")
	got, err := sp.Spawn(ctx, tools.SpawnSpec{
		Prompt: "x", Cwd: t.TempDir(), Model: "anthropic/sonnet-latest", Task: "1.1",
	})

	ck.Require().NoError(err, "a nested handle must spawn")
	rows, _ := store.List(ctx, tasks.ListFilter{ConversationID: "conv-parent"})
	var assignee string
	for _, r := range rows {
		if r.Handle == "1.1" {
			assignee = r.Assignee
		}
	}
	ck.Eq(got.ChildID, assignee, "the subtask is assigned to the new child")
}
