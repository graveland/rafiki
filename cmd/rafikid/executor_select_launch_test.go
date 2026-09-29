package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
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
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		ex("no-launch", map[string]string{"env": "home"}, ""),
		exWithLaunch("has-launch", map[string]string{"env": "home"}, "", "claude"),
	)
	exec, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	ck.NoError(err)
	ck.Eq("has-launch", exec.ID, "want has-launch, got")
}

func TestChooseLaunchExecutorRefusesWhenNoneAdvertiseTheKind(t *testing.T) {
	c := selectFixture(t, "", ex("no-launch", map[string]string{"env": "home"}, ""))
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "does not support launching"), "want a launch-kind refusal, got %v", err)
}

func TestChooseLaunchExecutorStillHonoursLineageNarrowing(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "env=home",
		exWithLaunch("work", map[string]string{"env": "work"}, "", "claude"),
		exWithLaunch("home", map[string]string{"env": "home"}, "", "claude"),
	)
	exec, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	ck.NoError(err)
	ck.Eq("home", exec.ID, "parent's env=home set must still apply, got")
}

func TestChooseLaunchExecutorRespectsAdmission(t *testing.T) {
	ck := assert.NewAborting(t)
	c := selectFixture(t, "",
		exWithLaunch("picky", map[string]string{"env": "home"}, "owner=someone-else", "claude"),
	)
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{Name: "brent"}, "claude")
	ck.Error(err, "want a refusal — the executor's own admission selector excludes this owner")
	ck.StrContains(err.Error(), "admission selector", "want the refusal to name the admission selector, got: %v", err)
}

func TestChooseLaunchExecutorIgnoresANonMatchingKind(t *testing.T) {
	c := selectFixture(t, "",
		exWithLaunch("fundi-only", map[string]string{"env": "home"}, "", "somethingelse"),
	)
	_, err := c.chooseLaunchExecutor(protocol.SpawnRequest{ParentChildID: "c_parent"}, executorOwner{}, "claude")
	assert.NewAborting(t).Error(err, "want a refusal — the executor advertises a different launch kind, not claude")
}
