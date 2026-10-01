// SPDX-License-Identifier: Apache-2.0

package main

// Tests for pending_resume.go: the executor-gated recovery sweep and the
// claude restart auto-resume's gates.

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/darajapool"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// exDescribe is ex() plus the self-reported Describe a launch-capability
// check reads (claudeExecutorLive).
func exDescribe(id string, launchKinds []string) execpool.LiveExecutor {
	le := ex(id, map[string]string{}, "")
	if launchKinds != nil {
		le.Describe = &executorpb.DescribeResponse{LaunchKinds: launchKinds}
	}
	return le
}

// claudeRec is a recovered claude row that reads ALIVE.
func claudeRec(status string, labels map[string]string) childstore.ChildRecord {
	if labels == nil {
		labels = map[string]string{}
	}
	return childstore.ChildRecord{
		ChildID:  "c_claude",
		Kind:     protocol.KindClaude,
		DaemonID: "me",
		Status:   status,
		Labels:   labels,
	}
}

// TestClaudeRowsAutoResumeOnlyWhenOwnedByMe pins the claude recovery gate: a
// claude conversation has NO lease to fence with, so the row's daemon_id
// classification is the only proof this daemon may run it. A claude row whose
// daemon this daemon cannot prove is its own past incarnation loads exited and
// stays dead — resuming an adopted claude row could double-run a conversation
// whose original daemon is still alive.
func TestClaudeRowsAutoResumeOnlyWhenOwnedByMe(t *testing.T) {
	ctx := context.Background()
	live := map[string]bool{}

	t.Run("foreign lapsed loads exited, never resumed", func(t *testing.T) {
		logs := captureLogs(t)
		c := newTestController(t)
		c.daemonID = "me"

		rec := claudeRec("streaming", map[string]string{"rafiki/daemon": "other-daemon"})
		rec.DaemonID = "other-daemon"
		rec.UpdatedAt = time.Now().Add(-2 * foreignFreshGrace) // past the fresh grace
		c.recoverOne(ctx, rec, live)

		assert.NewCollecting(t).NotStrContains(logs.String(), "auto-resuming child",
			"an adopted claude row must not be auto-resumed; log:\n%s", logs.String())
	})

	t.Run("unclaimed never resumed", func(t *testing.T) {
		logs := captureLogs(t)
		c := newTestController(t)
		c.daemonID = "me"

		rec := claudeRec("idle", nil)
		rec.DaemonID = "" // no daemon ever stamped it
		c.recoverOne(ctx, rec, live)

		assert.NewCollecting(t).NotStrContains(logs.String(), "auto-resuming child",
			"an unclaimed claude row must not be auto-resumed; log:\n%s", logs.String())
	})

	t.Run("own past incarnation resumes", func(t *testing.T) {
		logs := captureLogs(t)
		c := newTestController(t)
		c.daemonID = "me"

		c.recoverOne(ctx, claudeRec("idle", map[string]string{"rafiki/daemon": "me"}), live)

		assert.NewCollecting(t).StrContains(logs.String(), "auto-resuming child",
			"this daemon's own claude row must auto-resume; log:\n%s", logs.String())
	})
}

// TestClaudeWasWorkingAtDeath pins the continuation-prompt signal: the row's
// status, with the pre-shutdown label standing in for `shutting_down` (which
// the graceful path writes over idle and mid-turn children alike).
func TestClaudeWasWorkingAtDeath(t *testing.T) {
	cases := []struct {
		name string
		rec  childstore.ChildRecord
		want bool
	}{
		{"streaming at crash", claudeRec("streaming", nil), true},
		{"tool_running at crash", claudeRec("tool_running", nil), true},
		{"idle at crash", claudeRec("idle", nil), false},
		{"spawning at crash", claudeRec("spawning", nil), false},
		{"shutting_down with a working pre-status", claudeRec("shutting_down",
			map[string]string{preShutdownStatusLabel: "streaming"}), true},
		{"shutting_down from idle", claudeRec("shutting_down",
			map[string]string{preShutdownStatusLabel: "idle"}), false},
		{"shutting_down with no stamp", claudeRec("shutting_down", nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NewCollecting(t).Eq(tc.want, claudeWasWorkingAtDeath(tc.rec), "claudeWasWorkingAtDeath")
		})
	}
}

