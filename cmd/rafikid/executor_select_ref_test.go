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
