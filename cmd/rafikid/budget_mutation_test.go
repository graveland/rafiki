package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
)

func TestSetChildBudgetRaiseWithinRemainingSucceeds(t *testing.T) {
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1, both grant depth 3
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 5.00} // c_d0's whole subtree has spent $5 of its $10

	if err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 6.00); err != nil {
		t.Fatalf("raise within remaining ($5 left) must succeed: %v", err)
	}
	snap, _ := c.st.Get("c_d1")
	if snap.MaxCost != 6.00 {
		t.Fatalf("c_d1.MaxCost = %v, want 6.00", snap.MaxCost)
	}
}

func TestSetChildBudgetRaiseExceedingRemainingIsRefused(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 9.00} // only $1 left under c_d0's $10

	err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 5.00) // asks for +$3, only $1 left
	if err == nil {
		t.Fatal("a raise of $3 with only $1 remaining must be refused")
	}
	for _, want := range []string{"3.00", "1.00", "10.00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must show the delta, remaining and the caller's budget; missing %q in %v", want, err)
		}
	}
}

func TestSetChildBudgetRaiseToUnlimitedUnderABudgetedParentIsRefused(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	c.coster = fakeCoster{spend: 1.00}

	if err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 0); err == nil {
		t.Fatal("raising a child to unlimited under a BUDGETED parent must be refused — it is an unbounded raise")
	}
}

func TestSetChildBudgetRaiseToUnlimitedUnderAnUnbudgetedParentSucceeds(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2.00 })
	// c_d0 itself is unbudgeted (MaxCost left at its zero value) — no ceiling to check.
	c.coster = fakeCoster{spend: 1000.00}

	if err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 0); err != nil {
		t.Fatalf("an unbudgeted parent must be able to grant unlimited: %v", err)
	}
	snap, _ := c.st.Get("c_d1")
	if snap.MaxCost != 0 {
		t.Fatalf("c_d1.MaxCost = %v, want 0 (unlimited)", snap.MaxCost)
	}
}

func TestSetChildBudgetLowerIsAlwaysAllowedEvenBelowCurrentSpend(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10.00 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 8.00 })
	c.coster = fakeCoster{spend: 9.99} // c_d0 is nearly out of budget

	// Lowering c_d1 to $1 is well below what c_d1's own subtree may have
	// already spent — that is fine, no check applies to a lower at all.
	if err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", 1.00); err != nil {
		t.Fatalf("a lower must never be refused: %v", err)
	}
}

func TestSetChildBudgetNegativeIsRefused(t *testing.T) {
	c := limitsFixture(t, 3, 3)
	err := c.SetChildBudget(context.Background(), "c_d0", "c_d1", -5.00)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative cap must be refused and named as such: %v", err)
	}
}

func TestSetChildBudgetRefusesAGrandchild(t *testing.T) {
	c := limitsFixture(t, 3, 3, 3) // c_d0 -> c_d1 -> c_d2
	err := c.SetChildBudget(context.Background(), "c_d0", "c_d2", 5.00)
	if err == nil {
		t.Fatal("c_d2 is c_d0's GRANDchild, not its direct child — must be refused")
	}
	if !strings.Contains(err.Error(), "c_d2") {
		t.Errorf("refusal must name the target: %v", err)
	}
}

func TestSetChildBudgetRefusesSelf(t *testing.T) {
	c := limitsFixture(t, 3)
	err := c.SetChildBudget(context.Background(), "c_d0", "c_d0", 5.00)
	if err == nil {
		t.Fatal("an agent is never its own direct child — self-mutation must be refused")
	}
}

// A raise on a child that was never breached needs no resume message —
// only an actually-stuck child gets told it may resume.
func TestSetChildBudgetOnANeverBreachedChildSendsNoSteer(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.coster = fakeCoster{spend: 1.00}
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.MaxCost = 10.00 })

	if err := c.SetChildBudget(context.Background(), "c_coord", "c_w1", 20.00); err != nil {
		t.Fatalf("SetChildBudget: %v", err)
	}
	clk.Advance(time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a raise on a never-breached child must send no steer, got: %+v", got)
	}
}

