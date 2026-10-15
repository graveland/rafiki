// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/sched"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// TestPeriodicSweepsStopWhenContextCancelled pins the shutdown contract the
// old ticker's select{<-ctx.Done()} used to give: each periodic job re-arms
// only while ctx is live, so cancelling main's context stops it at the next
// fire and leaves no sweep: job armed on the scheduler.
func TestPeriodicSweepsStopWhenContextCancelled(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	clk := clock.NewFakeClock(time.Now())
	ctrl.clk = clk
	ctrl.sched = sched.New(clk, time.Second)
	rec := &sweepRecorder{Store: inbox.NewMemory()}
	ctrl.inbox = ctrl.newInboxQueue(rec)

	ctx, cancel := context.WithCancel(t.Context())
	ctrl.startPeriodicSweeps(ctx)

	clk.Advance(inboxSweepInterval)
	fired := len(rec.seen())
	ck.Eq(1, fired, "the inbox job must fire once before cancel; got %d", fired)

	cancel()
	clk.Advance(3 * inboxSweepInterval)
	ck.Eq(fired, len(rec.seen()), "a cancelled context must stop the sweeps; count grew from %d to %d", fired, len(rec.seen()))
	for _, e := range ctrl.sched.Pending() {
		ck.False(strings.HasPrefix(e.Key, "sweep:"), "a cancelled context left %q armed", e.Key)
	}
}

// TestPeriodicBudgetSweepSteersOnce pins the budget reconciliation as a
// self-re-arming scheduler job AND that mark()'s dedupe keeps it to ONE steer
// per breach across repeated fires: a job that re-injected every interval
// would interrupt a turn forever while the budget stayed exhausted.
func TestPeriodicBudgetSweepSteersOnce(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	c.clk = clk
	c.sched = sched.New(clk, time.Second)
	c.tasks = tasks.NewMemoryStore()
	c.coster = fakeCoster{spend: 99.00}
	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10.00 })

	c.startPeriodicSweeps(t.Context())

	// Two fires plus the eventbuf debounce window: the breach is announced
	// once, and the second fire re-steers nothing.
	clk.Advance(budgetSweepInterval)
	clk.Advance(budgetSweepInterval)
	clk.Advance(6 * time.Second)

	n := 0
	for _, b := range cap.batches() {
		if b.childID == "c_w1" && strings.Contains(strings.Join(b.fragments, " "), "budget") {
			n++
		}
	}
	ck.Eq(1, n, "steer batches to c_w1 = %d; mark() must dedupe across fires", n)
}
