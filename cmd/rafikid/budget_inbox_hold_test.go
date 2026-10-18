// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func budgetHoldController(t *testing.T) (*Controller, *inbox.Memory) {
	t.Helper()
	ctrl := newTestController(t)
	st := inbox.NewMemory()
	ctrl.inbox = ctrl.newInboxQueue(st)
	return ctrl, st
}

func pendingTexts(t *testing.T, st inbox.Store, childID string) []string {
	t.Helper()
	rows, err := st.Pending(context.Background(), childID)
	assert.NewAborting(t).NoError(err, "Pending")
	var out []string
	for _, r := range rows {
		out = append(out, r.Text)
	}
	return out
}

func hasFrame(ch interface{ InSnapshot() [][]byte }, typ, containing string) bool {
	for _, raw := range ch.InSnapshot() {
		var f childFrame
		if json.Unmarshal(raw, &f) == nil && f.Type == typ && strings.Contains(f.Message, containing) {
			return true
		}
	}
	return false
}

// A sender is told the child cannot run rather than being queued behind it,
// and nothing is persisted: a coordinator that learns this can raise the
// budget or stop retrying.
func TestSendToABudgetHaltedChildIsRefusedAndQueuesNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, st := budgetHoldController(t)
	childID := spawnTestChild(t, ctrl, nil)
	ctrl.breaches.mark(childID)

	for _, frame := range []string{
		`{"type":"prompt","message":"are you there"}`,
		`{"type":"steer","message":"are you there"}`,
	} {
		err := ctrl.Send(childID, json.RawMessage(frame))
		var ce *connectapi.ControllerError
		ck.True(errors.As(err, &ce), "%s: want a ControllerError, got %v", frame, err)
		ck.Eq(protocol.ErrBudgetExhausted, ce.Code, "%s: reason", frame)
		ck.StrContains(ce.Message, childID, "the refusal names the child")
	}
	ck.Empty(pendingTexts(t, st, childID), "a refused send must persist nothing")

	ctrl.breaches.clear(childID)
	ck.NoError(ctrl.Send(childID, json.RawMessage(`{"type":"prompt","message":"back to work"}`)), "send after the breach cleared")
	waitForChildFrame(t, ctrl, childID, "prompt")
}

// A system wake (a job exit, a subagent report) has no sender to tell, so it
// stays in the durable queue while the child is over budget, and goes out the
// moment the budget is raised. The breach notice itself is a steer and must
// still reach the child.
func TestWakeForABudgetHaltedChildIsHeldUntilTheBudgetIsRaised(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	ctrl, st := budgetHoldController(t)
	childID := spawnTestChild(t, ctrl, nil)
	ch, ok := ctrl.cm.Get(childID)
	ck.True(ok, "child %s is not live", childID)
	ctrl.breaches.mark(childID)

	_, err := ctrl.inbox.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Source: subagentEventSource, Text: "worker finished"})
	ck.NoError(err, "Accept wake")
	err = ctrl.inbox.Deliver(ctx, childID, subagentEventSource)
	ck.True(errors.Is(err, errInboxBudgetHeld), "delivery to a halted child = %v, want the held sentinel", err)
	ck.EqDeep([]string{"worker finished"}, pendingTexts(t, st, childID), "the wake must stay pending")
	ck.False(hasFrame(ch, "prompt", "worker finished"), "a halted child was written a wake")

	// The breach notice is a steer on a different source and is exempt.
	_, err = ctrl.inbox.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModeSteer, Source: "budget-notice", Text: "BUDGET EXHAUSTED"})
	ck.NoError(err, "Accept steer")
	ck.NoError(ctrl.inbox.Deliver(ctx, childID, "budget-notice"), "a steer must pass the hold")
	waitForChildFrame(t, ctrl, childID, "steer")

	ck.NoError(ctrl.SetChildBudgetAsOperator(ctx, childID, 500), "SetChildBudgetAsOperator")

	waitForChildFrame(t, ctrl, childID, "prompt")
	ck.True(hasFrame(ch, "prompt", "worker finished"), "the held wake was not delivered after the raise")
	ck.Empty(pendingTexts(t, st, childID), "nothing may stay pending once the budget is raised")
}

// Raising a coordinator's budget un-halts its whole subtree, so a descendant's
// held wake is released too, not only the coordinator's own.
func TestRaisingACoordinatorsBudgetReleasesItsDescendantsHeldWakes(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	ctrl, st := budgetHoldController(t)
	coord := spawnTestChild(t, ctrl, nil)
	worker := spawnTestChildWithParent(t, ctrl, coord)
	ch, ok := ctrl.cm.Get(worker)
	ck.True(ok, "worker %s is not live", worker)
	ctrl.breaches.mark(coord)

	_, err := ctrl.inbox.Accept(ctx, inbox.Inbound{ChildID: worker, Mode: inbox.ModePrompt, Source: subagentEventSource, Text: "sibling finished"})
	ck.NoError(err, "Accept wake")
	err = ctrl.inbox.Deliver(ctx, worker, subagentEventSource)
	ck.True(errors.Is(err, errInboxBudgetHeld), "delivery under a breached ancestor = %v, want the held sentinel", err)

	ck.NoError(ctrl.SetChildBudgetAsOperator(ctx, coord, 500), "SetChildBudgetAsOperator")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !hasFrame(ch, "prompt", "sibling finished") {
		time.Sleep(10 * time.Millisecond)
	}
	ck.True(hasFrame(ch, "prompt", "sibling finished"), "the descendant's held wake was not released")
	ck.Empty(pendingTexts(t, st, worker), "nothing may stay pending")
}

// A breach can also clear without anyone raising the budget (spend read back
// under the cap on the next sweep); the held wakes must not wait for a raise
// that is never coming.
func TestSweepClearingABreachReleasesTheHeldWakes(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	ctrl, st := budgetHoldController(t)
	childID := spawnTestChild(t, ctrl, nil)
	ch, ok := ctrl.cm.Get(childID)
	ck.True(ok, "child %s is not live", childID)
	ck.NoError(ctrl.st.Update(childID, func(s *childstore.Session) { s.MaxCost = 10 }), "set budget")
	ctrl.coster = fakeCoster{spend: 1}
	ctrl.breaches.mark(childID)

	_, err := ctrl.inbox.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Source: subagentEventSource, Text: "worker finished"})
	ck.NoError(err, "Accept wake")
	ck.True(errors.Is(ctrl.inbox.Deliver(ctx, childID, subagentEventSource), errInboxBudgetHeld), "the wake must be held while the breach stands")

	ctrl.sweepBudgets(ctx)

	ck.False(ctrl.budgetBreached(childID), "the sweep must clear a breach whose spend is back under the cap")
	waitForChildFrame(t, ctrl, childID, "prompt")
	ck.True(hasFrame(ch, "prompt", "worker finished"), "the held wake was not released by the sweep")
	ck.Empty(pendingTexts(t, st, childID), "nothing may stay pending")
}
