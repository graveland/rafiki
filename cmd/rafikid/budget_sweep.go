package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
)

// budgetBreaches records which budgeted roots are currently over. It exists so
// the sweep steers ONCE per breach: a periodic sweep that re-injected on every
// tick would interrupt a turn every interval for as long as the budget stayed
// exhausted, which is a denial of service the daemon inflicts on itself.
type budgetBreaches struct {
	mu sync.Mutex
	m  map[string]bool
}

func (b *budgetBreaches) mark(id string) (isNew bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = make(map[string]bool)
	}
	if b.m[id] {
		return false
	}
	b.m[id] = true
	return true
}

func (b *budgetBreaches) clear(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, id)
}

func (b *budgetBreaches) has(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[id]
}

func (c *Controller) budgetBreached(id string) bool { return c.breaches.has(id) }

// sweepBudgets checks every budgeted live agent's subtree spend.
//
// A ceiling hit mid-flight is deliberately NOT a kill. The agent is alive and
// the work is recoverable by raising the budget, so its unfinished tasks go
// BLOCKED — orphaned means "the owner is gone", and destroying that
// distinction destroys the only signal a coordinator has for telling a dead
// worker from a paused one.
//
// The notification is a STEER rather than a queued prompt, which is the narrow
// case eventbuf.PushSteer exists for: this event invalidates the turn already
// running. A worker that has lost its budget must not spend another 40 seconds
// planning spawns it can no longer make.
func (c *Controller) sweepBudgets(ctx context.Context) {
	if c.coster == nil {
		return
	}
	for _, snap := range c.st.List() {
		if snap.MaxCost <= 0 || snap.Status == protocol.StatusExited {
			continue
		}
		spent, err := c.subtreeSpend(ctx, snap.ChildID)
		if err != nil {
			// Best-effort here, unlike admission: this path cannot fail
			// closed usefully — refusing to sweep changes nothing, and
			// blocking every task in the fleet on a transient query error
			// would be far worse than a late breach.
			slog.Debug("budget sweep: spend unreadable", "childId", snap.ChildID, "error", err)
			continue
		}
		if spent < snap.MaxCost {
			c.breaches.clear(snap.ChildID)
			continue
		}
		if !c.breaches.mark(snap.ChildID) {
			continue // already reported; do not re-steer every tick
		}
		c.applyBudgetBreach(ctx, snap, spent)
	}
}

// applyBudgetBreach blocks the subtree's unfinished work and steers every live
// agent in it.
func (c *Controller) applyBudgetBreach(ctx context.Context, root childstore.Snapshot, spent float64) {
	slog.Warn("agent subtree over budget",
		"root", root.ChildID, "spent", spent, "budget", root.MaxCost)

	members := append([]childstore.Snapshot{root}, c.st.Descendants(root.ChildID)...)
	msg := fmt.Sprintf(
		"BUDGET EXHAUSTED: this agent tree has spent $%.2f of its $%.2f budget. "+
			"Stop starting new work — no further agents can be spawned. Your unfinished "+
			"tasks have been set to blocked; finish or hand off what is already in "+
			"flight, then stop. A human can raise the budget to resume.",
		spent, root.MaxCost)

	for _, m := range members {
		if m.Status == protocol.StatusExited {
			continue
		}
		c.blockTasksFor(ctx, m)
		if c.evbuf != nil {
			c.evbuf.PushSteer(m.ChildID, subagentEventSource, msg)
		}
	}
}

// blockTasksFor moves one agent's non-terminal rows to blocked.
func (c *Controller) blockTasksFor(ctx context.Context, snap childstore.Snapshot) {
	if c.tasks == nil || snap.SessionID == "" {
		return
	}
	rows, err := c.tasks.List(ctx, tasks.ListFilter{ConversationID: snap.SessionID})
	if err != nil {
		slog.Warn("budget sweep: task list failed", "childId", snap.ChildID, "error", err)
		return
	}
	var changes []tasks.Change
	for _, r := range rows {
		if r.Status.Terminal() || r.Status == tasks.StatusBlocked {
			continue
		}
		changes = append(changes, tasks.Change{Handle: r.Handle, Status: tasks.StatusBlocked})
	}
	if len(changes) == 0 {
		return
	}
	if _, err := c.tasks.Update(ctx, snap.SessionID, changes); err != nil {
		slog.Warn("budget sweep: could not block tasks", "childId", snap.ChildID, "error", err)
	}
}

// budgetSweepTimeout bounds each sweep cycle's query budget.
const budgetSweepTimeout = 30 * time.Second
