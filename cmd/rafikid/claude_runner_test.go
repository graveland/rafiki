package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestClaudeRunnerFallsBackToNilWhenNoExecutorPool(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: nativebus.New()}
	runner, err := c.claudeRunner(protocol.SpawnRequest{Kind: protocol.KindClaude, Cwd: t.TempDir()}, "c_x", "", nil)
	assert.NewAborting(t).False(err != nil || runner != nil, "want (nil, nil) for the local-subprocess fallback, got (%v, %v)", runner, err)
}

func TestClaudeRunnerRefusesWhenNoLaunchCapableExecutor(t *testing.T) {
	c := &Controller{
		st:           childstore.New(),
		cm:           newChildManager(),
		native:       nativebus.New(),
		execPool:     &fakePool{live: []execpool.LiveExecutor{ex("no-launch", map[string]string{"env": "home"}, "")}},
		execPoolConn: execpool.New(nil),
		darajaPool:   darajapool.New(darajapool.NewRegistry()),
	}
	_, err := c.claudeRunner(protocol.SpawnRequest{Kind: protocol.KindClaude, Cwd: t.TempDir()}, "c_x", "", nil)
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "does not support launching"), "want a launch-kind refusal, got %v", err)
}

func TestDarajaLaunchExecutorRequiresThePinnedExecutorToBeLive(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{
		st:       childstore.New(),
		cm:       newChildManager(),
		native:   nativebus.New(),
		execPool: &fakePool{live: []execpool.LiveExecutor{ex("some-other-executor", map[string]string{"env": "home"}, "")}},
	}
	snap := &childstore.Snapshot{
		ChildID: "c_resumed",
		Labels:  map[string]string{"rafiki/executor": "the-original-executor"},
	}
	_, err := c.darajaLaunchExecutor(protocol.SpawnRequest{Kind: protocol.KindClaude}, "", snap)
	ck.Error(err, "want a refusal — the pinned executor is not in Live()")
	ck.False(!strings.Contains(err.Error(), "the-original-executor") || !strings.Contains(err.Error(), "not currently connected"), "want the refusal to name the pinned executor and say it is not connected, got: %v", err)
}

func TestDarajaLaunchExecutorRefusesWhenNoLabelWasRecorded(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: nativebus.New()}
	snap := &childstore.Snapshot{ChildID: "c_resumed", Labels: map[string]string{}}
	_, err := c.darajaLaunchExecutor(protocol.SpawnRequest{Kind: protocol.KindClaude}, "", snap)
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "no rafiki/executor label"), "want a refusal naming the missing label, got %v", err)
}

func TestDarajaLaunchExecutorUsesThePinnedExecutorWhenLive(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{
		st:     childstore.New(),
		cm:     newChildManager(),
		native: nativebus.New(),
		execPool: &fakePool{live: []execpool.LiveExecutor{
			exWithLaunch("the-original-executor", map[string]string{"env": "home"}, "", "claude"),
		}},
	}
	snap := &childstore.Snapshot{
		ChildID: "c_resumed",
		Labels:  map[string]string{"rafiki/executor": "the-original-executor"},
	}
	exec, err := c.darajaLaunchExecutor(protocol.SpawnRequest{Kind: protocol.KindClaude}, "", snap)
	ck.NoError(err)
	ck.Eq("the-original-executor", exec.ID, "got")
}
