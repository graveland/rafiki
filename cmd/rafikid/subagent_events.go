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
//
// batch_wait is in the set: a parked child is mid-turn — its LLM call sits in
// the provider Batch API, not finished — so the idle transition after
// delivery must still fire the settle notification, and parent heartbeats
// keep reporting elapsed time while it waits.
func isWorkingStatus(s protocol.Status) bool {
	switch s {
	case protocol.StatusStreaming, protocol.StatusToolRunning,
		protocol.StatusCompacting, protocol.StatusBlockedUI,
		protocol.StatusBatchWait:
		return true
	}
	return false
}

// notifySubagentSettled announces that childID settled. It fans out to every
// live MCP session of the child's user (notifyMCPSettled, before the gate —
// an MCP caller has no inbox, so the buffer push cannot reach it; the fan-out
// is keyed to TOP-LEVEL rows, since a child-credential caller's spawns are
// parented and reach it through the gate below), then pushes one fragment
// into the child's PARENT's event buffer, keyed on childID.
//
// excludeMCPUser omits that user's sessions from the fan-out: the settlement
// path passes it when the child was killed by an MCP caller of that same
// user, whose agent_kill result already answered what this fragment would
// say. Empty means no exclusion — the ordinary settle path.
//
// stderrTail is the payload a script child that never called SetResult
// settles with — the last 4 KiB of its stderr (scriptSettleFor), empty for
// every other caller. When a result IS stored, the tail is dropped: the
// result is the work product and the stderr is diagnostics for a script that
// never said what it concluded.
//
// Keying is what makes this cheap: last-write-wins per key means a worker that
// settles three times contributes one fragment, and Push's per-(child, source)
// debounce means five workers finishing together contribute one injected frame
// rather than five turns.
func (c *Controller) notifySubagentSettled(childID, reason, stderrTail, excludeMCPUser string) {
	// The MCP fan-out runs first, independent of lineage AND of the event
	// buffer: the caller that spawned a top-level MCP agent must hear about its
	// settlement even though the parent gate below returns for it every time.
	// With no session registered it is a no-op.
	c.notifyMCPSettled(childID, reason, excludeMCPUser)

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
	// The settle fragment carries the child's final result (Connect SetResult)
	// verbatim when it has one: for a script child the result IS the work
	// product, and the parent reading the injected frame should not need a
	// second verb call to learn what the script concluded. Last write wins —
	// this is whatever was stored at settle time. A script that never said what
	// it concluded settles with its stderr tail instead (scriptSettleFor's
	// contract: diagnostics for a failed script, carried rather than dropped).
	fragment := settleFragment(childID, snap.Name, reason)
	if res := snap.Result; res != "" {
		fragment += "\nfinal result of " + childID + ": " + res
	} else if stderrTail != "" {
		fragment += "\n" + childID + " stderr tail:\n" + stderrTail
	}
	c.evbuf.Push(parent, subagentEventSource, childID, fragment)
}

// settleFragment is the one wording both settlement consumers render — the
// parent's event-buffer fragment and the MCP caller's notification — so a
// coordinator agent and an MCP client read the same text. The fragment
// deliberately does NOT summarise the work: the buffer says something
// happened, the ledger says what it was. A digest that tried to be the ledger
// would be a lossy copy of it, and a reader would learn to trust the copy.
func settleFragment(childID, name, reason string) string {
	if name == "" {
		name = "unnamed"
	}
	return fmt.Sprintf(
		"agent %s (%s) %s. Read what it did with task_list(assignee=%q); read how with agent_view(agent=%q).",
		childID, name, reason, childID, childID)
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
		//
		// Info, not Warn: non-terminal is not the same as neglected. Pending
		// rows are a backlog's normal steady state, and rows assigned to
		// still-running children are work in flight — both settle with
		// "unresolved" residue routinely. This line records that the agent
		// was nudged once and settled anyway; the ledger (`rafiki tasks`) is
		// the authoritative view of what is actually left.
		slog.Info("agent settled again with unresolved tasks",
			"childId", childID, "unresolved", len(unresolved))
		return
	}
	c.evbuf.Push(parent, subagentEventSource, childID+"::residue", fmt.Sprintf(
		"agent %s settled again with %d unresolved task(s): %s. It was already asked once.",
		childID, len(unresolved), strings.Join(unresolved, ", ")))
}
