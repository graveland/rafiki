package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// exWithLaunch is ex (executor_select_test.go) plus a Describe carrying
// LaunchKinds, as a live executor would after self-reporting them at connect
// time.
func exWithLaunch(id string, labels map[string]string, admits string, launchKinds ...string) execpool.LiveExecutor {
	le := ex(id, labels, admits)
	le.Describe = &executorpb.DescribeResponse{LaunchKinds: launchKinds}
	return le
}

func TestChooseLaunchExecutorRequiresTheLaunchKind(t *testing.T) {
	c := selectFixture(t, "",
		ex("no-launch", map[string]string{"env": "home"}, ""),
		exWithLaunch("has-launch", map[string]string{"env": "home"}, "", "claude"),
	)
	exec, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, "", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if exec.ID != "has-launch" {
		t.Fatalf("want has-launch, got %s", exec.ID)
	}
}

func TestChooseLaunchExecutorRefusesWhenNoneAdvertiseTheKind(t *testing.T) {
	c := selectFixture(t, "", ex("no-launch", map[string]string{"env": "home"}, ""))
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, "", "claude")
	if err == nil || !strings.Contains(err.Error(), "does not support launching") {
		t.Fatalf("want a launch-kind refusal, got %v", err)
	}
}

func TestChooseLaunchExecutorStillHonoursLineageNarrowing(t *testing.T) {
	c := selectFixture(t, "env=home",
		exWithLaunch("work", map[string]string{"env": "work"}, "", "claude"),
		exWithLaunch("home", map[string]string{"env": "home"}, "", "claude"),
	)
	exec, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, "", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if exec.ID != "home" {
		t.Fatalf("parent's env=home set must still apply, got %s", exec.ID)
	}
}

func TestChooseLaunchExecutorRespectsAdmission(t *testing.T) {
	c := selectFixture(t, "",
		exWithLaunch("picky", map[string]string{"env": "home"}, "owner=someone-else", "claude"),
	)
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, "brent", "claude")
	if err == nil {
		t.Fatal("want a refusal — the executor's own admission selector excludes this owner")
	}
	if !strings.Contains(err.Error(), "admission selector") {
		t.Fatalf("want the refusal to name the admission selector, got: %v", err)
	}
}

func TestChooseLaunchExecutorIgnoresANonMatchingKind(t *testing.T) {
	c := selectFixture(t, "",
		exWithLaunch("fundi-only", map[string]string{"env": "home"}, "", "somethingelse"),
	)
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, "", "claude")
	if err == nil {
		t.Fatal("want a refusal — the executor advertises a different launch kind, not claude")
	}
}
