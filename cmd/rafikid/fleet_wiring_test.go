// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
	"github.com/prometheus/client_golang/prometheus"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// These pin the call sites of fleetMetrics, which a test of the type alone
// cannot: deleting a `c.fleet.*` call in the controller must fail one of them.

func TestSpawnCountsOneLaunchOfItsKind(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	reg := prometheus.NewRegistry()
	ctrl.fleet = newFleetMetrics(reg)

	spawnTestChild(t, ctrl, nil)

	ck.Eq(1.0, mustValue(t, reg, "rafiki_child_launches_total", map[string]string{"kind": protocol.KindClaude}), "one spawn, one launch")
}

func TestChildExitCountsOutcomeByKindAndNormalisesALegacyEmptyKind(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}
	c, _, _ := settleFixture(t)
	reg := prometheus.NewRegistry()
	c.fleet = newFleetMetrics(reg)

	exitOf := func(id, kind, script string) {
		c.st.Insert(&childstore.Session{ChildID: id, Name: id, Kind: kind, Status: protocol.StatusStreaming, StartedAt: time.Now()})
		ch := spawnExitingChild(t, id, script)
		c.cm.Add(id, ch)
		c.handleChildExit(id, ch)
	}
	exitOf("c_dead", protocol.KindClaude, "exit 1")
	exitOf("c_clean", protocol.KindFundi, "exit 0")
	exitOf("c_legacy", "", "exit 0")

	ck.Eq(1.0, mustValue(t, reg, "rafiki_child_exits_total", map[string]string{"kind": "claude", "outcome": "error"}), "non-zero exit")
	ck.Eq(2.0, mustValue(t, reg, "rafiki_child_exits_total", map[string]string{"kind": "fundi", "outcome": "clean"}),
		"a clean fundi exit and a legacy row with no kind, which is a fundi child")
	_, emptyKind := metricValue(t, reg, "rafiki_child_exits_total", map[string]string{"kind": "", "outcome": "clean"})
	ck.False(emptyKind, "no series may carry an empty kind")
}

func TestHeldBudgetWakeCountsADeferral(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	ctrl, _ := budgetHoldController(t)
	reg := prometheus.NewRegistry()
	ctrl.fleet = newFleetMetrics(reg)
	childID := spawnTestChild(t, ctrl, nil)
	ctrl.breaches.mark(childID)

	_, err := ctrl.inbox.Accept(ctx, inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Source: subagentEventSource, Text: "worker finished"})
	ck.NoError(err, "Accept")
	ck.True(errors.Is(ctrl.inbox.Deliver(ctx, childID, subagentEventSource), errInboxBudgetHeld), "the wake is held")

	ck.True(mustValue(t, reg, "rafiki_inbox_delivery_deferrals_total", map[string]string{"reason": "budget"}) >= 1, "budget deferral counted")
	_, scriptCounted := metricValue(t, reg, "rafiki_inbox_delivery_deferrals_total", map[string]string{"reason": "script"})
	ck.False(scriptCounted, "no script deferral happened")
}

func TestScriptChildDeliveryCountsADeferral(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()
	ctrl, _ := budgetHoldController(t)
	reg := prometheus.NewRegistry()
	ctrl.fleet = newFleetMetrics(reg)
	ctrl.st.Insert(&childstore.Session{ChildID: "c_script", Kind: protocol.KindScript, Status: protocol.StatusStreaming, StartedAt: time.Now()})

	_, err := ctrl.inbox.Accept(ctx, inbox.Inbound{ChildID: "c_script", Mode: inbox.ModePrompt, Source: subagentEventSource, Text: "hello"})
	ck.NoError(err, "Accept")
	ck.True(errors.Is(ctrl.inbox.Deliver(ctx, "c_script", subagentEventSource), errInboxDeferred), "deferred to the Receive stream")

	ck.True(mustValue(t, reg, "rafiki_inbox_delivery_deferrals_total", map[string]string{"reason": "script"}) >= 1, "script deferral counted")
}
