package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// A live descendant's reach is its OWN stored set. The spawner's current set is
// read once, at spawn: a parent that shrank or was closed afterwards, and a
// restart that rebuilds the request with no parent, must not change what the
// child binds to.
func TestChooseForIgnoresTheSpawnersCurrentSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t, machineExec("A", "a", nil), machineExec("B", "b", nil))
	setParent(t, c, "c_parent", []string{allSet})
	snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"A", "B"}})
	ck.Require().NoError(err)

	// Only B is live, and the spawner now addresses only A.
	c.execPool = &fakePool{live: []execpool.LiveExecutor{machineExec("B", "b", nil)}}
	ck.NoError(c.st.SetExecutorSet("c_parent", []string{"A"}))

	withParent := c.binderFor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	got, err := withParent.ChooseFor(snap.ChildID)
	ck.Require().NoError(err, "the child's own set names B")
	ck.Eq("B", got, "a shrunk spawner set must not change a live descendant's choice")

	c.st.Delete("c_parent")
	got, err = withParent.ChooseFor(snap.ChildID)
	ck.Require().NoError(err)
	ck.Eq("B", got, "a closed spawner must not change it either")

	stored, ok := c.st.Get(snap.ChildID)
	ck.Require().True(ok)
	rebuilt := c.binderFor(resumeRequestFromSnapshot(stored, ""), executorOwner{})
	got, err = rebuilt.ChooseFor(snap.ChildID)
	ck.Require().NoError(err)
	ck.Eq("B", got, "the choice after a restart (no parent in the request) is identical")
}

// boundExecutor.resolve honours ChooseFor when Reachable is empty, so ChooseFor
// must be a subset of Reachable for every state of the pool and the child's
// labels, not just the happy one.
func TestChooseForStaysInsideReachable(t *testing.T) {
	picky := ex("C", map[string]string{"machine": "c"}, "role=x")
	all := []execpool.LiveExecutor{machineExec("A", "a", nil), machineExec("B", "b", nil), picky}
	for mask := 1; mask < 1<<len(all); mask++ {
		var live []execpool.LiveExecutor
		for i, le := range all {
			if mask&(1<<i) != 0 {
				live = append(live, le)
			}
		}
		ck := assert.NewAborting(t)
		c := setController(t, all...)
		setParent(t, c, "c_parent", []string{allSet})
		snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"A", "B"}})
		ck.Require().NoError(err)
		ck.NoError(c.st.SetExecutorSet(snap.ChildID, []string{"A", "B", "C"}))
		c.execPool = &fakePool{live: live}

		b := c.binderFor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
		reach, err := b.Reachable(snap.ChildID)
		ck.Require().NoError(err)
		chosen, cerr := b.ChooseFor(snap.ChildID)
		if len(reach) == 0 {
			ck.Error(cerr, "mask %b: nothing is reachable, so nothing may be chosen", mask)
			continue
		}
		ck.Require().NoError(cerr, "mask %b", mask)
		ck.True(slices.Contains(reachIDs(reach), chosen),
			"mask %b: ChooseFor chose %s outside Reachable %v (C's Admits excludes the child)", mask, chosen, reachIDs(reach))
	}
}

