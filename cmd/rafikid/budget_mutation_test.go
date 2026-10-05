package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestSetChildBudgetRaiseWithinRemainingSucceeds(t *testing.T) {
	ck := assert.NewAborting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1, both grant depth 3
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 5.00} // c_d0's whole subtree has spent $5 of its $10

	ck.NoError(c.SetChildBudget(context.Background(), "c_d0", "c_d1", 6.00), "raise within remaining ($5 left) must succeed")
	snap, _ := c.st.Get("c_d1")
	ck.Eq(6.00, snap.MaxCost, "c_d1.MaxCost")
}

func TestSetChildBudgetRaiseExceedingRemainingIsRefused(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 9.00} // only $1 left under c_d0's $10

	err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 5.00) // asks for +$3, only $1 left
	ck.Require().Error(err, "a raise of $3 with only $1 remaining must be refused")
	for _, want := range []string{"3.00", "1.00", "10.00"} {
		ck.StrContains(err.Error(), want, "refusal must show the delta, remaining and the caller's budget; missing %q in %v", want, err)
	}
}

func TestSetChildBudgetRaiseToUnlimitedUnderABudgetedParentIsRefused(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 1.00}

	assert.NewAborting(t).Error(c.SetChildBudget(context.Background(), "c_d0", "c_d1", 0), "raising a child to unlimited under a BUDGETED parent must be refused — it is an unbounded raise")
}

func TestSetChildBudgetRaiseToUnlimitedUnderAnUnbudgetedParentSucceeds(t *testing.T) {
	ck := assert.NewAborting(t)
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	// c_d0 itself is unbudgeted (MaxCost left at its zero value) — no ceiling to check.
	c.coster = fakeCoster{spend: 1000.00}

	ck.NoError(c.SetChildBudget(context.Background(), "c_d0", "c_d1", 0), "an unbudgeted parent must be able to grant unlimited")
	snap, _ := c.st.Get("c_d1")
	ck.Eq(0, snap.MaxCost, "c_d1.MaxCost")
}

func TestSetChildBudgetLowerIsAlwaysAllowedEvenBelowCurrentSpend(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 8.00 })
	c.coster = fakeCoster{spend: 9.99} // c_d0 is nearly out of budget

	// Lowering c_d1 to $1 is well below what c_d1's own subtree may have
	// already spent — that is fine, no check applies to a lower at all.
	assert.NewAborting(t).NoError(c.SetChildBudget(context.Background(), "c_d0", "c_d1", 1.00), "a lower must never be refused")
}

func TestSetChildBudgetNegativeIsRefused(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", -5.00)
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "negative"), "a negative cap must be refused and named as such: %v", err)
}

func TestSetChildBudgetRefusesAGrandchild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3, 3) // c_d0 -> c_d1 -> c_d2
	err := c.SetChildBudget(context.Background(), "c_d0", "c_d2", 5.00)
	ck.Require().Error(err, "c_d2 is c_d0's GRANDchild, not its direct child — must be refused")
	ck.StrContains(err.Error(), "c_d2", "refusal must name the target: %v", err)
}

func TestSetChildBudgetRefusesSelf(t *testing.T) {
	c := limitsFixture(t, 3)
	err := c.SetChildBudget(context.Background(), "c_d0", "c_d0", 5.00)
	assert.NewAborting(t).Error(err, "an agent is never its own direct child — self-mutation must be refused")
}

// A raise on a child that was never breached needs no resume message —
// only an actually-stuck child gets told it may resume.
func TestSetChildBudgetOnANeverBreachedChildSendsNoSteer(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	c.coster = fakeCoster{spend: 1.00}
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.MaxCost = 10.00 })

	ck.NoError(c.SetChildBudget(context.Background(), "c_coord", "c_w1", 20.00), "SetChildBudget")
	clk.Advance(time.Second)
	ck.Empty(cap.batches(), "a raise on a never-breached child must send no steer, got")
}

// Raising a BREACHED child's budget must clear the breach and tell every
// live member of ITS subtree (not the whole coordinator's subtree) that it
// may resume — the same member set applyBudgetBreach steered when it blocked
// them (cmd/rafikid/budget_sweep.go's applyBudgetBreach).
func TestSetChildBudgetOnABreachedChildClearsItAndSteersResume(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	c.tasks = nil // budget_sweep's blockTasksFor no-ops without a tasks store; irrelevant here
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.coster = fakeCoster{spend: 99.00}
	c.sweepBudgets(context.Background()) // breaches c_w1
	ck.True(c.budgetBreached("c_w1"), "test setup: sweepBudgets should have breached c_w1")

	ck.NoError(c.SetChildBudget(context.Background(), "c_coord", "c_w1", 500.00), "SetChildBudget")
	ck.False(c.budgetBreached("c_w1"), "a raise must clear the breach mark")

	clk.Advance(time.Second)
	var told bool
	for _, b := range cap.batches() {
		if b.childID == "c_w1" && strings.Contains(strings.Join(b.fragments, " "), "resume") {
			told = true
		}
	}
	ck.True(told, "a raise on a previously breached child must steer it that it may resume")
}

