package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// Over budget mid-flight is BLOCKED, not ORPHANED. orphaned means "the owner
// is gone"; here the agent is alive and the work is recoverable by raising the
// budget. Conflating them destroys the only signal that separates a dead agent
// from a paused one.
func TestOverBudgetBlocksTasksRatherThanOrphaningThem(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	c.coster = fakeCoster{spend: 99.00}
	ctx := context.Background()

	_ = c.st.Update("c_coord", func(s *childstore.Session) {
		s.MaxCost = 10.00
		s.SessionID = "conv-coord"
	})
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.SessionID = "conv-w1" })
	if _, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "in flight"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, "conv-w1", []tasks.Change{
		{Handle: "1", Status: tasks.StatusInProgress}}); err != nil {
		t.Fatal(err)
	}

	c.sweepBudgets(ctx)

	rows, err := store.List(ctx, tasks.ListFilter{ConversationID: "conv-w1"})
	ck.NoError(err)
	ck.Eq(tasks.StatusBlocked, rows[0].Status, "want blocked, got")
	ck.NotEq(tasks.StatusOrphaned, rows[0].Status, "orphaned means the owner is gone; this agent is alive")

	// And the agent is TOLD, as a steer: an over-budget worker must not spend
	// another 40 seconds believing it can still spawn and call models.
	clk.Advance(time.Second)
	var told bool
	for _, b := range cap.batches() {
		if b.childID == "c_w1" && strings.Contains(strings.Join(b.fragments, " "), "budget") {
			told = true
		}
	}
	ck.True(told, "an over-budget agent must be told mid-turn, not after it finishes")
}

func TestUnderBudgetSweepDoesNothing(t *testing.T) {
	c, _, cap := settleFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	c.coster = fakeCoster{spend: 1.00}
	ctx := context.Background()

	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.sweepBudgets(ctx)

	assert.NewAborting(t).Empty(cap.batches(), "an under-budget subtree must be silent")
}

// The sweep must not re-block and re-steer on every tick. Once told, an agent
// stays told until the budget is raised.
func TestBudgetSweepIsIdempotentWhileStillOverBudget(t *testing.T) {
	c, _, cap := settleFixture(t)
	c.tasks = tasks.NewMemoryStore()
	c.coster = fakeCoster{spend: 99.00}
	ctx := context.Background()
	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10.00 })

	c.sweepBudgets(ctx)
	before := len(cap.batches())
	c.sweepBudgets(ctx)
	c.sweepBudgets(ctx)
	assert.NewAborting(t).Eq(before, len(cap.batches()), "the sweep steered")
}

// Raising the budget must un-stick the subtree: the next sweep clears the
// breach so a later breach can be reported again.
func TestRaisingTheBudgetClearsTheBreach(t *testing.T) {
	c, _, _ := settleFixture(t)
	c.tasks = tasks.NewMemoryStore()
	c.coster = fakeCoster{spend: 99.00}
	ctx := context.Background()
	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.sweepBudgets(ctx)

	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 500.00 })
	c.sweepBudgets(ctx)
	assert.NewAborting(t).False(c.budgetBreached("c_coord"), "a raised budget must clear the breach")
}