// Admits is evaluated against the CHILD's labels on every path: the spawn-time
// set, Reachable and ChooseFor. A parent that carries a label the child lacks
// must not admit the child to an executor that wants it.
func TestAdmitsReadsTheChildsOwnLabels(t *testing.T) {
	ck := assert.NewAborting(t)
	x := ex("X", map[string]string{"machine": "x"}, "team=blue")
	c := setController(t, x, machineExec("A", "a", nil))
	c.st.Insert(&childstore.Session{
		ChildID: "c_parent", Status: protocol.StatusIdle, StartedAt: time.Now(),
		Kind: protocol.KindFundi, MaxDepth: 2, MaxChildren: 8,
		Labels: map[string]string{"team": "blue"}, ExecutorSet: []string{allSet},
	})

	_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{
		ParentChildID: "c_parent", Executors: []string{"X"}, Labels: map[string]string{"team": "red"},
	}, executorOwner{})
	ck.Error(err, "the parent's label must not admit a child whose own labels the executor refuses")

	snap, err := spawnFundi(t, c, protocol.SpawnRequest{
		ParentChildID: "c_parent", Executors: []string{"X"}, Labels: map[string]string{"team": "blue"},
	})
	ck.Require().NoError(err, "a child that carries the label is admitted")
	ck.EqDeep([]string{"X"}, snap.ExecutorSet, "set")

	b := c.binderFor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	_, err = c.st.SetLabels(snap.ChildID, map[string]string{"team": "red"}, nil)
	ck.NoError(err)
	reach, err := b.Reachable(snap.ChildID)
	ck.Require().NoError(err)
	ck.Empty(reach, "Reachable reads the child's labels")
	_, err = b.ChooseFor(snap.ChildID)
	ck.Error(err, "ChooseFor reads the same labels, so it cannot bind what Reachable refuses")
}

// A fresh spawn never stores nil, with or without a pool: nil reads as a legacy
// row that reaches everything its owner has.
func TestPoolessSpawnNeverStoresNilSet(t *testing.T) {
	newC := func(t *testing.T) *Controller {
		c := setController(t)
		c.execPool = nil
		return c
	}

	t.Run("top level inherits all", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newC(t)
		snap, err := spawnFundi(t, c, protocol.SpawnRequest{})
		ck.Require().NoError(err)
		ck.EqDeep([]string{allSet}, snap.ExecutorSet, "stored set")
	})

	t.Run("child inherits its spawner's stored set", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newC(t)
		setParent(t, c, "c_ids", []string{"A", "B"})
		setParent(t, c, "c_none", []string{})
		setParent(t, c, "c_legacy", nil)
		for parent, want := range map[string][]string{
			"c_ids": {"A", "B"}, "c_none": {}, "c_legacy": {allSet},
		} {
			snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: parent})
			ck.Require().NoError(err, parent)
			ck.EqDeep(want, snap.ExecutorSet, "a child of %s", parent)
			ck.True(snap.ExecutorSet != nil, "never nil")
		}
	})

	t.Run("a legacy selector is not widened", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newC(t)
		c.st.Insert(&childstore.Session{
			ChildID: "c_sel", Status: protocol.StatusIdle, StartedAt: time.Now(),
			Kind: protocol.KindFundi, MaxDepth: 2, MaxChildren: 8,
			Labels: map[string]string{}, ExecutorSelector: "env=ci",
		})
		snap, err := spawnFundi(t, c, protocol.SpawnRequest{ParentChildID: "c_sel"})
		ck.Require().NoError(err)
		ck.EqDeep([]string{}, snap.ExecutorSet, "a selector with no pool to resolve it addresses nothing")
	})

	t.Run("explicit all only from an all spawner", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newC(t)
		setParent(t, c, "c_all", []string{allSet})
		setParent(t, c, "c_ids", []string{"A"})
		_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_all", Executors: []string{allSet}}, executorOwner{})
		ck.NoError(err)
		_, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{Executors: []string{allSet}}, executorOwner{})
		ck.NoError(err, "a top-level caller is an all spawner")
		_, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_ids", Executors: []string{allSet}}, executorOwner{})
		ck.Error(err, "an all request from a finite spawner")
	})

	t.Run("names that cannot be verified are refused", func(t *testing.T) {
		ck := assert.NewAborting(t)
		c := newC(t)
		setParent(t, c, "c_all", []string{allSet})
		for name, req := range map[string]protocol.SpawnRequest{
			"executors": {ParentChildID: "c_all", Executors: []string{"A"}},
			"ref":       {ParentChildID: "c_all", ExecutorRef: "a"},
			"selector":  {ParentChildID: "c_all", ExecutorSelector: "env=ci"},
			"unknown parent": {
				ParentChildID: "c_gone",
			},
		} {
			_, err := c.resolveSpawnExecutorSet(req, executorOwner{})
			ck.Error(err, name)
		}
	})
}

