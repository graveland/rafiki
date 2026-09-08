package main

import (
	"context"
	"fmt"
	"math"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// SetChildBudget changes childID's MaxCost. It is the single authority point
// for this whole feature — every caller (today: controllerSpawner.SetBudget,
// which backs the agent_set_budget tool) goes through this method rather
// than writing childstore directly.
//
// callerChildID must be childID's DIRECT parent per stored lineage
// (childstore.Store.ParentOf) — never trusted from an argument, matching how
// every AgentSpawner-backed verb reads stored state for authority rather
// than an argument (see controllerSpawner in agent_spawner.go). This is
// STRICTER than that file's authorize() helper, which permits ANY
// descendant (used by View/Send/Kill): a coordinator may change the budget
// only of a child it spawned itself, never a grandchild — that must go
// through the intermediate agent instead. A child is never its own direct
// parent, so this same check also makes self-mutation impossible with no
// separate special case.
//
// A raise (newCap > oldCap, or newCap == 0 meaning "make unlimited" when
// oldCap was finite) is checked against the CALLER's own remaining budget —
// the same arithmetic checkBudget (limits.go) already applies to a fresh
// spawn's grant, since the caller cannot hand out more room than it itself
// has left. A lower has no check at all: a coordinator may always tighten
// its own grant, and if the new cap already sits below current spend, the
// next sweepBudgets tick (or the child's own live cost guardrail) breaches
// it normally — no special case is needed here for that.
func (c *Controller) SetChildBudget(ctx context.Context, callerChildID, childID string, newCap float64) error {
	if newCap < 0 {
		return limitError(
			"set budget refused: max-cost cannot be negative (asked for $%.2f). Pass 0 to make it unlimited, or a positive amount",
			newCap)
	}

	parentOfTarget, ok := c.st.ParentOf(childID)
	if !ok || parentOfTarget != callerChildID {
		return fmt.Errorf(
			"agent %s is not a child you spawned directly; you may only change the budget of your own direct children, not a grandchild or another agent's child. Use agent_list to see your subtree",
			childID)
	}

	target, ok := c.st.Get(childID)
	if !ok {
		return fmt.Errorf("agent %s is not registered", childID)
	}

	if delta := budgetRaiseDelta(target.MaxCost, newCap); delta > 0 {
		caller, ok := c.st.Get(callerChildID)
		if !ok {
			return fmt.Errorf("agent %s is not registered", callerChildID)
		}
		if caller.MaxCost > 0 {
			qctx, cancel := context.WithTimeout(ctx, budgetQueryTimeout)
			spent, err := c.subtreeSpend(qctx, callerChildID)
			cancel()
			if err != nil {
				return limitError(
					"set budget refused: you have a $%.2f budget and your own spend could not be read (%v). A budget that cannot be checked is not enforced, so this fails closed",
					caller.MaxCost, err)
			}
			remaining := caller.MaxCost - spent
			if delta > remaining {
				return limitError(
					"set budget refused: you asked to raise %s's budget by $%.2f but only $%.2f of your own $%.2f budget remains (already spent: $%.2f). Raise it by at most the remainder",
					childID, delta, remaining, caller.MaxCost, spent)
			}
		}
	}

	return c.applyBudgetChange(childID, newCap)
}

// applyBudgetChange performs the write side of a budget mutation, shared by
// SetChildBudget (agent-facing, lineage-gated) and SetChildBudgetAsOperator
// (operator-facing, no lineage/remaining-budget check). Both callers have
// already validated newCap >= 0 and resolved childID to exist by the time
// this runs.
func (c *Controller) applyBudgetChange(childID string, newCap float64) error {
	wasBreached := c.budgetBreached(childID)
	if err := c.st.SetMaxCost(childID, newCap); err != nil {
		return fmt.Errorf("set budget: %w", err)
	}
	c.breaches.clear(childID)

	if wasBreached {
		c.notifyBudgetRaised(childID, newCap)
	}
	return nil
}

// SetChildBudgetAsOperator changes childID's MaxCost with OPERATOR authority:
// no lineage check (any child, at any depth, may be targeted — this backs
// the Connect SetBudget RPC, which is a control-plane verb, not an
// agent-facing tool) and no remaining-budget check (an operator is not
// spending out of a parent's grant, so there is nothing to check the raise
// against). Only the negative/non-finite cap rejection applies.
func (c *Controller) SetChildBudgetAsOperator(ctx context.Context, childID string, newCap float64) error {
	if newCap < 0 || math.IsNaN(newCap) || math.IsInf(newCap, 0) {
		return limitError(
			"set budget refused: max-cost cannot be negative or non-finite (asked for %v). Pass 0 to make it unlimited, or a positive amount",
			newCap)
	}
	if _, ok := c.st.Get(childID); !ok {
		return &control.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("agent %s is not registered", childID),
		}
	}
	return c.applyBudgetChange(childID, newCap)
}

// budgetRaiseDelta returns how much MORE room newCap grants over oldCap.
// Zero or negative means newCap is a lower or a no-op, and callers must
// treat that as "no check needed." newCap == 0 (unlimited) when oldCap was
// finite is treated as an unbounded raise (+Inf) — granting no limit at all
// cannot be expressed as a finite delta, and it must never be free just
// because 0 is small as a number.
func budgetRaiseDelta(oldCap, newCap float64) float64 {
	if newCap == 0 && oldCap != 0 {
		return math.Inf(1)
	}
	return newCap - oldCap
}

// notifyBudgetRaised tells every live member of childID's OWN subtree
// (childID itself plus its descendants — exactly the set applyBudgetBreach
// steered when it blocked them, cmd/rafikid/budget_sweep.go) that the budget
// was raised and it may resume. Only ever called when childID was actually
// marked breached; a proactive raise on a child that was never stuck needs
// no such message and would just be noise.
func (c *Controller) notifyBudgetRaised(childID string, newCap float64) {
	if c.evbuf == nil {
		return
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	capStr := "unlimited"
	if newCap > 0 {
		capStr = fmt.Sprintf("$%.2f", newCap)
	}
	msg := fmt.Sprintf(
		"Your budget was raised to %s by your coordinator. You may resume; unblock any tasks you need with task_update.",
		capStr)
	members := append([]childstore.Snapshot{snap}, c.st.Descendants(childID)...)
	for _, m := range members {
		if m.Status == protocol.StatusExited {
			continue
		}
		c.evbuf.PushSteer(m.ChildID, subagentEventSource, msg)
	}
}
