package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorinstall"
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

// A store error is an ERROR, never "not owned" and never an empty set: reading
// it as "no sandbox" would let a sandboxed child rebind onto an ordinary host
// the operator never offered it.
func TestReachableSandboxStoreErrorIsAnError(t *testing.T) {
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{nativeExec("exec-a", "a", owner.UserID)}
	env.store.listErr = errors.New("sandbox store is down")
	b := env.ctrl.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: owner.UserID})

	got, err := b.Reachable("c1")
	ck.Error(err, "a store error must be reported, not swallowed")
	ck.StrContains(err.Error(), "sandbox store is down", "the store's error must reach the caller; got %q", err)
	ck.Empty(got, "and never a fall-through to the ordinary executors")
}

// --- Reachable: executors installed over ssh --------------------------------

// installStoreWith is an install store holding one LIVE recipe per executor id,
// all owned by ownerUserID.
func installStoreWith(t *testing.T, ownerUserID string, executorIDs ...string) *fakeInstallStore {
	t.Helper()
	installs := newFakeInstallStore()
	for i, id := range executorIDs {
		if err := installs.Insert(context.Background(), executorinstall.Row{
			ID:                 fmt.Sprintf("recipe-%d", i+1),
			OwnerUserID:        ownerUserID,
			ExecutorID:         id,
			LauncherExecutorID: "exec-launcher",
			Target:             "someone@" + id,
			Instance:           "work",
		}); err != nil {
			t.Fatalf("seed install row for %s: %v", id, err)
		}
	}
	return installs
}

// setInstaller records which child asked for an executor's install.
func setInstaller(t *testing.T, installs *fakeInstallStore, executorID, childID string) {
	t.Helper()
	installs.mu.Lock()
	defer installs.mu.Unlock()
	for id, row := range installs.rows {
		if row.ExecutorID == executorID {
			row.InstalledByChild = childID
			installs.rows[id] = row
		}
	}
}

// reachController is a Controller wired with exactly what the reachable set
// reads: a fake pool, a fake executor store and an install store.
func reachController(ownerUserID string, live []execpool.LiveExecutor, rows map[string]executors.Executor, installs *fakeInstallStore) *Controller {
	execs := newFakeExecStore()
	for id, e := range rows {
		execs.execs[id] = e
	}
	return &Controller{
		st:        childstore.New(),
		execPool:  &fakePool{live: live},
		execStore: execs,
		install:   newInstallState(installs, nil),
	}
}

// installedExecRow is the DURABLE row of an executor installed over ssh: its
// facts are the daemon's, written at Create.
func installedExecRow(id, machine, ownerUserID string) executors.Executor {
	return executors.Executor{
		ID: id, Enabled: true, OwnerUserID: ownerUserID,
		Labels: map[string]string{"machine": machine},
	}
}

// insertChildRow stores a child with a parent link, for the lineage walks.
func insertChildRow(c *Controller, id, parent, ownerUserID string) {
	labels := map[string]string{"owner": "u"}
	if parent != "" {
		labels[childstore.LabelParent] = parent
		labels[childstore.LabelRoot] = parent
	}
	c.st.Insert(&childstore.Session{
		ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(),
		OwnerUserID: ownerUserID, Labels: labels,
	})
}

// reachIDs is the ids of a reachable set, in its order.
func reachIDs(set []executors.Executor) []string {
	out := make([]string, 0, len(set))
	for _, e := range set {
		out = append(out, e.ID)
	}
	return out
}

// An executor installed over ssh that is NOT live is still reachable: its row
// is durable, and the call is what relaunches it. Without this a down installed
// machine would need a reinstall or a reconnect before anyone could name it.
func TestReachableIncludesOfflineInstalledRow(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "exec-ublk")
	c := reachController("u1",
		[]execpool.LiveExecutor{nativeExec("exec-grey", "greyshift", "u1")},
		map[string]executors.Executor{"exec-ublk": installedExecRow("exec-ublk", "ublk-s2", "u1")},
		installs,
	)
	b := c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"})

	got, err := b.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	ck.EqDeep([]string{"exec-grey", "exec-ublk"}, reachIDs(got),
		"the down installed executor must be reachable alongside the live one; got %v", reachIDs(got))
	ck.False(b.IsLive("exec-ublk"),
		"it is reachable because of its row, NOT because it is live — the call relaunches it")
}