// A raise on a child whose caller has a budget must FAIL CLOSED when the
// caller's own spend cannot be read: a budget that cannot be checked is not
// enforced, and treating an unreadable lineage as "spend is zero" would let a
// closed descendant's spend vanish from the rollup. Mirrors
// TestCheckSpawnLimitsFailsClosedOnALineageError for the SetChildBudget path.
func TestSetChildBudgetFailsClosedOnALineageError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 0}
	c.lineage = &fakeLineageSource{err: errors.New("lineage down")}

	err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 5.00)
	ck.Require().Error(err, "a budgeted caller whose lineage cannot be read must not be allowed to raise a child's budget")
	ck.StrContains(err.Error(), "could not be read", "the refusal must read as the fail-closed budget message; got %v", err)
}

func TestSetChildBudgetAsOperatorCanRaiseARootCoordinator(t *testing.T) {
	ck := assert.NewAborting(t)
	c := limitsFixture(t, 3) // a single root child, c_d0
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5.00 })

	ck.NoError(c.SetChildBudgetAsOperator(context.Background(), "c_d0", 50.00), "operator raise on a root coordinator must succeed")
	snap, _ := c.st.Get("c_d0")
	ck.Eq(50.00, snap.MaxCost, "c_d0.MaxCost")
}

func TestSetChildBudgetAsOperatorCanRaiseADeepDescendantWithNoRemainingCheck(t *testing.T) {
	ck := assert.NewAborting(t)
	c := limitsFixture(t, 3, 3, 3) // c_d0 -> c_d1 -> c_d2
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 1.00 })
	_ = c.st.Update("c_d2", func(s *childstore.Session) { s.MaxCost = 1.00 })
	c.coster = fakeCoster{spend: 1.00} // c_d0's whole subtree already at its $1 cap

	// c_d2 is a GRANDCHILD of c_d0, and the raise is far larger than c_d0's
	// own remaining budget ($0) -- both would refuse SetChildBudget. Neither
	// check applies to the operator path.
	ck.NoError(c.SetChildBudgetAsOperator(context.Background(), "c_d2", 999.00), "operator raise on a deep descendant must skip lineage and remaining-budget checks")
	snap, _ := c.st.Get("c_d2")
	ck.Eq(999.00, snap.MaxCost, "c_d2.MaxCost")
}

func TestSetChildBudgetAsOperatorNegativeIsRefused(t *testing.T) {
	c := limitsFixture(t, 3)
	err := c.SetChildBudgetAsOperator(context.Background(), "c_d0", -5.00)
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "negative"), "a negative cap must be refused and named as such: %v", err)
}

func TestSetChildBudgetAsOperatorNonFiniteIsRefused(t *testing.T) {
	c := limitsFixture(t, 3)
	for _, nonFinite := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		err := c.SetChildBudgetAsOperator(context.Background(), "c_d0", nonFinite)
		assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "negative or non-finite"), "non-finite cap %v must be refused: %v", nonFinite, err)
	}
}

func TestSetChildBudgetAsOperatorUnknownChildIsNotFound(t *testing.T) {
	ck := assert.NewAborting(t)
	c := limitsFixture(t, 3)
	err := c.SetChildBudgetAsOperator(context.Background(), "c_unknown", 5.00)
	ck.Error(err, "expected error on unknown child, got nil")
	var ce *connectapi.ControllerError
	ck.True(errors.As(err, &ce), "expected *connectapi.ControllerError, got %T: %v", err, err)
	ck.Eq(protocol.ErrNotFound, ce.Code, "ce.Code")
	ck.StrContains(ce.Message, "c_unknown", "ce.Message")
}

func TestSetChildBudgetAsOperatorZeroMeansUnlimitedAndIsAccepted(t *testing.T) {
	ck := assert.NewAborting(t)
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5.00 })

	ck.NoError(c.SetChildBudgetAsOperator(context.Background(), "c_d0", 0), "0 must be accepted as an explicit unlimited request")
	snap, _ := c.st.Get("c_d0")
	ck.Eq(0, snap.MaxCost, "c_d0.MaxCost")
}
