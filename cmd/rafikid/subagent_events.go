package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
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
	// The agent's own residue is checked whether or not it has a parent: a
	// top-level agent's escalation goes to the human, not into a void.
	c.checkTaskResidue(childID)

	parent, ok := c.st.ParentOf(childID)
	if !ok || parent == "" {
		return
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

// checkTaskResidue implements prompting.md's enforcement ladder:
// detect -> nudge once -> escalate.
//
// The rule ("resolve or drop everything before considering yourself done") is
// exactly checkable, so it is checked rather than written into a system prompt
// paid on every request by every agent forever. The daemon queries; no model
// cooperation is required and there is no wording to get right.
func (c *Controller) checkTaskResidue(childID string) {
	if c.tasks == nil || c.evbuf == nil {
		return
	}
	snap, ok := c.st.Get(childID)
	if !ok || snap.SessionID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.tasks.List(ctx, tasks.ListFilter{ConversationID: snap.SessionID})
	if err != nil {
		// Best-effort: a database hiccup must not turn a settle into a stall.
		slog.Warn("residue check failed", "childId", childID, "error", err)
		return
	}

	var unresolved []string
	for _, r := range rows {
		if !r.Status.Terminal() {
			unresolved = append(unresolved, fmt.Sprintf("%s %s", r.Handle, r.Status))
		}
	}
	if len(unresolved) == 0 {
		return
	}

	c.nudgedMu.Lock()
	already := c.nudgedOnce[childID]
	if !already {
		if c.nudgedOnce == nil {
			c.nudgedOnce = make(map[string]bool)
		}
		c.nudgedOnce[childID] = true
	}
	c.nudgedMu.Unlock()

	if !already {
		c.evbuf.Push(childID, subagentEventSource, "residue", fmt.Sprintf(
			"%d task(s) unresolved: %s. Resolve each (task_update) or drop it with a reason (task_drop) before considering yourself done.",
			len(unresolved), strings.Join(unresolved, ", ")))
		return
	}

	// Second settle with residue: do not nudge again. A model that ignored
	// the first is not more likely to honour the fifth, and each one costs a
	// full turn. Escalate to whoever can evaluate the claim of doneness.
	parent, ok := c.st.ParentOf(childID)
	if !ok || parent == "" {
		// A top-level agent escalates to the human, via the log and
		// `rafiki tasks`. There is nobody above it to inject into.
		slog.Warn("agent settled again with unresolved tasks",
			"childId", childID, "unresolved", len(unresolved))
		return
	}
	c.evbuf.Push(parent, subagentEventSource, childID+"::residue", fmt.Sprintf(
		"agent %s settled again with %d unresolved task(s): %s. It was already asked once.",
		childID, len(unresolved), strings.Join(unresolved, ", ")))
}
