// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/schedules"

	"github.com/multigres/testkit/assert"
)

// These pin the refresh triggers: the call sites that enqueue a display-state
// recompute when a fact behind waiting_on or the budget halt changes. Each
// drains the worker before asserting a publish.

// TestDisplayTriggerScheduleCreateAndDelete: creating a schedule makes the
// child wait on it; deleting it clears the wait.
func TestDisplayTriggerScheduleCreateAndDelete(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	c.schedules = schedules.NewMemory()
	c.daemonZone = "UTC"

	b := newScheduleBinding(c, "c_1")
	info, err := b.CronCreate(context.Background(), "*/5 * * * *", "wake and work", false)
	a.Require().NoError(err, "create the schedule")

	c.drainDisplayRefresh()
	last, ok := displayLastOf(c, "c_1")
	a.Require().True(ok, "creating a schedule publishes a display state")
	a.Eq(protocol.DisplayWaiting, last.state, "a child with an open schedule is waiting")
	a.Eq(protocol.WaitingOnSchedule, last.on, "waiting on the schedule")

	a.NoError(b.CronDelete(context.Background(), info.ID), "delete the schedule")
	c.drainDisplayRefresh()
	last, ok = displayLastOf(c, "c_1")
	a.Require().True(ok, "deleting a schedule publishes a display state")
	a.Eq(protocol.DisplayIdle, last.state, "with no open schedule the child is idle")
	a.Eq(protocol.WaitingOn(""), last.on, "idle waits on nothing")
}

// TestDisplayTriggerJobRunningAndExit: a running background job makes the
// child wait on it; the job's exit clears the wait. The events are fired from
// a separate goroutine so the test runs meaningfully under -race.
func TestDisplayTriggerJobRunningAndExit(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	// No event buffer: the exit fragment would otherwise leave the child
	// waiting/events, masking the job fact this test isolates.
	c.evbuf = nil

	running := make(chan struct{})
	go func() {
		defer close(running)
		c.handleJobEvent(jobRunningEvent("e1", "h1", "c_1", "make test"))
	}()
	<-running

	c.drainDisplayRefresh()
	last, ok := displayLastOf(c, "c_1")
	a.Require().True(ok, "a running job publishes a display state")
	a.Eq(protocol.DisplayWaiting, last.state, "a child with a running job is waiting")
	a.Eq(protocol.WaitingOnJob, last.on, "waiting on the job")

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		c.handleJobEvent(jobExitedEvent("e1", "h1", "c_1", "make test", 0))
	}()
	<-exited

	c.drainDisplayRefresh()
	last, ok = displayLastOf(c, "c_1")
	a.Require().True(ok, "a job exit publishes a display state")
	a.Eq(protocol.DisplayIdle, last.state, "with no running job the child is idle")
	a.Eq(protocol.WaitingOn(""), last.on, "idle waits on nothing")
}

// TestDisplayTriggerBudgetBreachAndMutation: a top-level child over budget
// waits on the operator; raising the cap clears it.
func TestDisplayTriggerBudgetBreachAndMutation(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.coster = fakeCoster{spend: 98.00}

	c.sweepBudgets(context.Background())
	c.drainDisplayRefresh()
	last, ok := displayLastOf(c, "c_1")
	a.Require().True(ok, "an over-budget child publishes a display state")
	a.Eq(protocol.DisplayWaiting, last.state, "an over-budget top-level child is waiting")
	a.Eq(protocol.WaitingOnOperator, last.on, "waiting on the operator")

	a.NoError(c.SetChildBudgetAsOperator(context.Background(), "c_1", 500.00), "raise the cap")
	c.drainDisplayRefresh()
	last, ok = displayLastOf(c, "c_1")
	a.Require().True(ok, "raising the cap publishes a display state")
	a.Eq(protocol.DisplayIdle, last.state, "a raised cap clears the operator wait")
	a.Eq(protocol.WaitingOn(""), last.on, "idle waits on nothing")
}

// TestDisplayTriggerTurnErrorSetsLastTurnError: an unclean outcome records the
// reason on the row; a clean one writes nothing; the next turn start clears it.
func TestDisplayTriggerTurnErrorSetsLastTurnError(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)

	c.recordTurnOutcome("c_1", fundi.TurnOutcome{Err: errors.New("upstream 500")})
	snap, _ := c.st.Get("c_1")
	a.Eq("upstream 500", snap.LastTurnError, "an unclean outcome records its error")

	// A clean outcome writes nothing: a pre-existing value survives.
	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.LastTurnError = "keeper" })
	c.recordTurnOutcome("c_1", fundi.TurnOutcome{Clean: true})
	snap, _ = c.st.Get("c_1")
	a.Eq("keeper", snap.LastTurnError, "a clean outcome must not touch the row")

	// The next turn start clears it.
	c.st.SetStatus("c_1", protocol.StatusIdle)
	c.handleStatusChange("c_1", protocol.StatusStreaming, protocol.StatusIdle)
	snap, _ = c.st.Get("c_1")
	a.Eq("", snap.LastTurnError, "a new turn clears the last turn's error")
}

// TestDisplayTriggerTurnErrorTextPrefersErr: the concrete error wins over the
// guardrail's limit reason.
func TestDisplayTriggerTurnErrorTextPrefersErr(t *testing.T) {
	a := assert.NewCollecting(t)
	a.Eq("boom", turnErrorText(fundi.TurnOutcome{Err: errors.New("boom"), LimitReason: "cost"}),
		"Err must win over LimitReason")
	a.Eq("cost", turnErrorText(fundi.TurnOutcome{LimitReason: "cost"}),
		"with no error, LimitReason is the reason")
}
