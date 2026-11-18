package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

const allSet = childstore.ExecutorSetAll

// setController is a Controller over a fake pool, wired far enough for
// Controller.Spawn to store a fundi child.
func setController(t *testing.T, live ...execpool.LiveExecutor) *Controller {
	t.Helper()
	c := &Controller{
		st: childstore.New(), cm: newChildManager(), native: nativebus.New(),
		execPool: &fakePool{live: live}, stateDir: t.TempDir(),
	}
	return c
}

// setParent stores a live parent that may spawn, holding set (nil is a legacy
// row).
func setParent(t *testing.T, c *Controller, id string, set []string) {
	t.Helper()
	c.st.Insert(&childstore.Session{
		ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, MaxDepth: 2, MaxChildren: 8,
		Labels:      map[string]string{},
		ExecutorSet: set,
	})
}

// spawnFundi spawns through the production path and returns the stored row.
func spawnFundi(t *testing.T, c *Controller, req protocol.SpawnRequest) (childstore.Snapshot, error) {
	t.Helper()
	req.Kind = protocol.KindFundi
	req.Model = "anthropic/sonnet-latest"
	req.Cwd = t.TempDir()
	res, err := c.Spawn(context.Background(), req, users.Identity{})
	if err != nil {
		return childstore.Snapshot{}, err
	}
	snap, ok := c.st.Get(res.ChildID)
	if !ok {
		t.Fatalf("spawned child %s is not stored", res.ChildID)
	}
	return snap, nil
}

func machineExec(id, machine string, extra map[string]string) execpool.LiveExecutor {
	labels := map[string]string{"machine": machine}
	for k, v := range extra {
		labels[k] = v
	}
	return ex(id, labels, "")
}

func TestSpawnDefaultsToTheParentsHome(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))
	setParent(t, c, "c_parent", []string{allSet})
	_, err := c.st.SetLabels("c_parent", map[string]string{homeExecutorLabel: "A"}, nil)
	ck.NoError(err)

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent"})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A"}, snap.ExecutorSet, "a child that names nothing keeps its spawner's home")
}

func TestSpawnDefaultSkipsAHomeThatCannotLaunchTheKind(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t,
		ex("A", map[string]string{"machine": "a"}, ""),
		exWithLaunch("B", map[string]string{"machine": "b"}, "", "claude"),
	)
	setParent(t, c, "c_parent", []string{"A", "B"})
	_, err := c.st.SetLabels("c_parent", map[string]string{homeExecutorLabel: "A"}, nil)
	ck.NoError(err)

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", Kind: protocol.KindClaude,
	}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A", "B"}, got.Executors, "a home that cannot launch the kind is not a single home")

	got, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", Kind: protocol.KindFundi,
	}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A"}, got.Executors, "a fundi child runs its tools wherever the home is")
}

func TestTopLevelSpawnDefaultsToAll(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{allSet}, snap.ExecutorSet, "a top-level child may address every executor of its owner")
}

func TestSpawnExecutorsMustBeInTheParentsSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "A", nil), machineExec("B", "B", nil))
	setParent(t, c, "c_parent", []string{"A"})

	_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", Executors: []string{"B"},
	}, executorOwner{})
	ck.Require().Error(err)
	ck.StrContains(err.Error(), `executor "B" is not in your executor set; yours: A`, "refusal text")
}

func TestSpawnExecutorsResolveNamesToIDs(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("id-a", "A", nil), machineExec("id-b", "B", nil))
	setParent(t, c, "c_parent", []string{allSet})

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", Executors: []string{"B", "A", "B"},
	}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"id-b", "id-a"}, got.Executors, "machine names become ids, in request order, once each")
}

func TestSpawnAllRequiresAnAllParent(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "A", nil), machineExec("B", "B", nil))
	setParent(t, c, "c_narrow", []string{"A", "B"})
	setParent(t, c, "c_all", []string{allSet})

	_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_narrow", Executors: []string{allSet},
	}, executorOwner{})
	ck.Require().Error(err)
	ck.StrContains(err.Error(), `executor set: "all" is available only to an agent whose own executor set is all`, "refusal text")

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_all", Executors: []string{allSet},
	}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{allSet}, got.Executors, "an all parent may hand down all")
}