// A stored set never holds another child's sandbox, and "all" never reaches a
// sandbox even when a selector names it.
func TestSpawnSetNeverHoldsAnotherChildsSandbox(t *testing.T) {
	ck := assert.NewAborting(t)
	c := setController(t,
		machineExec("A", "a", nil),
		childOwnedSandboxEx("S", "c_other", map[string]string{"machine": "s"}),
		sandboxExec("N", "named", ""),
	)
	setParent(t, c, "c_parent", []string{"A", "S", "N"})

	_, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"S"}}, executorOwner{})
	ck.Error(err, "another child's sandbox cannot be named even though the spawner's stored set holds its id")

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{})
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A", "N"}, got.Executors, "a defaulted set copies the spawner's reach, which excludes it")

	got, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", Executors: []string{"N"}}, executorOwner{})
	ck.Require().NoError(err, "an operator sandbox the spawner's set names is still nameable")
	ck.EqDeep([]string{"N"}, got.Executors, "set")

	_, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ExecutorSelector: "machine=named"}, executorOwner{})
	ck.Error(err, "a top-level (all) spawn's selector is placement only: it never reaches a sandbox")

	got, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ExecutorRef: "named"}, executorOwner{})
	ck.Require().NoError(err, "an all spawner may name an operator sandbox explicitly")
	ck.EqDeep([]string{"N"}, got.Executors, "set")
}

// The selector probe sees the spawner's offline installed members too: a
// matching offline member is frozen into the set, and only a selector that
// matches nothing anywhere refuses.
func TestSpawnSelectorSeesOfflineInstalledMembers(t *testing.T) {
	ck := assert.NewAborting(t)
	installs := installStoreWith(t, "u1", "I")
	c := reachController("u1",
		[]execpool.LiveExecutor{nativeExec("A", "a", "u1")},
		map[string]executors.Executor{"I": installedExecRow("I", "inst", "u1")},
		installs,
	)
	insertChildRow(c, "c_parent", "", "u1")
	ck.NoError(c.st.SetExecutorSet("c_parent", []string{"A", "I"}))
	owner := executorOwner{Name: "u", UserID: "u1"}

	got, err := c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "machine=inst"}, owner)
	ck.Require().NoError(err, "the only match is offline: the spawn is not refused")
	ck.EqDeep([]string{"I"}, got.Executors, "the offline member is frozen into the set")

	got, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "machine in (a,inst)"}, owner)
	ck.Require().NoError(err)
	ck.EqDeep([]string{"A", "I"}, got.Executors, "live and offline matches are both frozen")

	_, err = c.resolveSpawnExecutorSet(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorSelector: "machine=nowhere"}, owner)
	ck.Error(err, "a selector that matches nothing anywhere still refuses")
}

// A claude resume goes back to the executor it was launched on, but the pin is
// a place, not a grant: the executor must still be reachable by the child.
func TestClaudeResumePinMustStillBeReachable(t *testing.T) {
	pinned := func(le execpool.LiveExecutor) execpool.LiveExecutor {
		le.Describe = exWithLaunch(le.Executor.ID, nil, "", "claude").Describe
		return le
	}
	cases := []struct {
		name   string
		mutate func(*executors.Executor)
		snap   childstore.Snapshot
		owner  string
		ok     bool
	}{
		{"in the stored set", nil, childstore.Snapshot{ExecutorSet: []string{"P"}}, "", true},
		{"set is all", nil, childstore.Snapshot{ExecutorSet: []string{allSet}}, "", true},
		{"set no longer names it", nil, childstore.Snapshot{ExecutorSet: []string{"Q"}}, "", false},
		{"empty set", nil, childstore.Snapshot{ExecutorSet: []string{}}, "", false},
		{"disabled", func(e *executors.Executor) { e.Enabled = false }, childstore.Snapshot{ExecutorSet: []string{"P"}}, "", false},
		{"now owned by someone else", func(e *executors.Executor) { e.OwnerUserID = "other" }, childstore.Snapshot{ExecutorSet: []string{"P"}}, "", false},
		{"Admits now excludes the child", func(e *executors.Executor) { e.Admits = "team=blue" }, childstore.Snapshot{ExecutorSet: []string{"P"}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			le := pinned(machineExec("P", "p", nil))
			if tc.mutate != nil {
				tc.mutate(&le.Executor)
			}
			c := setController(t, le)
			snap := tc.snap
			snap.ChildID = "c_resume"
			snap.Labels = map[string]string{"rafiki/executor": "P"}
			_, err := c.darajaLaunchExecutor(protocol.SpawnRequest{Kind: protocol.KindClaude}, executorOwner{UserID: tc.owner}, &snap)
			if tc.ok {
				ck.NoError(err)
				return
			}
			ck.Error(err, "an unreachable pinned executor fails closed")
		})
	}
}

