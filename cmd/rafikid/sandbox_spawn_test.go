package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"

	"github.com/multigres/testkit/assert"
)

// newSandboxSpawnEnv is newSandboxEnv plus the wiring Controller.Spawn needs:
// a child manager, the native event bus, and a state dir for the child's spill
// files. Sandbox creation itself needs only the base fixture.
func newSandboxSpawnEnv(t *testing.T) *sandboxEnv {
	t.Helper()
	env := newSandboxEnv(t)
	env.ctrl.cm = newChildManager()
	env.ctrl.native = nativebus.New()
	env.ctrl.stateDir = t.TempDir()
	return env
}

// sandboxSeedParent inserts a live parent child that is permitted to spawn
// (depth and children grants above zero), which sandboxSeedChild is not.
func sandboxSeedParent(env *sandboxEnv, id string) {
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, MaxDepth: 1, MaxChildren: 8,
		Labels: map[string]string{},
	})
}

// spawnBlockSpec is the minimal spawn-block spec: an image and a scope (the
// scope is required for a spawn block — the zero value is refused).
func spawnBlockSpec() *protocol.SandboxSpec {
	return &protocol.SandboxSpec{Image: "rafiki/sandbox:test", Scope: protocol.ScopeSelf}
}

// insertSandboxRow stores a sandbox row directly, for the binder-level tests
// that do not run the whole create flow.
func insertSandboxRow(env *sandboxEnv, row sandbox.Row) {
	if err := env.ctrl.sandboxStore.Insert(context.Background(), row); err != nil {
		panic(err)
	}
}

// --- happy path and failure paths -------------------------------------------

// TestSandboxSpawnHappyPath: a spawn with a sandbox creates the block, the
// child owns it, its STORED selector pins the sandbox's machine (which confines
// its descendants through lineage narrowing), and the binder reaches it.
func TestSandboxSpawnHappyPath(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	sandboxSeedParent(env, "parent-1")
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	res, err := env.ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "parent-1",
		Sandbox:       spawnBlockSpec(),
	}, owner)
	ck.Require().NoError(err, "spawn")
	childID := res.ChildID

	row, ok, err := env.ctrl.ownedSandbox(context.Background(), childID, owner.UserID)
	ck.Require().NoError(err, "ownedSandbox")
	ck.True(ok, "the new child owns its spawn-block sandbox")
	ck.Eq(childID, row.OwnerChild, "owner child")
	// The row read back through the STORE carries its executor — this is what
	// makes the bind-by-ownership lookup (and removal) possible at all.
	ck.Eq("exec-created", row.ExecutorID, "the persisted row records its executor")

	snap, ok := env.ctrl.st.Get(childID)
	ck.Require().True(ok, "child stored")
	ck.Eq("machine="+sandboxSpawnMachineName(row.ID), snap.ExecutorSelector,
		"the stored selector pins the sandbox's machine")

	// The binder reaches the sandbox rather than falling back to selection.
	bound, err := env.ctrl.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor(childID)
	ck.NoError(err, "ChooseFor")
	ck.Eq("exec-created", bound, "bound executor")
}

// TestSandboxSpawnCreateFailureRefusesNoChildRow: a failed sandbox create
// refuses the spawn outright — the child never starts toolless or native — and
// leaves no child row behind.
func TestSandboxSpawnCreateFailureRefusesNoChildRow(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	sandboxSeedParent(env, "parent-1")
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.docker.createFail = true

	before := len(env.ctrl.st.List())
	_, err := env.ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "parent-1",
		Sandbox:       spawnBlockSpec(),
	}, owner)
	ck.Error(err, "a failed create must refuse the spawn")
	ck.Len(env.ctrl.st.List(), before, "no child row may be left behind")

	live, lerr := env.store.ListAllLive(context.Background())
	ck.NoError(lerr, "list live")
	ck.Len(live, 0, "the rolled-back sandbox is not live")
}

// TestSandboxSpawnFailureAfterCreateRemovesSandbox: once the sandbox exists, a
// later failure in the spawn (here, the runtime refuses --db) removes it rather
// than leaving a container and its cap slot to the reaper.
func TestSandboxSpawnFailureAfterCreateRemovesSandbox(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	sandboxSeedParent(env, "parent-1")
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	_, err := env.ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "parent-1",
		ExtraArgs:     []string{"--db", "x"},
		Sandbox:       spawnBlockSpec(),
	}, owner)
	ck.Error(err, "the runtime must refuse --db")
	ck.True(strings.Contains(err.Error(), "--db"), "error names --db: %v", err)

	live, lerr := env.store.ListAllLive(context.Background())
	ck.NoError(lerr, "list live")
	ck.Len(live, 0, "the sandbox is removed after the failed spawn")
}

