// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"
)

// schedAbandonAfter bounds how long shutdown waits for a running timer
// callback before abandoning it (and logging its key).
const schedAbandonAfter = 30 * time.Second

// now is the controller's clock; a test-constructed Controller with no clock
// reads the wall clock.
func (c *Controller) now() time.Time {
	if c.clk == nil {
		return time.Now()
	}
	return c.clk.Now()
}

// at registers fn under key; a no-op when the controller has no scheduler.
func (c *Controller) at(key string, deadline time.Time, fn func()) {
	c.sched.At(key, deadline, fn)
}

// cancelTimer cancels key; a no-op without a scheduler.
func (c *Controller) cancelTimer(key string) {
	c.sched.Cancel(key)
}

// heartbeatKey is the scheduler key for childID's next heartbeat check-in.
func heartbeatKey(childID string) string { return "heartbeat:" + childID }

// The two periodic reconciliations — the budget sweep and the inbox retention
// sweep — are scheduler jobs, not a ticker. Each re-arms itself under a fixed
// key one interval after it fires, so a single scheduler drives them alongside
// every deadline timer. Not finer than this without more thought: the interval
// itself is free, but the work is not — sweepBudgets runs a subtreeSpend query
// per over-budget subtree, so the DB load scales with 1/interval.
const (
	budgetSweepInterval = time.Minute
	inboxSweepInterval  = time.Minute
)

// Scheduler keys for the periodic jobs. Fixed strings so re-arming replaces
// the pending job rather than stacking a second one.
const (
	budgetSweepKey = "sweep:budgets"
	inboxSweepKey  = "sweep:inbox"
)

// startPeriodicSweeps arms the two periodic reconciliations as self-re-arming
// scheduler jobs. Each re-arms only while ctx is live, so cancelling main's
// context stops them at the next fire — the moment the old ticker goroutine
// returned — without waiting for Controller.Stop. Both first fire one interval
// after start.
func (c *Controller) startPeriodicSweeps(ctx context.Context) {
	c.at(budgetSweepKey, c.now().Add(budgetSweepInterval), func() { c.runBudgetSweep(ctx) })
	c.at(inboxSweepKey, c.now().Add(inboxSweepInterval), func() { c.runInboxSweep(ctx) })
}

// runBudgetSweep is one fire of the budget-reconciliation job. It re-arms
// BEFORE doing the work so a panic in the sweep cannot end the job for the
// daemon's life: pkg/sched recovers a callback panic and returns, so a re-arm
// placed last would simply never run and budget enforcement would stop
// silently. sched.run deletes this fire's pending entry before invoking the
// callback and At replaces by key, so arming here cannot stack a second job.
// The ctx check is FIRST: a cancelled context neither sweeps nor re-arms.
// sweepBudgets runs under a bounded child context so a stalled subtreeSpend
// query cannot outlive budgetSweepTimeout.
func (c *Controller) runBudgetSweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c.at(budgetSweepKey, c.now().Add(budgetSweepInterval), func() { c.runBudgetSweep(ctx) })
	sctx, cancel := context.WithTimeout(ctx, budgetSweepTimeout)
	defer cancel()
	c.sweepBudgets(sctx)
}

// runInboxSweep is one fire of the inbox-retention job, and re-arms exactly
// like runBudgetSweep — before the work, so a panic in the sweep still leaves
// the job armed: it stops at the first fire after ctx ends.
func (c *Controller) runInboxSweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c.at(inboxSweepKey, c.now().Add(inboxSweepInterval), func() { c.runInboxSweep(ctx) })
	c.sweepInbox()
}
