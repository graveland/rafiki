package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestMatchExecutorRefByMachineLabel(t *testing.T) {
	candidates := []executors.Executor{
		{ID: "exec-1", Labels: map[string]string{"machine": "greyshift"}},
		{ID: "exec-2", Labels: map[string]string{"machine": "silvershift"}},
	}
	got, ok := matchExecutorRef("greyshift", candidates)
	assert.NewAborting(t).False(!ok || got.ID != "exec-1", "want exec-1, got %+v ok=%v", got, ok)
}

func TestMatchExecutorRefByID(t *testing.T) {
	candidates := []executors.Executor{
		{ID: "exec-1", Labels: map[string]string{"machine": "greyshift"}},
	}
	got, ok := matchExecutorRef("exec-1", candidates)
	assert.NewAborting(t).False(!ok || got.ID != "exec-1", "want exec-1, got %+v ok=%v", got, ok)
}

func TestMatchExecutorRefMachineLabelWinsOverIDLookingLikeAnotherMachine(t *testing.T) {
	// A machine named exactly like another executor's raw id is a pathological
	// but possible operator choice; machine-label matching is checked FIRST so
	// it always wins, matching the design's stated resolution order.
	candidates := []executors.Executor{
		{ID: "greyshift", Labels: map[string]string{"machine": "not-greyshift"}},
		{ID: "exec-2", Labels: map[string]string{"machine": "greyshift"}},
	}
	got, ok := matchExecutorRef("greyshift", candidates)
	assert.NewAborting(t).False(!ok || got.ID != "exec-2", "want exec-2 (machine label match), got %+v ok=%v", got, ok)
}

func TestMatchExecutorRefNoMatch(t *testing.T) {
	_, ok := matchExecutorRef("nonexistent", []executors.Executor{{ID: "exec-1"}})
	assert.NewAborting(t).False(ok, "want no match")
}

func TestChooseExecutorHonoursExecutorRef(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		ex("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, ""),
		ex("exec-2", map[string]string{"machine": "silvershift", "env": "home"}, ""),
	)
	exec, err := c.chooseExecutor(protocol.SpawnRequest{Executors: []string{childstore.ExecutorSetAll}, ParentChildID: "c_parent", ExecutorRef: "greyshift"}, executorOwner{})
	ck.NoError(err)
	ck.Eq("exec-1", exec.ID, "want exec-1, got")
}

func TestChooseExecutorExecutorRefStillConfinementChecked(t *testing.T) {
	// exec-1 exists but its admission selector excludes this owner — an
	// explicit ref must still be refused, never bypass confinement.
	c := selectFixture(t, "",
		ex("exec-1", map[string]string{"machine": "greyshift"}, "owner=someone-else"),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{Executors: []string{childstore.ExecutorSetAll}, ParentChildID: "c_parent", ExecutorRef: "greyshift"}, executorOwner{Name: "brent"})
	assert.NewAborting(t).Error(err, "want a refusal — the named executor exists but does not admit this child")
}

func TestChooseExecutorExecutorRefNotFound(t *testing.T) {
	c := selectFixture(t, "", ex("exec-1", map[string]string{"machine": "greyshift"}, ""))
	_, err := c.chooseExecutor(protocol.SpawnRequest{Executors: []string{childstore.ExecutorSetAll}, ParentChildID: "c_parent", ExecutorRef: "nonexistent"}, executorOwner{})
	assert.NewAborting(t).Error(err, "want a refusal naming the unknown ref")
}

func TestChooseLaunchExecutorHonoursExecutorRefAndRequiresLaunchKind(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		exWithLaunch("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, "", "claude"),
		ex("exec-2", map[string]string{"machine": "silvershift", "env": "home"}, ""), // no launch support
	)
	// Pinning to the one WITHOUT launch support must fail even though the ref
	// matches — an explicit pin still has to clear every other check.
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{Executors: []string{childstore.ExecutorSetAll}, ParentChildID: "c_parent", ExecutorRef: "silvershift"}, executorOwner{}, "claude")
	ck.Error(err, "want a refusal — silvershift does not support launching claude")
	exec, err := c.chooseLaunchExecutor(protocol.SpawnRequest{Executors: []string{childstore.ExecutorSetAll}, ParentChildID: "c_parent", ExecutorRef: "greyshift"}, executorOwner{}, "claude")
	ck.NoError(err)
	ck.Eq("exec-1", exec.ID, "want exec-1, got")
}

// TestExecutorReasonLaunchBranchNamesTheSet pins the launch branch of
// executorReason reporting set membership. "work" passes every other check in
// that branch (enabled, launchable, admits, childSel.Explain) and is excluded
// only because the spawner's set (a legacy env=home selector) leaves it out.
//
// chooseLaunchExecutor's own refusal does not exercise this: its decision comes
// from narrowedExecutorCandidates, independent of executorReason, which only
// produces the exclusion text (explainNoLaunchMatch and ListExecutorRows), so
// the text is asserted directly.
func TestExecutorReasonLaunchBranchNamesTheSet(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		exWithLaunch("work", map[string]string{"env": "work"}, "", "claude"),
	)
	req := protocol.SpawnRequest{ParentChildID: "c_parent"}

	// Sanity: the end-to-end refusal is correct (and stays correct) either way.
	if _, err := c.chooseLaunchExecutor(req, executorOwner{}, "claude"); err == nil {
		t.Fatal("want a refusal: work is outside the spawner's set")
	}

	// Ask executorReason directly, the same way explainNoLaunchMatch and
	// ListExecutorRows do.
	candidates, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, executorOwner{})
	ck.NoError(err)
	ck.Empty(candidates, "want zero candidates (the set already excluded work), got")
	ck.Empty(parentSet, "want work excluded from parentSet by the set, got")
	launchable := launchKindSet(c.execPool.Live(), "claude")
	var work executors.Executor
	for _, le := range c.execPool.Live() {
		if le.Executor.ID == "work" {
			work = le.Executor
		}
	}
	reason := executorReason(work, req, launchable, "claude", sel, childLabels, parentSet, "")
	ck.Eq("not in your executor set", reason)
}
