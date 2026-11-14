package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"

	"github.com/multigres/testkit/assert"
)

// The bug this reproduces: recover() calls WorkspaceMode(prevExec) only in
// the branch reached after IsLive(prevExec) already came back false --
// pkg/execpool removes an executor from the live set BEFORE parking it, so by
// the time WorkspaceMode runs the executor is already gone from Live(). A
// lookup that (like executorRow) only scans Live() therefore always misses,
// permanently disabling ephemeral migration on the tool-call path. The
// executors.Store row is durable and must answer regardless of whether the
// executor is currently connected.
func TestWorkspaceModeReadsTheDurableRowWhenTheExecutorIsNotLive(t *testing.T) {
	fes := newFakeExecStore()
	fes.execs["exec-gone"] = executors.Executor{
		ID: "exec-gone", WorkspaceMode: "ephemeral", Enabled: true,
	}
	c := &Controller{
		execPool:  &fakePool{live: nil}, // NOT live -- this is the state recover() sees
		execStore: fes,
	}
	b := &controllerBinder{c: c}

	got := b.WorkspaceMode("exec-gone")
	assert.NewAborting(t).Eq("ephemeral", got, "WorkspaceMode = %q, want %q -- a mode lookup scoped to the "+
		"live pool can never see an executor recover() is already treating "+
		"as not-live, which makes ephemeral tool-call migration dead code", got, "ephemeral")
}

func TestWorkspaceModeFallsBackToPinnedWithNoExecStore(t *testing.T) {
	c := &Controller{execPool: &fakePool{live: nil}}
	b := &controllerBinder{c: c}
	assert.NewAborting(t).Eq("pinned", b.WorkspaceMode("exec-unknown"), "WorkspaceMode")
}

func TestWorkspaceModeFallsBackToPinnedOnStoreError(t *testing.T) {
	fes := newFakeExecStore()
	c := &Controller{execPool: &fakePool{live: nil}, execStore: fes}
	b := &controllerBinder{c: c}
	// exec-missing was never registered: fakeExecStore.Get answers ErrNotFound.
	assert.NewAborting(t).Eq("pinned", b.WorkspaceMode("exec-missing"), "WorkspaceMode")
}

// --- Reachable ---------------------------------------------------------------

// nativeExec is an ordinary live executor row for the reachable-set tests.
func nativeExec(id, machine, ownerUserID string) execpool.LiveExecutor {
	return execpool.LiveExecutor{Executor: executors.Executor{
		ID: id, Enabled: true, OwnerUserID: ownerUserID,
		Labels: map[string]string{"machine": machine},
	}}
}

// sandboxExec is a live executor row for a (non-child-owned) sandbox.
func sandboxExec(id, machine, ownerUserID string) execpool.LiveExecutor {
	return execpool.LiveExecutor{Executor: executors.Executor{
		ID: id, Enabled: true, OwnerUserID: ownerUserID,
		Labels: map[string]string{"machine": machine, sandbox.RowLabelSandbox: "1"},
	}}
}

// A spawn-block sandbox is EXCLUSIVE: the child reaches its sandbox and nothing
// else, and a sandbox that is not fully live is an ERROR rather than a chance to
// fall back onto a host the operator never offered it.
func TestReachableOwnedSandboxIsExclusive(t *testing.T) {
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		spawnedExecutor("exec-sbx", owner.UserID),
		nativeExec("exec-a", "a", owner.UserID),
		nativeExec("exec-b", "b", owner.UserID),
	}
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", Name: "box", OwnerChild: "c1", OwnerUserID: owner.UserID,
		ExecutorID: "exec-sbx", State: sandboxStateReady, Scope: protocol.ScopeSelf,
	})
	b := env.ctrl.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: owner.UserID})

	got, err := b.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	ck.Len(got, 1, "an owned sandbox is the ONLY reachable executor; got %+v", got)
	ck.Eq("exec-sbx", got[0].ID, "reachable[0]")

	// Owned but not ready: refuse, never fall through to the natives.
	if err := env.store.SetState(context.Background(), "sbx-1", sandboxStateCreating); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, err = b.Reachable("c1")
	ck.Error(err, "a sandbox that is not ready must refuse, not fall back")
	ck.Eq(`sandbox "box" is not connected`, err.Error(), "error text")
	ck.Empty(got, "nothing is reachable through a dead sandbox")
}

// An empty selector does not name a machine, so a sandbox must never be the
// implicit reachable set — the same rule chooseExecutor follows.
func TestReachableEmptySelectorDropsSandboxes(t *testing.T) {
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		nativeExec("exec-a", "a", owner.UserID),
		sandboxExec("exec-sbx", "box", owner.UserID),
	}
	b := env.ctrl.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: owner.UserID})

	got, err := b.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	ck.Len(got, 1, "the ordinary executor only; got %+v", got)
	ck.Eq("exec-a", got[0].ID, "reachable[0]")
}

// A ref bypasses SEARCH, never confinement: it narrows the same narrowed set to
// the one row it names, and a ref that names nothing reachable yields an empty
// set (the caller reports it) rather than widening anywhere.
func TestReachableExecutorRefNarrowsToOne(t *testing.T) {
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		nativeExec("exec-a", "a", owner.UserID),
		nativeExec("exec-b", "b", owner.UserID),
	}
	b := env.ctrl.binderFor(protocol.SpawnRequest{ExecutorRef: "b"},
		executorOwner{Name: "u", UserID: owner.UserID})

	got, err := b.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	ck.Len(got, 1, "a ref names one executor; got %+v", got)
	ck.Eq("exec-b", got[0].ID, "reachable[0]")

	// A ref to something no live row carries is empty, never "everything".
	b = env.ctrl.binderFor(protocol.SpawnRequest{ExecutorRef: "z"},
		executorOwner{Name: "u", UserID: owner.UserID})
	got, err = b.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	ck.Empty(got, "an unreachable ref reaches nothing")
}
