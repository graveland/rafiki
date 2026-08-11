package main

import (
	"fmt"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// subagentEventSource is the event-buffer source name for subagent lifecycle
// news. One source per concern keeps a coordinator's injected frame readable:
// the buffer coalesces per (child, source), so budget warnings and executor
// losses land in their own batches rather than being interleaved with these.
const subagentEventSource = "subagents"

// isWorkingStatus reports whether a status means a turn was actually in
// flight.
//
// This is the guard that makes "settled" mean settled. handleStatusChange
// fires on spawning->idle too, and without this every spawn immediately wakes
// the parent to tell it about the child it just created — costing a turn to
// learn nothing and, worse, arriving before the child has done anything a
// coordinator could read.
func isWorkingStatus(s protocol.Status) bool {
	switch s {
	case protocol.StatusStreaming, protocol.StatusToolRunning,
		protocol.StatusCompacting, protocol.StatusBlockedUI:
		return true
	}
	return false
}

// notifySubagentSettled pushes one fragment about childID into its PARENT's
// event buffer, keyed on childID.
//
// Keying is what makes this cheap: last-write-wins per key means a worker that
// settles three times contributes one fragment, and Push's per-(child, source)
// debounce means five workers finishing together contribute one injected frame
// rather than five turns.
//
// The fragment deliberately does NOT summarise the work. That is the division
// this phase rests on: the buffer says something happened, the ledger says what
// it was. A digest that tried to be the ledger would be a lossy copy of it,
// and a coordinator would learn to trust the copy.
func (c *Controller) notifySubagentSettled(childID, reason string) {
	if c.evbuf == nil {
		return
	}
	parent, ok := c.st.ParentOf(childID)
	if !ok || parent == "" {
		return // top-level agent: nobody to tell
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	name := snap.Name
	if name == "" {
		name = "unnamed"
	}
	frag := fmt.Sprintf(
		"agent %s (%s) %s. Read what it did with task_list(assignee=%q); read how with agent_view(agent=%q).",
		childID, name, reason, childID, childID)
	c.evbuf.Push(parent, subagentEventSource, childID, frag)
}