func TestSpawnAllCannotBeCombined(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "A", nil))
	setParent(t, c, "c_parent", []string{allSet})

	_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", Executors: []string{allSet, "A"},
	}, executorOwner{})
	ck.Require().Error(err)
	ck.StrContains(err.Error(), `executor set: "all" cannot be combined with other names`, "refusal text")
}

func TestSpawnSelectorFreezesEveryMatch(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t,
		machineExec("B", "b", map[string]string{"gpu": "yes"}),
		machineExec("A", "a", map[string]string{"gpu": "yes"}),
		machineExec("C", "c", nil),
	)
	setParent(t, c, "c_parent", []string{allSet})

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorSelector: "gpu=yes",
	}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A", "B"}, got.Executors, "every match is stored, in sortCandidates order")
}

func TestSpawnRefOutsideTheParentsSetIsRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "A", nil), machineExec("B", "B", nil))
	setParent(t, c, "c_parent", []string{"A"})

	_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorRef: "B",
	}, executorOwner{})
	ck.Require().Error(err)
	ck.StrContains(err.Error(), `executor "B" is not in your executor set; yours: A`, "refusal text")

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", ExecutorRef: "A",
	}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A"}, got.Executors, "a ref inside the set stores that one id")
}

func TestSandboxSpawnAddressesOnlyItsSandbox(t *testing.T) {
	ck := assert.NewAborting(t)
	env := newSandboxSpawnEnv(t)
	owner := sandboxOwner()
	sandboxSeedParent(env, "parent-1")
	env.pool.live = []execpool.LiveExecutor{
		sandboxLauncher("launcher", "box", owner.UserID),
		spawnedExecutor("exec-created", owner.UserID),
		nativeExec("A", "a", owner.UserID),
	}

	res, err := env.ctrl.Spawn(context.Background(), protocol.SpawnRequest{
		Kind: protocol.KindFundi, Model: "anthropic/sonnet-latest", Cwd: t.TempDir(),
		ParentChildID: "parent-1", Sandbox: spawnBlockSpec(),
		Executors: []string{"A"},
	}, owner)
	ck.Require().NoError(err, "spawn")
	snap, ok := env.ctrl.st.Get(res.ChildID)
	ck.Require().True(ok, "child stored")
	ck.EqDeep([]string{"exec-created"}, snap.ExecutorSet, "a sandboxed child addresses its sandbox and nothing else")
}

// reachFor builds a binder for childID the way a running child has one: its row
// exists, so the set is read from the store on every call.
func reachFor(t *testing.T, c *Controller, childID string, req protocol.SpawnRequest) []string {
	t.Helper()
	b := c.binderFor(req, executorOwner{})
	got, err := b.Reachable(childID)
	assert.NewAborting(t).NoError(err, "Reachable")
	return reachIDs(got)
}

func TestReachIsTheChildsOwnSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil), machineExec("C", "c", nil))
	setParent(t, c, "c_parent", []string{"A", "B"})

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"A"}})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A"}, reachFor(t, c, snap.ChildID, protocol.SpawnRequest{}), "reach is the child's set")

	ck.NoError(c.st.SetExecutorSet(snap.ChildID, []string{"A", "B"}))
	ck.EqDeep([]string{"A", "B"}, reachFor(t, c, snap.ChildID, protocol.SpawnRequest{}),
		"a set changed after the binder was built applies on the next call")
}

func TestReachIgnoresAncestorSelectors(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))
	c.st.Insert(&childstore.Session{
		ChildID: "c_grand", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: "machine=a", Labels: map[string]string{},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{allSet},
		Labels: map[string]string{childstore.LabelParent: "c_grand", childstore.LabelRoot: "c_grand"},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{"A", "B"},
		Labels: map[string]string{childstore.LabelParent: "c_parent", childstore.LabelRoot: "c_grand"},
	})

	ck.EqDeep([]string{"A", "B"}, reachFor(t, c, "c_child", protocol.SpawnRequest{}),
		"a grandparent's legacy selector narrows nothing")
}