// TestSandboxSpawnNonFundiKindRefused: claude and script children are launched,
// not workspace-bound, so a sandbox on one is refused before it is created.
func TestSandboxSpawnNonFundiKindRefused(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()

	_, err := env.ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:    protocol.KindClaude,
		Cwd:     t.TempDir(),
		Sandbox: spawnBlockSpec(),
	}, owner)
	ck.Error(err, "a sandbox on a claude child must be refused")
	ck.True(strings.Contains(err.Error(), "kind=fundi"), "error names the supported kind: %v", err)

	live, lerr := env.store.ListAllLive(context.Background())
	ck.NoError(lerr, "list live")
	ck.Len(live, 0, "no sandbox is created for a refused kind")
}

// --- persisted executor_id and its downstream effects -----------------------

// TestSandboxSpawnCreatedRowPersistsExecutorID: a create leaves a PERSISTED row
// (read back through the store, not the in-memory info) whose executor_id is the
// executor the flow minted. An empty executor_id breaks the bind-by-ownership
// lookup and leaks the executor row on removal.
func TestSandboxSpawnCreatedRowPersistsExecutorID(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	info, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Require().NoError(err, "create")

	row, ok := env.store.get(info.ID)
	ck.Require().True(ok, "row stored")
	ck.NotEq("", row.ExecutorID, "executor_id must be persisted, not just set in memory")
	ck.Eq("exec-created", row.ExecutorID, "persisted executor_id")
	ck.Eq(sandboxStateReady, row.State, "state")
}

// TestSandboxSpawnCreatedSandboxRemovesExecutor: because the row now records its
// executor, removeSandboxRow actually evicts and deletes the executor row rather
// than leaking it.
func TestSandboxSpawnCreatedSandboxRemovesExecutor(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Require().NoError(err, "create")
	ck.Require().NoError(env.ctrl.SandboxRemove(context.Background(), owner, "", "dev"), "remove")

	ck.True(env.pool.evicted["exec-created"], "the executor was evicted")
	ck.True(sandboxHas(env.exec.deleted, "exec-created"), "the executor row was deleted: %v", env.exec.deleted)
}

// TestSandboxSpawnCreatedSandboxReportsConnected: SandboxList reports a live
// sandbox as connected, which it can only do from the persisted executor_id.
func TestSandboxSpawnCreatedSandboxReportsConnected(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "", sandboxSpec("dev"))
	ck.Require().NoError(err, "create")

	infos, err := env.ctrl.SandboxList(owner)
	ck.Require().NoError(err, "list")
	ck.Len(infos, 1, "one sandbox")
	ck.True(infos[0].Connected, "a live sandbox reports connected")
	ck.Eq("exec-created", infos[0].ExecutorID, "the reported executor")
	ck.Eq(sandboxStateReady, infos[0].State, "a connected ready row stays ready")
}

// --- Close / Kill -----------------------------------------------------------

// TestSandboxSpawnCloseRemovesSandbox: closing a child removes its spawn-block
// sandbox (its container, executor row and row tombstone), promptly rather than
// waiting for the reaper.
func TestSandboxSpawnCloseRemovesSandbox(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusExited, StartedAt: time.Now(),
		Kind: protocol.KindFundi, Labels: map[string]string{},
	})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateReady,
	})

	ck.NoError(env.ctrl.Close("c1"), "close")
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt != nil, "the closed child's sandbox is removed")
	ck.True(sandboxHas(env.docker.removed, "ctr-1"), "its container is removed: %v", env.docker.removed)
}

// TestSandboxSpawnCloseFailedRemovalStillCloses: a sandbox whose launcher is
// offline cannot be torn down now, but that must NEVER fail the close — the
// reaper finishes it later.
func TestSandboxSpawnCloseFailedRemovalStillCloses(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	// The launcher's row still exists (it may come back), so removal is left
	// `removing` and reports an error — which the close must swallow.
	env.exec.execs["launcher"] = executors.Executor{ID: "launcher", Enabled: true}
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusExited, StartedAt: time.Now(),
		Kind: protocol.KindFundi, Labels: map[string]string{},
	})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-created", LauncherExecutorID: "launcher", ContainerID: "ctr-1",
		State: sandboxStateReady,
	})

	ck.NoError(env.ctrl.Close("c1"), "a failed sandbox removal must not fail the close")
	row, _ := env.store.get("sbx-1")
	ck.True(row.RemovedAt == nil, "the row is left for the reaper")
	ck.Eq(sandboxStateRemoving, row.State, "left removing")
}

