// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

func quietSnap(mutate func(*childstore.Snapshot)) childstore.Snapshot {
	s := childstore.Snapshot{NotifyPolicy: protocol.NotifyResult}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func TestWakesSpawnerDefaultPolicyAlwaysWakes(t *testing.T) {
	assert.NewAborting(t).True(wakesSpawner(childstore.Snapshot{}, false, false),
		"a default-policy child must wake its spawner on every turn end")
}

func TestWakesSpawnerQuietDropsPlainTurnEnd(t *testing.T) {
	assert.NewAborting(t).False(wakesSpawner(quietSnap(nil), false, false),
		"a quiet child with nothing recorded must not wake its spawner")
}

func TestWakesSpawnerQuietWakesOnResult(t *testing.T) {
	assert.NewAborting(t).True(wakesSpawner(quietSnap(func(s *childstore.Snapshot) { s.Result = `"x"` }), false, false))
}

func TestWakesSpawnerQuietWakesOnPendingQuestion(t *testing.T) {
	assert.NewAborting(t).True(wakesSpawner(quietSnap(func(s *childstore.Snapshot) { s.PendingQuestion = "which?" }), false, false))
}

func TestWakesSpawnerQuietWakesOnLastTurnError(t *testing.T) {
	assert.NewAborting(t).True(wakesSpawner(quietSnap(func(s *childstore.Snapshot) { s.LastTurnError = "boom" }), false, false))
}

func TestWakesSpawnerQuietWakesOnUncleanTurnOutcome(t *testing.T) {
	assert.NewAborting(t).True(wakesSpawner(quietSnap(nil), true, false))
}

func TestWakesSpawnerQuietWakesOnOwnCapBreach(t *testing.T) {
	assert.NewAborting(t).True(wakesSpawner(quietSnap(nil), false, true))
}

// quietFixture is the settle fixture with c_w1 made quiet.
func quietFixture(t *testing.T) (*Controller, *clock.FakeClock, *capturedFlush) {
	t.Helper()
	c, clk, cap := settleFixture(t)
	assert.NewAborting(t).NoError(c.st.Update("c_w1", func(s *childstore.Session) {
		s.NotifyPolicy = protocol.NotifyResult
	}))
	return c, clk, cap
}

func coordBatches(cap *capturedFlush) []capturedBatch {
	var out []capturedBatch
	for _, b := range cap.batches() {
		if b.childID == "c_coord" && b.source == subagentEventSource {
			out = append(out, b)
		}
	}
	return out
}

// quietMCPFixture adds a quiet TOP-LEVEL owned child and a live MCP session for
// its owner, the audience of the fan-out route.
func quietMCPFixture(t *testing.T, c *Controller, owner string) *settleSession {
	t.Helper()
	prev := mcpSettlements
	reg := newMCPSessions()
	mcpSettlements = reg
	t.Cleanup(func() { mcpSettlements = prev })
	ss := newSettleSession(t, "info")
	reg.Add(owner, ss.ss)
	c.st.Insert(&childstore.Session{
		ChildID: "c_quiet_top", Name: "quiet top", OwnerUserID: owner, NotifyPolicy: protocol.NotifyResult,
		Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	return ss
}

func TestNotifyTurnEndQuietChildSendsNoParentPushNoMCPFanout(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := quietFixture(t)
	ss := quietMCPFixture(t, c, "u-quiet-plain")

	c.notifyTurnEnd("c_w1")
	c.notifyTurnEnd("c_quiet_top")
	clk.Advance(6 * time.Second)

	ck.Empty(coordBatches(cap), "a quiet child's plain turn end must not push to its parent")
	ck.Eq("", assertSilence(t, ss.got), "a quiet child's plain turn end must not fan out to MCP sessions")
}

func TestNotifyTurnEndQuietChildWithResultSendsBoth(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := quietFixture(t)
	ss := quietMCPFixture(t, c, "u-quiet-result")
	for _, id := range []string{"c_w1", "c_quiet_top"} {
		ck.Require().NoError(c.st.Update(id, func(s *childstore.Session) { s.Result = `"done"` }))
	}

	c.notifyTurnEnd("c_w1")
	c.notifyTurnEnd("c_quiet_top")
	clk.Advance(6 * time.Second)

	batches := coordBatches(cap)
	ck.Require().Len(batches, 1, "a quiet child with a result must push to its parent")
	ck.StrContains(strings.Join(batches[0].fragments, "\n"), "final result of c_w1: done")
	ck.StrContains(waitFor(t, ss.got), "c_quiet_top", "the MCP fan-out must name the settled child")
}

func TestNotifyTurnEndQuietWakesOnUncleanOutcomeAlone(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := quietFixture(t)
	c.turnOutcomes.set("c_w1", fundi.TurnOutcome{Err: errors.New("provider down")})

	c.notifyTurnEnd("c_w1")
	clk.Advance(6 * time.Second)

	batches := coordBatches(cap)
	ck.Require().Len(batches, 1)
	ck.StrContains(strings.Join(batches[0].fragments, "\n"), "provider down", "the settle must carry the turn error as its reason")
}

func TestNotifyTurnEndQuietStillRunsResidueCheck(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := quietFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()
	ck.Require().NoError(c.st.Update("c_w1", func(s *childstore.Session) { s.SessionID = "conv-w1" }))
	_, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "half-done thing"}})
	ck.Require().NoError(err)

	c.notifyTurnEnd("c_w1")
	clk.Advance(6 * time.Second)

	var nudged bool
	for _, b := range cap.batches() {
		if b.childID == "c_w1" {
			nudged = true
		}
	}
	ck.True(nudged, "a dropped settle must still nudge the child about its own residue")
	ck.Empty(coordBatches(cap), "the dropped settle must not wake the spawner")
}