// Every path that reaches a local fork on the daemon host asks localForkRefused
// with the child's RESOLVED set. The table drives each entry point with the set
// the production resolver produced, so a guard that reads the spawner instead of
// the child fails here.
func TestEveryLocalForkPathSeesTheResolvedSet(t *testing.T) {
	kinds := []string{protocol.KindClaude, protocol.KindScript}
	entries := map[string]func(c *Controller, req protocol.SpawnRequest, id string) error{
		"agentRunner": func(c *Controller, req protocol.SpawnRequest, id string) error {
			_, err := c.agentRunner(req, id, false, "", "", nil)
			return err
		},
		"kind runner": func(c *Controller, req protocol.SpawnRequest, id string) error {
			var err error
			if req.Kind == protocol.KindClaude {
				_, err = c.claudeRunner(req, id, "", "", nil)
			} else {
				_, err = c.scriptRunner(req, id, "", "")
			}
			return err
		},
	}
	for _, kind := range kinds {
		for entry, run := range entries {
			t.Run(kind+"/"+entry, func(t *testing.T) {
				ck := assert.NewAborting(t)
				c := setController(t, machineExec("A", "a", nil))
				setParent(t, c, "p_all", []string{allSet})

				base := protocol.SpawnRequest{Kind: kind, Cwd: t.TempDir(), Script: &protocol.ScriptSpec{Repo: "local", Script: "s"}}
				resolve := func(parent string, executors ...string) protocol.SpawnRequest {
					req := base
					req.ParentChildID, req.Executors = parent, executors
					got, err := c.resolveSpawnExecutorSet(req, executorOwner{})
					ck.Require().NoError(err, "resolve %v", executors)
					return got
				}

				// Fresh, resolved by production code: a finite set under an all parent
				// and at the top level is confined and must not fork locally.
				ck.Error(run(c, resolve("p_all", "A"), "c_fresh_all"), "finite set under an all parent")
				ck.Error(run(c, resolve("", "A"), "c_fresh_top"), "finite set at the top level")

				// Resume: the stored row decides, whatever the request says.
				for id, set := range map[string][]string{"c_row_ids": {"A"}, "c_row_none": {}} {
					setParent(t, c, id, set)
					req := base
					req.Executors = []string{allSet}
					ck.Error(run(c, req, id), "a resumed child whose row holds %v", set)
				}

				// A fresh child that was never resolved cannot be shown unconfined.
				ck.Error(run(c, base, "c_unresolved"), "an unresolved fresh set fails closed")

				// An all child is not refused for confinement (it may still fail for
				// another reason in the local runner).
				err := run(c, resolve("p_all", allSet), "c_all")
				if err != nil {
					ck.False(isConfinementRefusal(err), "an all child was refused as confined: %v", err)
				}
			})
		}
	}
}

func isConfinementRefusal(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "confined to the executor plane") ||
		strings.Contains(err.Error(), "cannot confirm the executor set"))
}