// Raising a BREACHED child's budget must clear the breach and tell every
// live member of ITS subtree (not the whole coordinator's subtree) that it
// may resume — the same member set applyBudgetBreach steered when it blocked
// them (cmd/rafikid/budget_sweep.go's applyBudgetBreach).
func TestSetChildBudgetOnABreachedChildClearsItAndSteersResume(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.tasks = nil // budget_sweep's blockTasksFor no-ops without a tasks store; irrelevant here
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.MaxCost = 10.00 })
	c.coster = fakeCoster{spend: 99.00}
	c.sweepBudgets(context.Background()) // breaches c_w1
	if !c.budgetBreached("c_w1") {
		t.Fatal("test setup: sweepBudgets should have breached c_w1")
	}

	if err := c.SetChildBudget(context.Background(), "c_coord", "c_w1", 500.00); err != nil {
		t.Fatalf("SetChildBudget: %v", err)
	}
	if c.budgetBreached("c_w1") {
		t.Fatal("a raise must clear the breach mark")
	}

	clk.Advance(time.Second)
	var told bool
	for _, b := range cap.batches() {
		if b.childID == "c_w1" && strings.Contains(strings.Join(b.fragments, " "), "resume") {
			told = true
		}
	}
	if !told {
		t.Fatal("a raise on a previously breached child must steer it that it may resume")
	}
}

func TestSetChildBudgetAsOperatorCanRaiseARootCoordinator(t *testing.T) {
	c := limitsFixture(t, 3) // a single root child, c_d0
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5.00 })

	if err := c.SetChildBudgetAsOperator(context.Background(), "c_d0", 50.00); err != nil {
		t.Fatalf("operator raise on a root coordinator must succeed: %v", err)
	}
	snap, _ := c.st.Get("c_d0")
	if snap.MaxCost != 50.00 {
		t.Fatalf("c_d0.MaxCost = %v, want 50.00", snap.MaxCost)
	}
}

func TestSetChildBudgetAsOperatorCanRaiseADeepDescendantWithNoRemainingCheck(t *testing.T) {
	c := limitsFixture(t, 3, 3, 3) // c_d0 -> c_d1 -> c_d2
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 1.00 })
	_ = c.st.Update("c_d2", func(s *childstore.Session) { s.MaxCost = 1.00 })
	c.coster = fakeCoster{spend: 1.00} // c_d0's whole subtree already at its $1 cap

	// c_d2 is a GRANDCHILD of c_d0, and the raise is far larger than c_d0's
	// own remaining budget ($0) -- both would refuse SetChildBudget. Neither
	// check applies to the operator path.
	if err := c.SetChildBudgetAsOperator(context.Background(), "c_d2", 999.00); err != nil {
		t.Fatalf("operator raise on a deep descendant must skip lineage and remaining-budget checks: %v", err)
	}
	snap, _ := c.st.Get("c_d2")
	if snap.MaxCost != 999.00 {
		t.Fatalf("c_d2.MaxCost = %v, want 999.00", snap.MaxCost)
	}
}

func TestSetChildBudgetAsOperatorNegativeIsRefused(t *testing.T) {
	c := limitsFixture(t, 3)
	err := c.SetChildBudgetAsOperator(context.Background(), "c_d0", -5.00)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative cap must be refused and named as such: %v", err)
	}
}

func TestSetChildBudgetAsOperatorZeroMeansUnlimitedAndIsAccepted(t *testing.T) {
	c := limitsFixture(t, 3)
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 5.00 })

	if err := c.SetChildBudgetAsOperator(context.Background(), "c_d0", 0); err != nil {
		t.Fatalf("0 must be accepted as an explicit unlimited request: %v", err)
	}
	snap, _ := c.st.Get("c_d0")
	if snap.MaxCost != 0 {
		t.Fatalf("c_d0.MaxCost = %v, want 0 (unlimited)", snap.MaxCost)
	}
}
