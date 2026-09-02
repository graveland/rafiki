package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// heartbeatEventSource is the event-buffer source name for periodic
// check-ins, kept distinct from subagentEventSource so a heartbeat coalesces
// independently of settle and budget-breach fragments — a coordinator should
// never lose a "your worker just finished" notice underneath a routine
// check-in that happened to land in the same debounce window.
const heartbeatEventSource = "subagents-heartbeat"

// heartbeatState tracks, per child, when its current unbroken working spell
// began and when it last got a heartbeat pushed to its parent. Guarded by its
// own mutex, matching budgetBreaches and turnOutcomeStore.
type heartbeatState struct {
	mu           sync.Mutex
	workingSince map[string]time.Time
	lastSent     map[string]time.Time
}

// startWorking records the beginning of a working spell. Called from
// handleStatusChange on a transition INTO a working status; a call for a
// child already in the map is a no-op (a stray extra transition between two
// working statuses, e.g. streaming -> tool_running, must not reset the
// window).
func (h *heartbeatState) startWorking(childID string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.workingSince == nil {
		h.workingSince = make(map[string]time.Time)
	}
	if _, ok := h.workingSince[childID]; !ok {
		h.workingSince[childID] = now
	}
}

// stopWorking clears a child's working spell and heartbeat history. Called on
// a transition OUT of a working status (back to idle), so the next spell
// starts its own window rather than inheriting a stale one.
func (h *heartbeatState) stopWorking(childID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.workingSince, childID)
	delete(h.lastSent, childID)
}

// due reports whether childID, currently tracked as working, has gone at
// least interval since it was last observed by a sweep — and if so, records
// now as its new lastSent.
//
// workingSince (set by startWorking, from handleStatusChange's own
// time.Now()) gates only PRESENCE here — "is this child in a spell we are
// tracking at all" — never the elapsed-time arithmetic. The elapsed
// comparison is measured entirely in the caller's clock, seeded from the
// FIRST sweep that notices a tracked child (below): sweepHeartbeats only
// ever runs on the periodic reconciliation tick, so a heartbeat can never
// fire more precisely than once per tick regardless, and deriving elapsed
// from two different clocks (handleStatusChange's real wall time and
// whatever sweepHeartbeats is driven by) would make the interval math
// incoherent — the daemon calls both with time.Now() so this is invisible
// in production, but a test driving sweepHeartbeats off a manual clock while
// handleStatusChange still calls the real one would see elapsed durations
// that don't correspond to anything simulated.
func (h *heartbeatState) due(childID string, now time.Time, interval time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, tracked := h.workingSince[childID]; !tracked {
		return false
	}
	if h.lastSent == nil {
		h.lastSent = make(map[string]time.Time)
	}
	last, sent := h.lastSent[childID]
	if !sent {
		h.lastSent[childID] = now
		return false
	}
	if now.Sub(last) < interval {
		return false
	}
	h.lastSent[childID] = now
	return true
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
func (c *Controller) sweepHeartbeats(now time.Time) {
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
		spent := 0.0
		if c.coster != nil {
			if s, err := c.subtreeSpend(context.Background(), snap.ChildID); err == nil {
				spent = s
			}
		}
		name := snap.Name
		if name == "" {
			name = "unnamed"
		}
		frag := fmt.Sprintf(
			"agent %s (%s) is still working (spent $%.2f so far). No action needed unless you want to check in.",
			snap.ChildID, name, spent)
		c.evbuf.Push(parent, heartbeatEventSource, snap.ChildID, frag)
	}
}
