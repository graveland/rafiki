// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
)

// budgetHalted reports whether childID may not start agent work because it,
// or any ancestor, is over budget. c.breaches records only the budgeted node
// that breached, so the ancestor walk IS the predicate. root is the breached
// node. In-memory only: populated by sweepBudgets and, at startup, by
// seedBudgetBreaches.
func (c *Controller) budgetHalted(childID string) (root string, halted bool) {
	seen := make(map[string]bool)
	for id := childID; id != "" && !seen[id]; {
		seen[id] = true
		if c.breaches.has(id) {
			return id, true
		}
		p, ok := c.st.ParentOf(id)
		if !ok {
			break
		}
		id = p
	}
	return "", false
}

// seedBudgetBreaches marks every budgeted node whose subtree is already at or
// over its cap, WITHOUT steering. Run once at the end of the boot walk:
// c.breaches is in-memory and empty after a restart, and sweepBudgets skips
// exited rows — every recovered row is inserted exited before its async
// resume. Marking without steering is deliberate: the steer was delivered by
// the previous incarnation (it rides the durable inbox), and a marked breach
// makes sweepBudgets' mark() return false so it is not re-sent.
func (c *Controller) seedBudgetBreaches(ctx context.Context) {
	if c.coster == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, budgetSweepTimeout)
	defer cancel()
	for _, snap := range c.st.List() {
		if snap.MaxCost <= 0 {
			continue
		}
		spent, err := c.subtreeSpend(ctx, snap.ChildID)
		if err != nil {
			// Best-effort here, matching sweepBudgets: a boot-time seed
			// cannot fail the daemon usefully, and a PERSISTENT unreadable
			// spend leaves the tree's breach gate blind until the first
			// live sweep, so it is warned rather than silent.
			slog.Warn("budget seed: spend unreadable", "childId", snap.ChildID, "errorType", fmt.Sprintf("%T", err))
			continue
		}
		if spent >= snap.MaxCost {
			c.breaches.mark(snap.ChildID)
		}
	}
}