func TestReachOfALegacyRowUsesItsOwnSelector(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))
	c.st.Insert(&childstore.Session{
		ChildID: "c_sel", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSelector: "machine=a", Labels: map[string]string{},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_plain", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, Labels: map[string]string{},
	})

	ck.EqDeep([]string{"A"}, reachFor(t, c, "c_sel", protocol.SpawnRequest{}), "nil set, selector")
	ck.EqDeep([]string{"A", "B"}, reachFor(t, c, "c_plain", protocol.SpawnRequest{}), "nil set, no selector is every executor")
}

func TestReachOfAnUnknownChildIsEmpty(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil))
	ck.Empty(reachFor(t, c, "c_missing", protocol.SpawnRequest{}), "an unknown child addresses nothing")
	// A request carrying no resolved set does not stand in for one either.
	ck.Empty(reachFor(t, c, "c_missing", protocol.SpawnRequest{ExecutorSelector: "machine=a"}), "nor does a selector")
}

func TestReachOfAnEmptySetIsEmpty(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil))
	c.st.Insert(&childstore.Session{
		ChildID: "c_none", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{}, Labels: map[string]string{},
	})
	ck.Empty(reachFor(t, c, "c_none", protocol.SpawnRequest{}), "an empty non-nil set addresses nothing")
	b := c.binderFor(protocol.SpawnRequest{}, executorOwner{})
	_, err := b.ChooseFor("c_none")
	ck.Error(err, "and binds to nothing")
}

func TestReachStillRequiresOwnerAndAdmits(t *testing.T) {
	ck := assert.NewAborting(t)
	a := ex("A", map[string]string{"machine": "a"}, "role=x")
	b := exOwned("B", map[string]string{"machine": "b"}, "", "someone-else")
	c := setController(t, a, b)
	c.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{"A", "B"}, Labels: map[string]string{},
	})

	ck.Empty(reachFor(t, c, "c_child", protocol.SpawnRequest{}),
		"a set names executors; owner and Admits still decide whether the child may use them")
}

func TestReachAllExcludesSandboxes(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), sandboxExec("S", "box", ""))
	c.st.Insert(&childstore.Session{
		ChildID: "c_all", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{allSet}, Labels: map[string]string{},
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_named", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{"A", "S"}, Labels: map[string]string{},
	})

	ck.EqDeep([]string{"A"}, reachFor(t, c, "c_all", protocol.SpawnRequest{}), "all never reaches a sandbox")
	ck.EqDeep([]string{"A", "S"}, reachFor(t, c, "c_named", protocol.SpawnRequest{}), "a sandbox named in the set is reachable")
}

func TestReachIncludesAnOfflineInstalledMember(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "I")
	c := reachController("u1",
		[]execpool.LiveExecutor{nativeExec("A", "a", "u1")},
		map[string]executors.Executor{"I": installedExecRow("I", "inst", "u1")},
		installs,
	)
	insertChildRow(c, "c_member", "", "u1")
	ck.NoError(c.st.SetExecutorSet("c_member", []string{"I"}))
	insertChildRow(c, "c_other", "", "u1")
	ck.NoError(c.st.SetExecutorSet("c_other", []string{"A"}))

	b := c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"})
	got, err := b.Reachable("c_member")
	ck.Require().NoError(err)
	ck.EqDeep([]string{"I"}, reachIDs(got), "an offline installed executor is reachable when the set names it")
}

