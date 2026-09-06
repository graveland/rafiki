package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestMatchExecutorRefByMachineLabel(t *testing.T) {
	candidates := []executors.Executor{
		{ID: "exec-1", Labels: map[string]string{"machine": "greyshift"}},
		{ID: "exec-2", Labels: map[string]string{"machine": "silvershift"}},
	}
	got, ok := matchExecutorRef("greyshift", candidates)
	if !ok || got.ID != "exec-1" {
		t.Fatalf("want exec-1, got %+v ok=%v", got, ok)
	}
}

func TestMatchExecutorRefByID(t *testing.T) {
	candidates := []executors.Executor{
		{ID: "exec-1", Labels: map[string]string{"machine": "greyshift"}},
	}
	got, ok := matchExecutorRef("exec-1", candidates)
	if !ok || got.ID != "exec-1" {
		t.Fatalf("want exec-1, got %+v ok=%v", got, ok)
	}
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
	if !ok || got.ID != "exec-2" {
		t.Fatalf("want exec-2 (machine label match), got %+v ok=%v", got, ok)
	}
}

func TestMatchExecutorRefNoMatch(t *testing.T) {
	_, ok := matchExecutorRef("nonexistent", []executors.Executor{{ID: "exec-1"}})
	if ok {
		t.Fatal("want no match")
	}
}

func TestChooseExecutorHonoursExecutorRef(t *testing.T) {
	c := selectFixture(t, "",
		ex("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, ""),
		ex("exec-2", map[string]string{"machine": "silvershift", "env": "home"}, ""),
	)
	exec, err := c.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "greyshift"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if exec.ID != "exec-1" {
		t.Fatalf("want exec-1, got %s", exec.ID)
	}
}

func TestChooseExecutorExecutorRefStillConfinementChecked(t *testing.T) {
	// exec-1 exists but its admission selector excludes this owner — an
	// explicit ref must still be refused, never bypass confinement.
	c := selectFixture(t, "",
		ex("exec-1", map[string]string{"machine": "greyshift"}, "owner=someone-else"),
	)
	_, err := c.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "greyshift"}, "brent")
	if err == nil {
		t.Fatal("want a refusal — the named executor exists but does not admit this child")
	}
}

func TestChooseExecutorExecutorRefNotFound(t *testing.T) {
	c := selectFixture(t, "", ex("exec-1", map[string]string{"machine": "greyshift"}, ""))
	_, err := c.chooseExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "nonexistent"}, "")
	if err == nil {
		t.Fatal("want a refusal naming the unknown ref")
	}
}

func TestChooseLaunchExecutorHonoursExecutorRefAndRequiresLaunchKind(t *testing.T) {
	c := selectFixture(t, "",
		exWithLaunch("exec-1", map[string]string{"machine": "greyshift", "env": "home"}, "", "claude"),
		ex("exec-2", map[string]string{"machine": "silvershift", "env": "home"}, ""), // no launch support
	)
	// Pinning to the one WITHOUT launch support must fail even though the ref
	// matches — an explicit pin still has to clear every other check.
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "silvershift"}, "", "claude")
	if err == nil {
		t.Fatal("want a refusal — silvershift does not support launching claude")
	}
	exec, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent", ExecutorRef: "greyshift"}, "", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if exec.ID != "exec-1" {
		t.Fatalf("want exec-1, got %s", exec.ID)
	}
}

// TestExecutorReasonLaunchBranchDetectsLineageExclusion is a regression test
// for a gap the launch branch of executorReason had: it checked e.Enabled,
// launchable[e.ID], its own admits selector, and the child's own selector --
// but never parentSet membership, which is where an ANCESTOR's selector
// (evaluated during lineage narrowing in effectiveExecutorSetFor) excludes an
// executor. "work" here passes every launch-branch check the old code ran
// (enabled, launchable, admits, childSel.Explain all pass) and is excluded
// ONLY by the parent's env=home selector during lineage narrowing -- exactly
// the case the missing check let slip through as "not excluded".
//
// chooseLaunchExecutor's own refusal (checked first, as a sanity check) does
// NOT exercise this bug: its actual selection decision comes from
// Narrow(parentSet, sel) inside narrowedExecutorCandidates, which is entirely
// independent of executorReason and already excludes "work" correctly for
// its own reasons. executorReason itself is used only to produce the
// human-readable exclusion text (explainNoLaunchMatch's per-row message, and
// -- the case that actually matters -- ListExecutorRows.Eligible), so the
// regression must be asserted directly against executorReason's return
// value, not inferred from chooseLaunchExecutor's error/success outcome.
func TestExecutorReasonLaunchBranchDetectsLineageExclusion(t *testing.T) {
	c := selectFixture(t, "env=home",
		exWithLaunch("work", map[string]string{"env": "work"}, "", "claude"),
	)
	req := protocol.SpawnRequest{ParentChildID: "c_parent"}

	// Sanity: the end-to-end refusal is correct (and stays correct) either way.
	if _, err := c.chooseLaunchExecutor(req, "", "claude"); err == nil {
		t.Fatal("want a refusal — work is excluded by the parent's lineage selector")
	}

	// The actual regression: ask executorReason directly, the same way
	// explainNoLaunchMatch and ListExecutorRows do, whether "work" is
	// excluded. Before the fix this returns "" (claims eligible) because the
	// launch branch never checks parentSet membership.
	candidates, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("want zero post-selector candidates (lineage already excluded work), got %v", candidates)
	}
	if len(parentSet) != 0 {
		t.Fatalf("want work excluded from parentSet by the lineage narrowing, got %v", parentSet)
	}
	launchable := launchKindSet(c.execPool.Live(), "claude")
	var work executors.Executor
	for _, le := range c.execPool.Live() {
		if le.Executor.ID == "work" {
			work = le.Executor
		}
	}
	reason := executorReason(work, req, launchable, "claude", sel, childLabels, parentSet)
	if reason == "" {
		t.Fatal("executorReason claims work is eligible, but it is excluded by the parent's lineage selector")
	}
}