// TestHandleStatusChangeStampsPreShutdownStatus pins the durable signal
// claudeWasWorkingAtDeath reads: the shutting_down transition records what the
// child was doing before the overwrite; no other transition writes the label.
func TestHandleStatusChangeStampsPreShutdownStatus(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager()}

	c.st.Insert(&childstore.Session{
		ChildID: "c_1", Kind: protocol.KindClaude, Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	c.handleStatusChange("c_1", protocol.StatusShuttingDown, protocol.StatusStreaming)
	snap, ok := c.st.Get("c_1")
	ck := assert.NewAborting(t)
	ck.Require().True(ok, "child present")
	ck.Eq("streaming", snap.Labels[preShutdownStatusLabel], "pre-shutdown status label")

	// An idle child's shutting_down stamps NOTHING: the label exists to
	// distinguish "was working" from "was idle", and absence reads as
	// not-working (claudeWasWorkingAtDeath).
	c.st.Insert(&childstore.Session{
		ChildID: "c_2", Kind: protocol.KindClaude, Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	c.handleStatusChange("c_2", protocol.StatusShuttingDown, protocol.StatusIdle)
	snap2, _ := c.st.Get("c_2")
	ck.Eq("", snap2.Labels[preShutdownStatusLabel], "an idle child gets no stamp")

	// No stamp on ordinary transitions.
	c.handleStatusChange("c_2", protocol.StatusStreaming, protocol.StatusShuttingDown)
	snap3, _ := c.st.Get("c_2")
	ck.Eq("", snap3.Labels[preShutdownStatusLabel], "a working transition leaves the stamp alone")
}

// TestClaudePendingUntilItsPinnedExecutorConnects pins the executor gate: a
// recovered daraja-hosted claude child whose pinned executor is not connected
// pends instead of resuming (the launch would refuse outright), and the sweep
// fired by an executor connection launches it — on EVERY connect, since
// multiple executors are standard and the child's requirement may only be
// satisfied by the one that connects last.
func TestClaudePendingUntilItsPinnedExecutorConnects(t *testing.T) {
	ctx := context.Background()
	ck := assert.NewAborting(t)
	logs := captureLogs(t)

	c := newTestController(t)
	c.daemonID = "me"
	// daraja-routed: execPoolConn must be non-nil (concrete pool) and so must
	// darajaPool. The interface the predicates READ is the fake, so the test
	// controls what is live.
	c.execPoolConn = execpool.New(newFakeExecStore())
	c.darajaPool = darajapool.New(darajapool.NewRegistry())
	pool := &fakePool{}
	c.execPool = pool

	rec := claudeRec("streaming", map[string]string{
		"rafiki/daemon":   "me",
		"rafiki/executor": "e_home",
	})
	c.recoverOne(ctx, rec, nil)
	// recoverOne is called directly here, not through loadChildren, so mirror
	// the walk's completion (loadChildren marks this after its loop).
	c.recoveryWalk.markDone()

	ck.StrContains(logs.String(), "deferring auto-resume until the child's executor connects",
		"the resume must have pended; log:\n%s", logs.String())
	ck.Eq(1, c.pendingCount(), "pending count")
	ck.NotStrContains(logs.String(), "auto-resuming child", "a pended child must not resume yet")

	// A connect that does not satisfy the child's pin: the sweep leaves it
	// pending. (Run directly — the real trigger is Pool.OnConnect, which
	// main.go wires to this same method.)
	pool.live = []execpool.LiveExecutor{exDescribe("e_other", nil)}
	c.sweepPendingResumes()
	ck.Eq(1, c.pendingCount(), "a connect that satisfies nothing must not resume the child")

	// The pinned executor connects and advertises claude launch.
	pool.live = []execpool.LiveExecutor{exDescribe("e_home", []string{"claude"})}
	c.sweepPendingResumes()
	ck.StrContains(logs.String(), "auto-resuming child", "the sweep must launch the pending resume; log:\n%s", logs.String())
	ck.Eq(0, c.pendingCount(), "the resumed child leaves the pending set")
}

// TestSweepIgnoresAnUnfinishedWalk pins the walk gate: a sweep arriving during
// the boot walk (an executor connecting mid-walk) must not fire half the walk's
// pendings; the walk's own tail sweep covers them.
func TestSweepIgnoresAnUnfinishedWalk(t *testing.T) {
	ck := assert.NewAborting(t)
	logs := captureLogs(t)
	c := newTestController(t)
	c.daemonID = "me"

	c.pendResume(pendingResume{
		rec: childstore.ChildRecord{ChildID: "c_p", Kind: protocol.KindClaude, Status: "idle"},
	})
	c.sweepPendingResumes() // recoveryWalk.done is still false
	ck.Eq(1, c.pendingCount(), "a mid-walk sweep must not fire pendings")
	ck.NotStrContains(logs.String(), "auto-resuming child", "log:\n%s", logs.String())

	c.recoveryWalk.markDone()
	c.sweepPendingResumes()
	ck.Eq(0, c.pendingCount(), "the walk's own tail sweep (or a later one) fires it")
}

// TestCloseDropsAPendingResume pins the resurrection guard: a child closed or
// killed while it waits for its executor must never be launched by a sweep.
func TestCloseDropsAPendingResume(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newTestController(t)
	c.daemonID = "me"

	sess := childstore.SessionFromRecord(claudeRec("exited", map[string]string{"rafiki/daemon": "me"}))
	c.st.Insert(sess)

	c.pendResume(pendingResume{rec: claudeRec("exited", map[string]string{"rafiki/daemon": "me"})})
	ck.Eq(1, c.pendingCount(), "pending before close")

	err := c.Close("c_claude")
	ck.NoError(err, "Close")
	ck.Eq(0, c.pendingCount(), "a closed child must not stay pending")
}
