// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// displayValue is one child's last PUBLISHED operator display state, the
// baseline refreshDisplayState compares against so an event only goes out when
// the (state, waiting_on, msg) tuple actually changed. See display_state.go for
// computeDisplayState/displayMessage.
type displayValue struct {
	state protocol.DisplayState
	on    protocol.WaitingOn
	msg   string
}

// displayStateScheduleTimeout bounds the one database round trip displayFacts
// makes (the open-schedule count). A display fact must not hold up the refresh
// worker on a slow store.
const displayStateScheduleTimeout = 2 * time.Second

// displayFacts assembles the fact set computeDisplayState folds into a child's
// display state. The bool is false when childID is unknown — a caller then has
// no state to compute.
//
// It is the only I/O in the display-state pipeline, and it runs on the refresh
// worker, never on the status path: handleStatusChange enqueues, it does not
// compute.
func (c *Controller) displayFacts(childID string) (displayFacts, bool) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return displayFacts{}, false
	}
	f := displayFacts{
		Status:          snap.Status,
		PendingQuestion: snap.PendingQuestion,
		LastTurnError:   snap.LastTurnError,
	}
	// TopLevel is "no parent": ParentOf reports false for a root, and an empty
	// parent label is the same thing. A budget halt is only the OPERATOR's to
	// answer for a root, so the (ancestor-walking) budgetHalted call is skipped
	// entirely for a child.
	parent, hasParent := c.st.ParentOf(childID)
	f.TopLevel = !hasParent || parent == ""
	if f.TopLevel {
		_, f.BudgetHalted = c.budgetHalted(childID)
	}
	if c.schedules != nil {
		ctx, cancel := context.WithTimeout(context.Background(), displayStateScheduleTimeout)
		jobs, err := c.schedules.ListOpen(ctx, childID)
		cancel()
		if err != nil {
			// Never silent: a store that cannot answer reads as "no open
			// schedules" only after the failure is surfaced.
			slog.Warn("display state: list open schedules", "childId", childID, "error", err)
		} else {
			f.OpenSchedules = len(jobs)
		}
	}
	f.RunningJobs = c.runningJobsFor(childID)
	// Only a descendant's own machine status holds an ancestor: a settled
	// worker sits at idle until it is closed, and its own schedule or job is
	// its own wait, not its coordinator's. Counting "not exited" would hold the
	// coordinator in waiting forever.
	for _, d := range c.st.Descendants(childID) {
		if d.Status != protocol.StatusIdle && d.Status != protocol.StatusExited {
			f.LiveDescendants++
		}
	}
	f.PendingEvents = c.evbuf != nil && c.evbuf.Pending(childID)
	return f, true
}

// runningJobsFor counts the jobs the daemon believes are running for childID
// across every executor's map. jobRunningMu is held for the scan only — the
// caller has no other lock to order against.
func (c *Controller) runningJobsFor(childID string) int {
	c.jobRunningMu.Lock()
	defer c.jobRunningMu.Unlock()
	n := 0
	for _, byHandle := range c.jobRunning {
		for _, ref := range byHandle {
			if ref.owner == childID {
				n++
			}
		}
	}
	return n
}

// displayStateFor computes one child's display tuple. An unknown child reads
// as exited: the process is gone and nothing can answer for it.
func (c *Controller) displayStateFor(snapID string) (protocol.DisplayState, protocol.WaitingOn, string) {
	f, ok := c.displayFacts(snapID)
	if !ok {
		return protocol.DisplayExited, "", ""
	}
	ds, on := computeDisplayState(f)
	return ds, on, displayMessage(f, ds, on)
}

// refreshDisplayState is a NON-BLOCKING enqueue: it records childID in a
// coalescing set and signals the one display-state worker, then returns. It
// does no I/O on the caller's goroutine, so it is safe from handleStatusChange's
// status path and from the event buffer's change callback.
//
// A zero-value Controller (a hand-built test fixture) has no worker and no
// displayWake; refresh is then a no-op.
func (c *Controller) refreshDisplayState(childID string) {
	if c.displayWake == nil {
		return
	}
	c.displayMu.Lock()
	_, dup := c.displayPending[childID]
	c.displayPending[childID] = struct{}{}
	c.displayMu.Unlock()
	if dup {
		// Already queued: the pending set is either the worker's next batch or
		// the one it is processing now, and a wake is already owed either way.
		return
	}
	select {
	case c.displayWake <- struct{}{}:
	default:
		// A wake is already buffered; the worker drains the whole pending set
		// on that one wake, so a dropped signal loses nothing.
	}
}

// drainDisplayRefresh blocks until every enqueued refresh has been published
// and the worker is idle. Used by tests and by shutdown; it does not spin.
func (c *Controller) drainDisplayRefresh() {
	if c.displayDrain == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case c.displayDrain <- ack:
	case <-c.displayDone:
		return
	}
	select {
	case <-ack:
	case <-c.displayDone:
	}
}

// displayStateWorker is the ONE goroutine that computes and publishes operator
// display states. Serialising every publish through it is what makes a child's
// operator_state events ordered.
func (c *Controller) displayStateWorker(ctx context.Context) {
	defer close(c.displayDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.displayWake:
			c.processDisplayPending()
		case ack := <-c.displayDrain:
			c.processDisplayPending()
			close(ack)
		}
	}
}

// processDisplayPending publishes the display state of every child queued so
// far, looping until the pending set is empty: a publish can enqueue nothing
// itself (the ancestor walk is inline), but a status change racing the worker
// can leave work for the next pass.
func (c *Controller) processDisplayPending() {
	for {
		c.displayMu.Lock()
		batch := c.displayPending
		c.displayPending = make(map[string]struct{})
		c.displayMu.Unlock()
		if len(batch) == 0 {
			return
		}
		for id := range batch {
			c.publishDisplayState(id)
		}
	}
}

// publishDisplayState publishes childID's display state and then refreshes
// every ancestor's, so a child's settle is visible in its coordinator's
// waiting_on. The seen set bounds the upward walk against a corrupt or
// hand-edited parent chain that forms a cycle.
func (c *Controller) publishDisplayState(childID string) {
	seen := make(map[string]struct{})
	for id := childID; id != ""; {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		c.publishOneDisplayState(id)
		parent, ok := c.st.ParentOf(id)
		if !ok {
			return
		}
		id = parent
	}
}

// publishOneDisplayState emits an operator_state event for childID when its
// computed tuple differs from the last one published for it. The absent
// baseline is idle, so the first refresh of an already-idle child publishes
// nothing.
func (c *Controller) publishOneDisplayState(childID string) {
	ds, on, msg := c.displayStateFor(childID)
	v := displayValue{state: ds, on: on, msg: msg}

	c.displayMu.Lock()
	prev := displayValue{state: protocol.DisplayIdle}
	if last, ok := c.displayLast[childID]; ok {
		prev = last
	}
	if prev == v {
		c.displayMu.Unlock()
		return
	}
	c.displayLast[childID] = v
	c.displayMu.Unlock()

	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_OperatorState{OperatorState: &rafikiv1.OperatorState{
			DisplayState: string(ds),
			WaitingOn:    string(on),
			Msg:          msg,
		}},
	})
}