// Reachability never widens: an offline installed row is narrowed exactly as a
// live one is — by the spawn's selector, by the executor row's owner, and by
// the install row's owner.
func TestReachableOfflineInstalledStillNarrowed(t *testing.T) {
	t.Run("a selector that does not match excludes it", func(t *testing.T) {
		ck := assert.NewAborting(t)
		installs := installStoreWith(t, "u1", "exec-ublk")
		c := reachController("u1",
			[]execpool.LiveExecutor{nativeExec("exec-grey", "greyshift", "u1")},
			map[string]executors.Executor{"exec-ublk": installedExecRow("exec-ublk", "ublk-s2", "u1")},
			installs,
		)
		b := c.binderFor(protocol.SpawnRequest{ExecutorSelector: "machine=greyshift"},
			executorOwner{Name: "u", UserID: "u1"})

		got, err := b.Reachable("c1")
		ck.Require().NoError(err, "Reachable")
		ck.EqDeep([]string{"exec-grey"}, reachIDs(got),
			"an installed executor the spawn's selector excludes is not reachable; got %v", reachIDs(got))
	})

	t.Run("another owner's install row is never read", func(t *testing.T) {
		ck := assert.NewAborting(t)
		// The recipe belongs to u2; the executor row would otherwise match u1.
		installs := installStoreWith(t, "u2", "exec-ublk")
		c := reachController("u1",
			[]execpool.LiveExecutor{nativeExec("exec-grey", "greyshift", "u1")},
			map[string]executors.Executor{"exec-ublk": installedExecRow("exec-ublk", "ublk-s2", "u1")},
			installs,
		)
		b := c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"})

		got, err := b.Reachable("c1")
		ck.Require().NoError(err, "Reachable")
		ck.EqDeep([]string{"exec-grey"}, reachIDs(got),
			"ListLive is an EXACT owner match, so a foreign recipe adds nothing; got %v", reachIDs(got))
	})

	t.Run("an installed executor owned by another user is excluded", func(t *testing.T) {
		ck := assert.NewAborting(t)
		installs := installStoreWith(t, "u1", "exec-ublk")
		c := reachController("u1",
			[]execpool.LiveExecutor{nativeExec("exec-grey", "greyshift", "u1")},
			// The recipe is ours; the executor row is not.
			map[string]executors.Executor{"exec-ublk": installedExecRow("exec-ublk", "ublk-s2", "u2")},
			installs,
		)
		b := c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"})

		got, err := b.Reachable("c1")
		ck.Require().NoError(err, "Reachable")
		ck.EqDeep([]string{"exec-grey"}, reachIDs(got),
			"the ownership rule applies to a down installed row too; got %v", reachIDs(got))
	})

	t.Run("a workspace mode the row does not offer excludes it", func(t *testing.T) {
		ck := assert.NewAborting(t)
		installs := installStoreWith(t, "u1", "exec-ublk")
		row := installedExecRow("exec-ublk", "ublk-s2", "u1")
		row.WorkspaceMode = "pinned"
		c := reachController("u1",
			[]execpool.LiveExecutor{nativeExec("exec-grey", "greyshift", "u1")},
			map[string]executors.Executor{"exec-ublk": row},
			installs,
		)
		b := c.binderFor(protocol.SpawnRequest{WorkspaceMode: "ephemeral"},
			executorOwner{Name: "u", UserID: "u1"})

		got, err := b.Reachable("c1")
		ck.Require().NoError(err, "Reachable")
		ck.Empty(got, "a pinned installed executor cannot serve an ephemeral spawn; got %v", reachIDs(got))
	})
}