// A lookup miss takes the delivering path: the zero Snapshot a miss would
// yield is the default policy, which always wakes. The call must not panic and
// must leave the stored outcome consumed like any other settle.
func TestNotifyTurnEndMissingRowStillDelivers(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, _, _ := settleFixture(t)
	c.turnOutcomes.set("c_absent", fundi.TurnOutcome{Err: errors.New("boom")})

	c.notifyTurnEnd("c_absent")

	_, left := c.turnOutcomes.take("c_absent")
	ck.False(left, "the settle must consume the stored outcome")
	ck.True(wakesSpawner(childstore.Snapshot{}, false, false), "the zero Snapshot a miss yields must wake the spawner")
}

func TestQuietChildExitSettleIsNeverGated(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := quietFixture(t)
	ss := quietMCPFixture(t, c, "u-quiet-exit")

	c.notifySubagentSettled("c_w1", "exited", "", "")
	c.notifySubagentSettled("c_quiet_top", "exited", "", "")
	clk.Advance(6 * time.Second)

	batches := coordBatches(cap)
	ck.Require().Len(batches, 1, "a quiet child's exit must still wake its spawner")
	ck.StrContains(strings.Join(batches[0].fragments, "\n"), "exited")
	ck.StrContains(waitFor(t, ss.got), "exited", "a quiet child's exit must still fan out")
}

func TestQuietChildRestartFailureIsNeverGated(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := quietFixture(t)

	c.notifySubagentSettled("c_w1", "failed (daemon restarted)", "", "")
	clk.Advance(6 * time.Second)

	batches := coordBatches(cap)
	ck.Require().Len(batches, 1, "a quiet child's restart failure must still wake its spawner")
	ck.StrContains(strings.Join(batches[0].fragments, "\n"), "failed (daemon restarted)")
}

