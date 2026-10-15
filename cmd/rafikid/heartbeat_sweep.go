// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// heartbeatEventSource is the event-buffer source name for periodic
// check-ins, kept distinct from subagentEventSource so a heartbeat coalesces
// independently of settle and budget-breach fragments — a coordinator should
// never lose a "your worker just finished" notice underneath a routine
// check-in that happened to land in the same debounce window.
const heartbeatEventSource = "subagents-heartbeat"

// defaultHeartbeatInterval is how long a child works continuously before its
// parent gets a check-in. Overridable with RAFIKI_HEARTBEAT_INTERVAL.
//
// The heartbeat is a deadline on the scheduler (see armHeartbeat), not a tick
// sweep: the first check-in of a spell lands at exactly this interval after
// the spell started, not at the next coarse reconciliation tick. The value is
// chosen for how stale a coordinator's view of subagent spend may get, not
// for any prompt-cache TTL — see CLAUDE.md on why the cache reading of this
// number does not survive contact with the data.
const defaultHeartbeatInterval = 4 * time.Minute

// heartbeatState tracks, per child, when its current unbroken working spell
// started. A spell is a run of consecutive working statuses; the elapsed time
// a check-in reports is measured from its start. Guarded by its own mutex,
// matching budgetBreaches and turnOutcomeStore.
type heartbeatState struct {
	mu         sync.Mutex
	spellSince map[string]time.Time
}

// startWorking records the start of a working spell at now and reports
// whether it is NEW (a working→working transition must not reset it). A false
// return means the child was already in a spell, so the caller must NOT arm a
// fresh deadline under it.
func (h *heartbeatState) startWorking(childID string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.spellSince == nil {
		h.spellSince = make(map[string]time.Time)
	}
	if _, tracked := h.spellSince[childID]; tracked {
		return false
	}
	h.spellSince[childID] = now
	return true
}

// stopWorking clears a child's working spell. Called on a transition OUT of a
// working status (back to idle) and on exit, so the next spell starts its own
// window rather than inheriting a stale one.
func (h *heartbeatState) stopWorking(childID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.spellSince, childID)
}

// since reports when childID's current working spell started. The bool is
// false when the child has no tracked spell (never started one, or one that
// has already gone idle), which is distinct from a zero Time.
func (h *heartbeatState) since(childID string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.spellSince[childID]
	return t, ok
}

// armHeartbeat registers childID's next check-in one heartbeatInterval from
// now. Top-level children have nobody to check in with and are never armed.
// Zero interval disables the feature entirely (see NewController's flag
// wiring), as does a missing event buffer.
func (c *Controller) armHeartbeat(childID string) {
	if c.heartbeatInterval <= 0 || c.evbuf == nil {
		return
	}
	if parent, ok := c.st.ParentOf(childID); !ok || parent == "" {
		return // top-level: nobody to check in with
	}
	c.at(heartbeatKey(childID), c.now().Add(c.heartbeatInterval), func() {
		c.fireHeartbeat(childID)
	})
}

// fireHeartbeat pushes one check-in to childID's parent and re-arms, unless
// the child has vanished or stopped working (then it returns WITHOUT
// re-arming — the vanished-subject rule: a timer for a subject that is gone
// must not keep rescheduling itself).
//
// The parenthetical is built from whatever data is actually available.
// Elapsed time comes from the spell's recorded start, measured on the
// controller's clock. A failed or unconfigured cost query OMITS the cost
// clause entirely rather than asserting a false "$0.00", mirroring
// sweepBudgets' own best-effort handling of the identical situation
// (budget_sweep.go).
func (c *Controller) fireHeartbeat(childID string) {
	snap, ok := c.st.Get(childID)
	if !ok || !isWorkingStatus(snap.Status) {
		return
	}
	since, tracked := c.heartbeats.since(childID)
	if !tracked {
		return
	}
	parent, ok := c.st.ParentOf(childID)
	if !ok || parent == "" {
		return
	}

	var parts []string
	parts = append(parts, c.now().Sub(since).Round(time.Minute).String()+" so far")
	if c.coster != nil {
		ctx, cancel := context.WithTimeout(context.Background(), budgetSweepTimeout)
		spent, err := c.subtreeSpend(ctx, childID)
		cancel()
		if err == nil {
			parts = append(parts, fmt.Sprintf("this agent and its subagents have spent $%.2f", spent))
		} else {
			// The cost clause is omitted, not asserted as $0.00 — but a
			// persistent unreadable spend (lineage blind) must be visible, so
			// warn rather than swallow it.
			slog.Warn("heartbeat sweep: spend unreadable", "childId", childID, "errorType", fmt.Sprintf("%T", err))
		}
	}
	detail := " (" + strings.Join(parts, "; ") + ")"

	name := snap.Name
	if name == "" {
		name = "unnamed"
	}
	frag := fmt.Sprintf(
		"agent %s (%s) is still working%s. No action needed unless you want to check in.",
		snap.ChildID, name, detail)
	c.evbuf.Push(parent, heartbeatEventSource, childID, frag)

	// Re-arm for the next interval: only a child still working and still
	// present reaches here, and the next fire re-checks both.
	c.armHeartbeat(childID)
}