func TestReachExcludesAnOfflineInstalledNonMember(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "I")
	setInstaller(t, installs, "I", "c_other")
	c := reachController("u1",
		[]execpool.LiveExecutor{nativeExec("A", "a", "u1")},
		map[string]executors.Executor{"I": installedExecRow("I", "inst", "u1")},
		installs,
	)
	insertChildRow(c, "c_other", "", "u1")
	ck.NoError(c.st.SetExecutorSet("c_other", []string{"A"}))

	b := c.binderFor(protocol.SpawnRequest{}, executorOwner{Name: "u", UserID: "u1"})
	got, err := b.Reachable("c_other")
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A"}, reachIDs(got), "being the installer grants nothing; only the set does")
}

func TestTwoReachableStillRequireTheExecutorArgument(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "A", nil), machineExec("B", "B", nil))
	c.st.Insert(&childstore.Session{
		ChildID: "c1", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, ExecutorSet: []string{"A", "B"}, Labels: map[string]string{},
	})
	be := newBoundExecutor("c1", c.binderFor(protocol.SpawnRequest{}, executorOwner{}))

	_, err := be.Execute(context.Background(), "bash", nil)
	ck.Require().Error(err)
	ck.StrContains(err.Error(), `this agent can reach 2 executors (A, B); pass executor: "<name>"`, "error text")
}

func TestEphemeralSpawnKeepsEveryEphemeralMember(t *testing.T) {
	ck := assert.NewAborting(t)
	eph := func(id string) execpool.LiveExecutor {
		le := machineExec(id, id, nil)
		le.Executor.WorkspaceMode = "ephemeral"
		return le
	}
	pinned := machineExec("P", "P", nil)
	pinned.Executor.WorkspaceMode = "pinned"
	c := setController(t, eph("E1"), eph("E2"), pinned)
	setParent(t, c, "c_parent", []string{allSet})
	_, err := c.st.SetLabels("c_parent", map[string]string{homeExecutorLabel: "E1"}, nil)
	ck.NoError(err)

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", WorkspaceMode: "ephemeral"})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"E1", "E2"}, snap.ExecutorSet, "an ephemeral child never gets a single home")

	c.execPool = &fakePool{live: []execpool.LiveExecutor{eph("E2"), pinned}}
	b := c.binderFor(protocol.SpawnRequest{WorkspaceMode: "ephemeral"}, executorOwner{})
	got, err := b.ChooseFor(snap.ChildID)
	ck.Require().NoError(err)
	ck.Eq("E2", got, "re-placement lands on the remaining member")
}

// A resumed child's set is the one on its row. The request a resume rebuilds
// carries it, and the binder agrees: nothing re-resolves a stored set, so a
// stored empty or legacy set can never widen.
func TestResumeRebuildsTheSetFromTheRow(t *testing.T) {
	cases := []struct {
		name      string
		stored    []string
		wantReach []string
	}{
		{"legacy nil", nil, []string{"A", "B"}},
		{"empty", []string{}, nil},
		{"all", []string{allSet}, []string{"A", "B"}},
		{"ids", []string{"B"}, []string{"B"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))
			setParent(t, c, "c_parent", []string{allSet})
			snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"A"}})
			ck.Require().NoError(err)
			ck.NoError(c.st.SetExecutorSet(snap.ChildID, tc.stored))

			stored, ok := c.st.Get(snap.ChildID)
			ck.Require().True(ok)
			req := resumeRequestFromSnapshot(stored, "")
			ck.EqDeep(tc.stored, req.Executors, "the rebuilt request carries the stored set unchanged")
			ck.Eq(tc.stored == nil, req.Executors == nil, "nil stays nil and empty stays empty")

			b := c.binderFor(req, executorOwner{})
			got, err := b.Reachable(snap.ChildID)
			ck.Require().NoError(err)
			ck.EqDeep(len(tc.wantReach), len(got), "reach after resume")
			if len(tc.wantReach) > 0 {
				ck.EqDeep(tc.wantReach, reachIDs(got), "reach after resume")
			}
			chosen, cerr := b.ChooseFor(snap.ChildID)
			if len(tc.wantReach) == 0 {
				ck.Error(cerr, "an empty stored set binds to nothing")
				return
			}
			ck.NoError(cerr)
			ck.True(slices.Contains(tc.wantReach, chosen), "ChooseFor stays inside the stored set %v; got %s", tc.wantReach, chosen)
		})
	}
}