// The installer's own grant: an executor installed by a child's ANCESTOR is
// reachable to that child whatever the child's selector says, and to no other
// tree. Authority flows down.
func TestReachableInstallerLineageBypassesSelector(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "exec-ublk")
	c := reachController("u1",
		[]execpool.LiveExecutor{
			nativeExec("exec-grey", "greyshift", "u1"),
			// Live, so this is an ordinary candidate the SELECTOR drops — which
			// is what makes the second rule's bypass observable.
			nativeExec("exec-ublk", "ublk-s2", "u1"),
		},
		map[string]executors.Executor{"exec-ublk": installedExecRow("exec-ublk", "ublk-s2", "u1")},
		installs,
	)
	insertChildRow(c, "c_root", "", "u1")
	insertChildRow(c, "c_mid", "c_root", "u1")
	insertChildRow(c, "c_other", "c_root", "u1")
	insertChildRow(c, "c_otherkid", "c_other", "u1")
	// The grandparent installed it.
	setInstaller(t, installs, "exec-ublk", "c_mid")

	b := c.binderFor(protocol.SpawnRequest{ParentChildID: "c_mid", ExecutorSelector: "machine=greyshift"},
		executorOwner{Name: "u", UserID: "u1"})
	got, err := b.Reachable("c_gc")
	ck.Require().NoError(err, "Reachable")
	ck.EqDeep([]string{"exec-grey", "exec-ublk"}, reachIDs(got),
		"the installer's grandchild reaches it even though its selector excludes it; got %v", reachIDs(got))

	// A sibling tree under the same root: the installer is not on its chain.
	other := c.binderFor(protocol.SpawnRequest{ParentChildID: "c_other", ExecutorSelector: "machine=greyshift"},
		executorOwner{Name: "u", UserID: "u1"})
	got, err = other.Reachable("c_otherkid")
	ck.Require().NoError(err, "Reachable")
	ck.EqDeep([]string{"exec-grey"}, reachIDs(got),
		"a sibling tree must not inherit the installer's grant; got %v", reachIDs(got))

	// An executor BOTH rules reach appears ONCE: a duplicate would make one
	// machine look like two and turn every implicit call into "pass executor".
	both := c.binderFor(protocol.SpawnRequest{ParentChildID: "c_mid"},
		executorOwner{Name: "u", UserID: "u1"})
	got, err = both.Reachable("c_gc")
	ck.Require().NoError(err, "Reachable")
	ck.EqDeep([]string{"exec-grey", "exec-ublk"}, reachIDs(got),
		"a live installed executor on the installer's chain must not be listed twice; got %v", reachIDs(got))
}

// The installer's grant licenses a SELECTOR bypass and nothing else. An
// executor's Admits selector and the workspace mode its row offers are
// confinement facts — who may use the machine at all — so a descendant of the
// installer does not get past them by being a descendant. Every case here uses
// an OFFLINE installed row and a request selector the row cannot match, so rule
// 1's pipeline rejects it on the same grounds and rule 2 is the only path that
// could let it in: whatever is reachable here is rule 2's doing.
func TestReachableInstallerGrantAppliesConfinement(t *testing.T) {
	// lineageGrantEnv is one offline installed executor, installed by c_mid, and
	// a request from c_mid's child that no selector can match the row with.
	lineageGrantEnv := func(t *testing.T, row executors.Executor, req protocol.SpawnRequest) executorBinder {
		t.Helper()
		installs := installStoreWith(t, "u1", "exec-ublk")
		c := reachController("u1", nil,
			map[string]executors.Executor{"exec-ublk": row}, installs)
		insertChildRow(c, "c_mid", "", "u1")
		setInstaller(t, installs, "exec-ublk", "c_mid")
		if req.ParentChildID == "" {
			req.ParentChildID = "c_mid"
		}
		if req.ExecutorSelector == "" {
			req.ExecutorSelector = "machine=greyshift"
		}
		return c.binderFor(req, executorOwner{Name: "u", UserID: "u1"})
	}

	t.Run("a workspace mode the row does not offer excludes it", func(t *testing.T) {
		ck := assert.NewAborting(t)
		row := installedExecRow("exec-ublk", "ublk-s2", "u1")
		row.WorkspaceMode = "pinned"
		b := lineageGrantEnv(t, row, protocol.SpawnRequest{WorkspaceMode: "ephemeral"})

		got, err := b.Reachable("c_gc")
		ck.Require().NoError(err, "Reachable")
		ck.Empty(got, "an ephemeral spawn must not be handed a pinned executor through lineage; got %v", reachIDs(got))
	})

	t.Run("an Admits selector that refuses the child excludes it", func(t *testing.T) {
		ck := assert.NewAborting(t)
		row := installedExecRow("exec-ublk", "ublk-s2", "u1")
		row.Admits = "owner=someone-else"
		b := lineageGrantEnv(t, row, protocol.SpawnRequest{})

		got, err := b.Reachable("c_gc")
		ck.Require().NoError(err, "Reachable")
		ck.Empty(got, "lineage must not walk a child past the row's own admission selector; got %v", reachIDs(got))
	})

	t.Run("the selector bypass survives when confinement allows it", func(t *testing.T) {
		ck := assert.NewAborting(t)
		row := installedExecRow("exec-ublk", "ublk-s2", "u1")
		row.Admits = "owner=u"
		row.WorkspaceMode = "pinned"
		b := lineageGrantEnv(t, row, protocol.SpawnRequest{WorkspaceMode: "pinned"})

		got, err := b.Reachable("c_gc")
		ck.Require().NoError(err, "Reachable")
		ck.EqDeep([]string{"exec-ublk"}, reachIDs(got),
			"the installer's grant still bypasses the SELECTOR — confinement alone must not refuse it; got %v", reachIDs(got))
	})

	t.Run("an unrequested workspace mode constrains nothing", func(t *testing.T) {
		ck := assert.NewAborting(t)
		row := installedExecRow("exec-ublk", "ublk-s2", "u1")
		row.Admits = "owner=u"
		row.WorkspaceMode = "pinned"
		b := lineageGrantEnv(t, row, protocol.SpawnRequest{})

		got, err := b.Reachable("c_gc")
		ck.Require().NoError(err, "Reachable")
		ck.EqDeep([]string{"exec-ublk"}, reachIDs(got),
			"a request that asks for no mode is offered any mode, as narrowByWorkspaceMode says; got %v", reachIDs(got))
	})
}

