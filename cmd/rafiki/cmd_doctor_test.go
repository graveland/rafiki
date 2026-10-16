// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestDoctorAllPass: a daemon that answers, an eligible executor, and a claude
// agent that replies — doctor succeeds and closes the child.
func TestDoctorAllPass(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.stub.spawnChildID = "c_smoke"
	f.stub.events = []*rafikiv1.Event{assistantEvent("c_smoke", "hello"), turnEndEvent("c_smoke")}

	var buf bytes.Buffer
	c.NoError(runDoctor(context.Background(), newDoctorCmd(), &buf), "doctor must pass")
	out := buf.String()
	c.StrContains(out, "ok    daemon", "the daemon check line is missing:\n")
	c.StrContains(out, "ok    executor", "the executor check line is missing:\n")
	c.StrContains(out, "ok    agent", "the agent check line is missing:\n")
	c.Eq(1, f.stub.closeCalls, "the doctor child must be closed")
}

// TestDoctorAgentNoReplyFailsAndCloses: a turn that ends with no assistant text
// is a failure, and the child is still closed.
func TestDoctorAgentNoReplyFailsAndCloses(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.stub.spawnChildID = "c_smoke"
	f.stub.events = []*rafikiv1.Event{turnEndEvent("c_smoke")}

	var buf bytes.Buffer
	err := runDoctor(context.Background(), newDoctorCmd(), &buf)
	c.Error(err, "an agent that never replies must fail doctor")
	c.StrContains(err.Error(), "1 check(s) failed", "the failure summary is missing:\n")
	c.StrContains(buf.String(), "FAIL  agent", "the agent failure line is missing:\n")
	c.Eq(1, f.stub.closeCalls, "the doctor child must be closed even on failure")
}

// TestDoctorNoExecutorSkipsAgent: no eligible executor fails the executor check
// and skips the agent check — nothing is spawned.
func TestDoctorNoExecutorSkipsAgent(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.stub.executors = nil

	var buf bytes.Buffer
	err := runDoctor(context.Background(), newDoctorCmd(), &buf)
	c.Error(err, "a machine with no executor must fail doctor")
	out := buf.String()
	c.StrContains(out, "FAIL  executor", "the executor failure line is missing:\n")
	c.StrContains(out, "skip  agent", "the agent check must be skipped:\n")
	c.Eq(0, f.stub.spawnCalls, "the agent check must not spawn when the executor check fails")
}

// TestDoctorAnyNonEmptyReplyPasses: doctor only requires SOME reply — "Okay!"
// passes, pinning that the old strict OK check is gone.
func TestDoctorAnyNonEmptyReplyPasses(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.stub.spawnChildID = "c_smoke"
	f.stub.events = []*rafikiv1.Event{assistantEvent("c_smoke", "Okay!"), turnEndEvent("c_smoke")}

	var buf bytes.Buffer
	c.NoError(runDoctor(context.Background(), newDoctorCmd(), &buf), "any non-empty reply must pass")
	c.StrContains(buf.String(), "ok    agent", "the agent check line is missing:\n")
}

func TestParseDoctorAgents(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := parseDoctorAgents([]string{"claude", "fundi:openrouter/~deepseek/deepseek-flash-latest"})
	c.Require().NoError(err, "valid specs")
	c.EqDeep([]doctorAgent{
		{kind: protocol.KindClaude},
		{kind: protocol.KindFundi, model: "openrouter/~deepseek/deepseek-flash-latest"},
	}, got, "parsed agents")
	for _, bad := range []string{"", "fundi", "fundi:", "script:x", "claude:sonnet"} {
		_, err := parseDoctorAgents([]string{bad})
		c.Error(err, "parseDoctorAgents(%q) must fail", bad)
	}
}

// TestDoctorFundiAgentSpawnsWithModel: a fundi-only run spawns a fundi child on
// the named model and never asks for a claude launch executor.
func TestDoctorFundiAgentSpawnsWithModel(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.stub.executors = nil // a fundi agent needs no launch executor
	f.stub.spawnChildID = "c_fundi"
	f.stub.events = []*rafikiv1.Event{assistantEvent("c_fundi", "hi"), turnEndEvent("c_fundi")}

	cmd := newDoctorCmd()
	c.NoError(cmd.Flags().Set("agent", "fundi:openrouter/~deepseek/deepseek-flash-latest"), "set --agent")
	var buf bytes.Buffer
	c.NoError(runDoctor(context.Background(), cmd, &buf), "doctor with a fundi agent")
	out := buf.String()
	c.NotStrContains(out, "executor", "a fundi-only run must not check the claude executor:\n")
	c.StrContains(out, "ok    agent fundi:openrouter/~deepseek/deepseek-flash-latest: replied", "the fundi agent line is missing:\n")
	c.Eq(1, len(f.stub.spawnReqs), "spawn count")
	c.Eq(protocol.KindFundi, f.stub.spawnReqs[0].GetKind(), "spawn kind")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest", f.stub.spawnReqs[0].GetModel(), "spawn model")
	c.Eq(0, f.stub.listCalls, "a fundi-only run must not list executors")
}

// TestDoctorNoExecutorStillRunsFundiAgent: a failed executor check skips the
// claude agent but not a fundi one, and both failures are counted.
func TestDoctorNoExecutorStillRunsFundiAgent(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.stub.executors = nil
	f.stub.spawnChildID = "c_fundi"
	f.stub.events = []*rafikiv1.Event{turnEndEvent("c_fundi")}

	cmd := newDoctorCmd()
	c.NoError(cmd.Flags().Set("agent", "claude"), "set --agent claude")
	c.NoError(cmd.Flags().Set("agent", "fundi:anthropic/haiku-latest"), "set --agent fundi")
	var buf bytes.Buffer
	err := runDoctor(context.Background(), cmd, &buf)
	c.Error(err, "doctor must fail")
	c.StrContains(err.Error(), "2 check(s) failed", "executor + fundi no-reply must both count:\n")
	out := buf.String()
	c.StrContains(out, "skip  agent\n", "the claude agent must be skipped:\n")
	c.StrContains(out, "FAIL  agent fundi:anthropic/haiku-latest: no reply", "the fundi agent must still run:\n")
	c.Eq(1, f.stub.spawnCalls, "only the fundi agent spawns")
}