func TestFilterBySetFailsClosed(t *testing.T) {
	ck := assert.NewAborting(t)
	rows := []executors.Executor{{ID: "A", Labels: map[string]string{"machine": "a"}}, {ID: "B"}}
	ck.Len(filterBySet(rows, true, nil, ""), 2, "all")
	ck.Empty(filterBySet(rows, false, []string{}, ""), "an empty id list")
	ck.Empty(filterBySet(rows, false, nil, ""), "an absent id list")
	ck.Empty(filterBySet(rows, false, nil, "machine in ("), "an unparseable legacy selector")
	ck.EqDeep([]string{"B", "A"}, idsOf(filterBySet([]executors.Executor{rows[1], rows[0]}, false, []string{"A", "B"}, "")), "input order is kept")
}

func TestExecutorSetSpecOfAnUnknownChildAddressesNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil))

	all, ids, selector := c.executorSetSpec("c_missing")
	ck.False(all, "an unknown child is not all")
	ck.Empty(ids, "ids")
	ck.Eq("", selector, "selector")
	set, err := c.effectiveExecutorSet("c_missing")
	ck.NoError(err)
	ck.Empty(set, "an unknown child's effective set")

	all, _, _ = c.executorSetSpec("")
	ck.True(all, "a top-level caller addresses every executor of its owner")
}

// Admits is evaluated at spawn against exactly the labels the child's row then
// carries, daemon keys included, so spawn and reach agree: an executor that
// admits rafiki/kind=fundi serves a fundi child at spawn and in reach, and one
// that admits rafiki/kind=claude serves it in neither.
func TestSpawnAdmitsReadsTheLabelsTheRowWillCarry(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t,
		ex("A", map[string]string{"machine": "a"}, "rafiki/kind=fundi"),
		ex("B", map[string]string{"machine": "b"}, "rafiki/kind=claude"),
		ex("N", map[string]string{"machine": "n"}, "rafiki/kind!=fundi"))
	setParent(t, c, "c_parent", []string{allSet})

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"a"}})
	ck.Require().NoError(err, "an executor admitting the child's daemon-written kind label")
	ck.EqDeep([]string{"A"}, snap.ExecutorSet, "the set")
	for id, want := range map[string]bool{"A": true, "B": false, "N": false} {
		for _, le := range c.execPool.Live() {
			if le.Executor.ID == id {
				ck.Eq(want, executorAdmitsChild(le.Executor, snap.Labels, ""), "reach on the stored row's labels agrees for %s", id)
			}
		}
	}

	for _, name := range []string{"b", "n"} {
		_, err = spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{name}})
		ck.Error(err, "an executor whose Admits rejects the row's labels is refused at spawn: %s", name)
	}
}

// newChildLabels carries every daemon label known at spawn, and admission reads
// the request's resolved copy over a rebuild.
func TestAdmissionLabelsPreferTheResolvedRowLabels(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t)
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: "/w", Routing: "r", ConfigDir: "/cfg", Labels: map[string]string{"team": "x"}}
	labels := newChildLabels(req, "brent", childLabelFacts{ModelAlias: "al", ForkSource: "c_src", Parent: "c_p", Root: "c_r"})
	ck.EqDeep(map[string]string{
		"team": "x", "owner": "brent", "rafiki/cwd": "/w", "rafiki/kind": "fundi", "rafiki/routing": "r",
		"rafiki/config_dir": "/cfg", "rafiki/model-alias": "al", "rafiki/forked-from": "c_src",
		childstore.LabelParent: "c_p", childstore.LabelRoot: "c_r",
	}, labels, "the labels")

	req.RowLabels = labels
	ck.EqDeep(labels, c.admissionLabels(req, executorOwner{Name: "other"}), "admission reads the resolved labels")
	req.RowLabels = nil
	ck.Eq("brent", c.admissionLabels(req, executorOwner{Name: "brent"})["owner"], "a request with none is rebuilt")
}
