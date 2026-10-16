// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

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