// TestSandboxSpawnCloseDoesNotCascadeToDescendants: closing a parent removes its
// OWN spawn-block sandbox but must NOT touch a still-live descendant's own
// sandbox (whose workspace it may be actively using). Teardown is keyed on the
// owning child; the reaper covers a sandbox once its own child's row closes.
func TestSandboxSpawnCloseDoesNotCascadeToDescendants(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	// parent (exited, closing) -> child (still live).
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "parent", Status: protocol.StatusExited, StartedAt: time.Now(),
		Kind: protocol.KindFundi, Labels: map[string]string{},
	})
	sandboxSeedChild(env, "child", "parent")
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-parent", OwnerUserID: owner.UserID, OwnerChild: "parent", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-p", LauncherExecutorID: "launcher", ContainerID: "ctr-p", State: sandboxStateReady,
	})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-child", OwnerUserID: owner.UserID, OwnerChild: "child", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-c", LauncherExecutorID: "launcher", ContainerID: "ctr-c", State: sandboxStateReady,
	})

	ck.NoError(env.ctrl.Close("parent"), "close")

	prow, _ := env.store.get("sbx-parent")
	ck.True(prow.RemovedAt != nil, "the closing child's own sandbox is removed")
	crow, _ := env.store.get("sbx-child")
	ck.True(crow.RemovedAt == nil, "a still-live descendant's own sandbox must survive its ancestor's close")
	ck.False(sandboxHas(env.docker.removed, "ctr-c"), "the descendant's container is kept: %v", env.docker.removed)
}

// TestSandboxSpawnRemovingOrLostSandboxDoesNotBind: only a `ready` row is a fully
// live sandbox. A `removing` (being torn down) or `lost` (no container) row whose
// executor is still connected must take the not-connected error path, never bind
// and never fall through to ordinary selection.
func TestSandboxSpawnRemovingOrLostSandboxDoesNotBind(t *testing.T) {
	t.Parallel()
	for _, state := range []string{sandboxStateRemoving, sandboxStateLost} {
		t.Run(state, func(t *testing.T) {
			ck := assert.NewAborting(t)
			env := newSandboxSpawnEnv(t)
			owner := sandboxOwner()
			env.pool.live = []execpool.LiveExecutor{ownedSandboxEx("exec-sbx", "c1", "sbx-1", owner.UserID)}
			insertSandboxRow(env, sandbox.Row{
				ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c1", Scope: protocol.ScopeSelf,
				ExecutorID: "exec-sbx", State: state,
			})

			got, err := env.ctrl.binderFor(protocol.SpawnRequest{},
				executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor("c1")
			ck.Error(err, "a %s sandbox must not bind", state)
			ck.True(strings.Contains(err.Error(), "not connected"), "error text: %v", err)
			ck.Eq("", got, "must not bind")
		})
	}
}

// TestSandboxSpawnKillLeavesSandbox: a killed child keeps its sandbox — a killed
// child can resume, and its workspace must be there if it does.
func TestSandboxSpawnKillLeavesSandbox(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	sandboxSeedParent(env, "parent-1")
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
	}

	res, err := env.ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Model:         "anthropic/sonnet-latest",
		Cwd:           t.TempDir(),
		ParentChildID: "parent-1",
		Sandbox:       spawnBlockSpec(),
	}, owner)
	ck.Require().NoError(err, "spawn")
	childID := res.ChildID

	_, err = env.ctrl.Kill(context.Background(), childID, 5*time.Second, 2*time.Second)
	ck.NoError(err, "kill")

	_, ok, oerr := env.ctrl.ownedSandbox(context.Background(), childID, owner.UserID)
	ck.NoError(oerr, "ownedSandbox")
	ck.True(ok, "a killed child keeps its sandbox")
}

// --- binding ----------------------------------------------------------------

// TestSandboxSpawnOwnerNeverFallsBackToAnOrdinaryExecutor: a child whose sandbox
// executor is not connected gets the not-connected error and does NOT bind to an
// ordinary executor, even when one is available.
func TestSandboxSpawnOwnerNeverFallsBackToAnOrdinaryExecutor(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	// An ordinary executor the fallback WOULD have chosen.
	env.pool.live = []execpool.LiveExecutor{
		exOwned("exec-ordinary", map[string]string{"machine": "box"}, "", owner.UserID),
	}
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})

	got, err := env.ctrl.binderFor(protocol.SpawnRequest{ExecutorSelector: "machine=box"},
		executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor("c1")
	ck.Error(err, "a sandboxed child with no live sandbox executor must fail")
	ck.True(strings.Contains(err.Error(), "not connected"), "error text: %v", err)
	ck.Eq("", got, "must not bind to the ordinary executor")
}

// TestSandboxSpawnSelfScopeDescendantUnbound: a self-scoped block does not own a
// descendant, and ordinary selection never reaches a child-owned sandbox — so a
// descendant with the sandbox's machine selector binds to nothing.
func TestSandboxSpawnSelfScopeDescendantUnbound(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	seedLineage(env, "a", "b")
	env.pool.live = []execpool.LiveExecutor{ownedSandboxEx("exec-sbx", "a", "sbx-1", owner.UserID)}
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "a", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})

	got, err := env.ctrl.binderFor(protocol.SpawnRequest{ExecutorSelector: "machine=sbx-1"},
		executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor("b")
	ck.Error(err, "a self-scoped block does not own the descendant")
	ck.Eq("", got, "the descendant is unbound, not bound to the sandbox")
}

