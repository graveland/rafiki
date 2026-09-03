package main

import (
	"context"
	"fmt"
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

// heartbeatState tracks, per child, whether it is currently in an unbroken
// working spell, when it last got a heartbeat pushed to its parent, and when
// the current spell was first observed. Guarded by its own mutex, matching
// budgetBreaches and turnOutcomeStore.
type heartbeatState struct {
	mu      sync.Mutex
	working map[string]struct{}

	// lastSent advances on every real fire — see due().
	lastSent map[string]time.Time

	// spellSince is seeded at the SAME moment as lastSent's first entry for a
	// child (due()'s lazy-seed branch) but, unlike lastSent, is NEVER
	// overwritten again for that spell — it stays fixed at the spell's start
	// until stopWorking clears it. sweepHeartbeats reads it to report elapsed
	// time in the check-in fragment.
	spellSince map[string]time.Time
}

// startWorking marks a child as being in a working spell. Called from
// handleStatusChange on a transition INTO a working status; a call for a
// child already in the map is a no-op (a stray extra transition between two
// working statuses, e.g. streaming -> tool_running, must not reset the
// window).
func (h *heartbeatState) startWorking(childID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.working == nil {
		h.working = make(map[string]struct{})
	}
	h.working[childID] = struct{}{}
}

// stopWorking clears a child's working spell and heartbeat history. Called on
// a transition OUT of a working status (back to idle), so the next spell
// starts its own window rather than inheriting a stale one.
func (h *heartbeatState) stopWorking(childID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.working, childID)
	delete(h.lastSent, childID)
	delete(h.spellSince, childID)
}

// due reports whether childID, currently tracked as working, has gone at
// least interval since it was last observed by a sweep — and if so, records
// now as its new lastSent.
//
// working (set by startWorking) gates only PRESENCE — "is this child in a
// spell we are tracking at all." It deliberately carries no timestamp: the
// elapsed-time arithmetic is measured entirely in sweepHeartbeats' own
// caller-supplied clock, seeded lazily on the FIRST sweep that notices a
// newly (re)tracked child (below), rather than from workingSince/whenever
// handleStatusChange happened to run. This costs real precision — a spell's
// very first heartbeat fires up to one extra sweep tick later than a true
// elapsed-since-start design would give it, regardless of how short
// heartbeatInterval is configured, because that first sweep only starts the
// clock rather than measuring against it — but it is the only way this stays
// correct: sweepHeartbeats is driven by whatever `now` its caller passes
// (real time.Now() from sweepTick in production, a manually-advanced test
// clock in tests), while handleStatusChange's startWorking has no access to
// that same clock and must not pretend to.
func (h *heartbeatState) due(childID string, now time.Time, interval time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, tracked := h.working[childID]; !tracked {
		return false
	}
	if h.lastSent == nil {
		h.lastSent = make(map[string]time.Time)
	}
	last, sent := h.lastSent[childID]
	if !sent {
		h.lastSent[childID] = now
		// Seeded once, at this same first-observation moment, and never
		// touched again until stopWorking clears it alongside lastSent —
		// see spellSince's doc.
		if h.spellSince == nil {
			h.spellSince = make(map[string]time.Time)
		}
		h.spellSince[childID] = now
		return false
	}
	if now.Sub(last) < interval {
		return false
	}
	h.lastSent[childID] = now
	return true
}

// since reports when childID's current working spell was first observed —
// see spellSince's doc. Returns the zero Time if childID has no tracked
// spell (never seen by due(), or one that has already gone idle). Callers
// must diff it against their OWN caller-supplied clock, never time.Now(),
// for the same reason due() takes now as a parameter.
func (h *heartbeatState) since(childID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spellSince[childID]
}

// sweepHeartbeats pushes one coalesced check-in fragment to each working
// child's parent, for every child that has been continuously working past
// c.heartbeatInterval since its last one. Zero interval disables the feature
// entirely (see NewController's flag wiring).
//
// This rides the same 5-minute reconciliation tick as sweepBudgets and
// sweepExpired (see sweepTick) rather than a ticker of its own: all three are
// periodic reconciliations of stored state, and a fourth ticker would be a
// fourth thing to reason about at shutdown.
//
// ctx bounds the subtreeSpend query below, exactly as sweepBudgets is
// bounded — sweepTick passes the SAME sweepCtx (budgetSweepTimeout) into
// both, so a stalled cost query here cannot hang the shared sweep goroutine
// any more than one in the budget sweep already could.
func (c *Controller) sweepHeartbeats(ctx context.Context, now time.Time) {
	if c.heartbeatInterval <= 0 || c.evbuf == nil {
		return
	}
	for _, snap := range c.st.List() {
		if !isWorkingStatus(snap.Status) {
			continue
		}
		parent, ok := c.st.ParentOf(snap.ChildID)
		if !ok || parent == "" {
			continue // top-level: nobody to check in with
		}
		if !c.heartbeats.due(snap.ChildID, now, c.heartbeatInterval) {
			continue
		}

		// Build the parenthetical from whatever data is actually available.
		// Elapsed time comes from the sweep's OWN clock (now), never
		// time.Now() — see heartbeatState.since's doc. A failed or
		// unconfigured cost query OMITS the cost clause entirely rather than
		// asserting a false "$0.00", mirroring sweepBudgets' own
		// best-effort handling of the identical situation (budget_sweep.go).
		var parts []string
		if since := c.heartbeats.since(snap.ChildID); !since.IsZero() {
			parts = append(parts, now.Sub(since).Round(time.Minute).String()+" so far")
		}
		if c.coster != nil {
			if spent, err := c.subtreeSpend(ctx, snap.ChildID); err == nil {
				parts = append(parts, fmt.Sprintf("this agent and its subagents have spent $%.2f", spent))
			}
		}
		var detail string
		if len(parts) > 0 {
			detail = " (" + strings.Join(parts, "; ") + ")"
		}

		name := snap.Name
		if name == "" {
			name = "unnamed"
		}
		frag := fmt.Sprintf(
			"agent %s (%s) is still working%s. No action needed unless you want to check in.",
			snap.ChildID, name, detail)
		c.evbuf.Push(parent, heartbeatEventSource, snap.ChildID, frag)
	}
}