// TestQuietChildSettleSeesRowFieldsAtIdleTransition pins that every signal the
// gate reads is on the row (or in the outcome store) by the time the idle
// transition runs the settle: the engine reports its outcome (OnTurnEnded)
// before AgentEnd drives the status to idle, and a result or question is
// recorded by a tool call inside the turn.
func TestQuietChildSettleSeesRowFieldsAtIdleTransition(t *testing.T) {
	cases := []struct {
		name string
		set  func(c *Controller)
	}{
		{"result", func(c *Controller) {
			_ = c.st.Update("c_w1", func(s *childstore.Session) { s.Result = `"r"` })
		}},
		{"pending question", func(c *Controller) {
			_ = c.st.Update("c_w1", func(s *childstore.Session) { s.PendingQuestion = "which?" })
		}},
		{"unclean outcome", func(c *Controller) {
			c.recordTurnOutcome("c_w1", fundi.TurnOutcome{Err: errors.New("boom")})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c, clk, cap := quietFixture(t)
			tc.set(c)

			c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
			clk.Advance(6 * time.Second)

			ck.Len(coordBatches(cap), 1, "the idle transition must settle to the spawner")
		})
	}

	t.Run("nothing recorded", func(t *testing.T) {
		c, clk, cap := quietFixture(t)
		c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
		clk.Advance(6 * time.Second)
		assert.NewCollecting(t).Empty(coordBatches(cap), "a plain idle transition must stay quiet")
	})
}

func spawnNotifyReq(t *testing.T, notify, parent string) protocol.SpawnRequest {
	t.Helper()
	return protocol.SpawnRequest{
		Kind: protocol.KindClaude, Cwd: t.TempDir(), PiBinary: fakePiBin(t), NoSession: true,
		Notify: notify, ParentChildID: parent,
	}
}

func TestSpawnNotifyPersistsAndSurvivesResume(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctx := t.Context()

	res, err := ctrl.Spawn(ctx, spawnNotifyReq(t, protocol.NotifyResult, ""), users.Identity{})
	ck.Require().NoError(err, "spawn")
	ck.Eq(protocol.NotifyResult, mustSnapshot(t, ctrl, res.ChildID).NotifyPolicy)

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = ctrl.Kill(killCtx, res.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, ctrl.st, res.ChildID, 5*time.Second)

	_, err = ctrl.Resume(ctx, res.ChildID, "")
	ck.Require().NoError(err, "resume")
	ck.Eq(protocol.NotifyResult, mustSnapshot(t, ctrl, res.ChildID).NotifyPolicy,
		"the resumed Session literal dropped NotifyPolicy")

	req := resumeRequestFromSnapshot(childstore.Snapshot{
		Cwd: "/tmp", Kind: protocol.KindClaude, SessionID: "s", NotifyPolicy: protocol.NotifyResult,
	}, "")
	ck.Eq(protocol.NotifyResult, req.Notify, "the respawn request dropped NotifyPolicy")
}

func TestSpawnNotifyIsNotInherited(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctx := t.Context()

	parent, err := ctrl.Spawn(ctx, spawnNotifyReq(t, protocol.NotifyResult, ""), users.Identity{})
	ck.Require().NoError(err, "spawn parent")
	child, err := ctrl.Spawn(ctx, spawnNotifyReq(t, "", parent.ChildID), users.Identity{})
	ck.Require().NoError(err, "spawn child")
	ck.Eq("", mustSnapshot(t, ctrl, child.ChildID).NotifyPolicy, "a child must not take its parent's notify policy")

	all, err := ctrl.Spawn(ctx, spawnNotifyReq(t, protocol.NotifyAll, ""), users.Identity{})
	ck.Require().NoError(err, "spawn all")
	ck.Eq("", mustSnapshot(t, ctrl, all.ChildID).NotifyPolicy, "notify=all is stored as the empty default")
}

func TestSpawnNotifyUnknownValueRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	_, err := ctrl.Spawn(t.Context(), spawnNotifyReq(t, "none", ""), users.Identity{})
	ck.Require().Error(err)
	var ce *connectapi.ControllerError
	ck.Require().True(errors.As(err, &ce), "err = %v, want a *connectapi.ControllerError", err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code)
}