// TestSandboxSpawnSubtreeDescendantBinds: a subtree-scoped block owns a
// descendant, which binds to the SAME sandbox through ChooseFor.
func TestSandboxSpawnSubtreeDescendantBinds(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	seedLineage(env, "a", "b")
	env.pool.live = []execpool.LiveExecutor{ownedSandboxEx("exec-sbx", "a", "sbx-1", owner.UserID)}
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "a", Scope: protocol.ScopeSubtree,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})

	got, err := env.ctrl.binderFor(protocol.SpawnRequest{ExecutorSelector: "machine=sbx-1"},
		executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor("b")
	ck.NoError(err, "ChooseFor")
	ck.Eq("exec-sbx", got, "the descendant binds to the same sandbox")
}

// TestSandboxSpawnUnrelatedChildCannotReach: a child outside the block's subtree
// neither owns it nor reaches it by selector.
func TestSandboxSpawnUnrelatedChildCannotReach(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	sandboxSeedChild(env, "a", "")
	sandboxSeedChild(env, "sib", "")
	env.pool.live = []execpool.LiveExecutor{ownedSandboxEx("exec-sbx", "a", "sbx-1", owner.UserID)}
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "a", Scope: protocol.ScopeSubtree,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})

	got, err := env.ctrl.binderFor(protocol.SpawnRequest{ExecutorSelector: "machine=sbx-1"},
		executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor("sib")
	ck.Error(err, "an unrelated child must not reach the sandbox")
	ck.Eq("", got, "unbound")
}

// TestSandboxSpawnResumeRebindsThroughChooseFor: resume re-runs the same
// ChooseFor the eager bind uses, so a resumed sandboxed child returns to its
// sandbox from its STORED selector.
func TestSandboxSpawnResumeRebindsThroughChooseFor(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{ownedSandboxEx("exec-sbx", "c1", "sbx-1", owner.UserID)}
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusExited, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: "machine=sbx-1", Labels: map[string]string{},
	})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})

	got, err := env.ctrl.binderFor(protocol.SpawnRequest{ExecutorSelector: "machine=sbx-1"},
		executorOwner{Name: "u", UserID: owner.UserID}).ChooseFor("c1")
	ck.NoError(err, "ChooseFor")
	ck.Eq("exec-sbx", got, "the resumed child returns to its sandbox")
}

// --- no nesting, and executor rm --------------------------------------------

// TestSandboxSpawnNoNestedSandbox: a sandboxed child's effective set is its
// sandbox, which advertises no docker proxy — so it cannot create another.
func TestSandboxSpawnNoNestedSandbox(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{ownedSandboxEx("exec-sbx", "c1", "sbx-1", owner.UserID)}
	env.ctrl.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: "machine=sbx-1", Labels: map[string]string{},
	})
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, OwnerChild: "c1", Scope: protocol.ScopeSelf,
		ExecutorID: "exec-sbx", State: sandboxStateReady,
	})

	_, err := env.ctrl.SandboxCreate(context.Background(), owner, "c1", sandboxSpec("inner"))
	ck.Error(err, "a sandboxed child cannot create a sandbox")
	ck.True(strings.Contains(err.Error(), "--proxy docker"), "error points at the fix: %v", err)
}

// TestSandboxSpawnExecutorRmRefused: deleting a sandbox's executor row directly
// is refused — the row alone would orphan a container that restarts forever.
func TestSandboxSpawnExecutorRmRefused(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	env.exec.execs["exec-sbx"] = executors.Executor{
		ID: "exec-sbx", Enabled: true,
		Labels: map[string]string{sandbox.RowLabelSandbox: "1", sandbox.RowLabelID: "sbx-1"},
	}

	err := env.ctrl.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: "exec-sbx"})
	ck.Error(err, "deleting a sandbox executor row must be refused")
	ck.True(strings.Contains(err.Error(), "rafiki sandbox rm"), "error points at the fix: %v", err)
	ck.True(strings.Contains(err.Error(), "sbx-1"), "error names the sandbox: %v", err)
	_, still := env.exec.execs["exec-sbx"]
	ck.True(still, "the row was not deleted")
}

// ownedSandboxEx is childOwnedSandboxEx with an owner stamped on, so it passes
// the binder's ownership check.
func ownedSandboxEx(id, ownerChild, machine, ownerUserID string) execpool.LiveExecutor {
	le := childOwnedSandboxEx(id, ownerChild, map[string]string{"machine": machine})
	le.Executor.OwnerUserID = ownerUserID
	return le
}