// A sandbox-owned child returns its sandbox and nothing else, installed rows
// included: the same exclusivity that keeps it off ordinary hosts.
func TestReachableSandboxOwnedChildSeesNoInstalled(t *testing.T) {
	ck := assert.NewAborting(t)
	env := newSandboxEnv(t)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{
		spawnedExecutor("exec-sbx", owner.UserID),
		nativeExec("exec-grey", "greyshift", owner.UserID),
	}
	insertSandboxRow(env, sandbox.Row{
		ID: "sbx-1", Name: "box", OwnerChild: "c1", OwnerUserID: owner.UserID,
		ExecutorID: "exec-sbx", State: sandboxStateReady, Scope: protocol.ScopeSelf,
	})
	env.exec.execs["exec-ublk"] = installedExecRow("exec-ublk", "ublk-s2", owner.UserID)
	env.ctrl.install = newInstallState(installStoreWith(t, owner.UserID, "exec-ublk"), nil)

	b := env.ctrl.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: owner.UserID})
	got, err := b.Reachable("c1")
	ck.Require().NoError(err, "Reachable")
	ck.EqDeep([]string{"exec-sbx"}, reachIDs(got),
		"an owned sandbox is the only reachable executor, installed rows included; got %v", reachIDs(got))
}

// A down installed executor is a machine the child can name, so it counts when
// an implicit call has to decide it is ambiguous: silently calling the one live
// machine instead is how work lands on a host the caller did not choose.
func TestBoundExecutorAmbiguityCountsOfflineInstalled(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "exec-ublk")
	c := reachController("u1",
		[]execpool.LiveExecutor{nativeExec("exec-grey", "greyshift", "u1")},
		map[string]executors.Executor{"exec-ublk": installedExecRow("exec-ublk", "ublk-s2", "u1")},
		installs,
	)
	be := newBoundExecutor("c1", c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"}))

	_, err := be.Execute(context.Background(), "read", nil)
	ck.Require().Error(err, "two reachable executors, one of them down, is still ambiguous")
	ck.StrContains(err.Error(), "this agent can reach 2 executors", "the down installed one counts: %v", err)
	ck.StrContains(err.Error(), "pass executor", "the model must be told to name one: %v", err)
	ck.Len(c.execPool.(*fakePool).provisionCalls(), 0, "nothing may be provisioned for an ambiguous call")
}
