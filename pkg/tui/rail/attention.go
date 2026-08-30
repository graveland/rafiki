// SPDX-License-Identifier: Apache-2.0

package rail

import (
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// replayFloor bounds how far back a reconnect will replay. It is a safety cap
// on volume, not a correctness mechanism: the per-child ordinals in the cursor
// already bound replay exactly.
const replayFloor = 24 * time.Hour

// Types is the rail stream's type filter: six small messages.
//
// The exclusions are the design. assistant_message and user_message are out
// because the rail needs to know a turn HAPPENED, not what it said -- carrying
// them ships every child's full content, tool results included, to a pane a few
// glyphs wide. turn_start and tool_execution_* are out because agent_status
// already carries streaming / tool_running / idle, which is the entire glyph
// vocabulary. content_block_delta needs no exclusion here: it is ephemeral and
// the rail subscribes at tier=DURABLE.
//
// This is what answers "the rail's filter set is a guess until measured" by
// construction: what remains is bounded by turns and status transitions, not by
// tokens.
func Types() []string {
	return []string{
		"turn_end",
		"agent_status",
		"error",
		"retry",
		"child_spawned",
		"child_exited",
	}
}

// Notable reports whether an event should increment a child's attention badge.
//
// Notable means "a human should look", not "something happened" -- everything
// on the rail stream is something happening.
func Notable(ev *rafikiv1.Event) bool {
	switch p := ev.GetPayload().(type) {
	case *rafikiv1.Event_TurnEnd:
		return true // the agent finished a turn; there is something to read
	case *rafikiv1.Event_Error:
		return true
	case *rafikiv1.Event_ChildExited:
		return true
	case *rafikiv1.Event_AgentStatus:
		switch p.AgentStatus.GetState() {
		case "idle":
			return true // settled and waiting on you
		case "blocked_ui":
			return true // explicitly blocked on a human
		}
		return false
	}
	// Retry is deliberately NOT notable: transient-error retry exists so that a
	// recoverable stream error is not a human's problem. child_spawned is not
	// notable either -- it already announces itself by adding a row.
	return false
}

// SetFocus records which child the user is looking at. Events for the focused
// child never accumulate attention.
func (r *Rail) SetFocus(childID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.focused = childID
}

// Focus returns the focused child id, or "".
func (r *Rail) Focus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.focused
}

// countAttention decides whether one event already folded into n earns a badge.
func (r *Rail) countAttention(n *Node, ev *rafikiv1.Event) {
	if !Notable(ev) {
		return
	}
	if n.ChildID == r.focused {
		return
	}
	// An ordinal-less notable event cannot be deduplicated across a reconnect,
	// and this is a REACHABLE state rather than a defensive hypothetical:
	// Controller.publishEvent stamps the ordinal from a log append that is
	// deliberately best-effort, and on failure it warns and publishes anyway.
	// Not counting it under-reports by one; counting it re-badges on every
	// replay, forever.
	if ev.Ordinal == nil {
		return
	}
	ord := ev.GetOrdinal()
	if n.HasSeen && ord <= n.Seen {
		return
	}
	if ord <= n.CountedThrough {
		return // already counted; this is a replay
	}
	n.CountedThrough = ord
	n.Attention++
}

// MarkRead advances a child's watermark and clears its badge.
//
// upTo is the highest ordinal actually DELIVERED to the focus session, not the
// highest that exists and not the scroll position: you are looking at it, and
// scrolling back to reread is not unreading.
func (r *Rail) MarkRead(childID string, upTo int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[childID]
	if !ok {
		return
	}
	if n.HasSeen && upTo < n.Seen {
		return
	}
	n.Seen = upTo
	n.HasSeen = true
	if upTo > n.CountedThrough {
		n.CountedThrough = upTo
	}
	n.Attention = 0
}

// NextAttention returns the next child needing you, in display order, starting
// after the focused row and wrapping. "" means nothing needs you.
func (r *Rail) NextAttention() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := r.nodesLocked()
	if len(nodes) == 0 {
		return ""
	}
	start := 0
	for i, n := range nodes {
		if n.ChildID == r.focused {
			start = i + 1
			break
		}
	}
	for i := 0; i < len(nodes); i++ {
		n := nodes[(start+i)%len(nodes)]
		if n.Attention > 0 {
			return n.ChildID
		}
	}
	return ""
}

// PrevAttention returns the previous child needing attention, scanning
// backwards from the focused row and wrapping.
//
// The backwards mirror of NextAttention. Both exist so alt+n and alt+p are a
// real pair: with attention on both sides of where you are, "next" and "prev"
// must land on different children or the second binding is decoration.
func (r *Rail) PrevAttention() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := r.nodesLocked()
	if len(nodes) == 0 {
		return ""
	}
	start := 0
	for i, n := range nodes {
		if n.ChildID == r.focused {
			start = i
			break
		}
	}
	// Go's % keeps the sign of the dividend, so a bare (start-i)%len is
	// negative and panics on index. Normalise before indexing.
	for i := 1; i <= len(nodes); i++ {
		idx := ((start-i)%len(nodes) + len(nodes)) % len(nodes)
		if nodes[idx].Attention > 0 {
			return nodes[idx].ChildID
		}
	}
	return ""
}

// Cursor builds the rail stream's resume point.
//
// It uses RailCursor, never Seen. Seen is the READ watermark and is far behind
// on any child you have not opened; resuming from it re-delivers events the
// rail already folded in, and only CountedThrough stops that from doubling
// every badge on every reconnect.
//
// FloorUnixMs is not optional. Without it a child that spawned AND exited
// entirely inside a disconnect is indistinguishable from a brand new one -- the
// EventCursor proto says so, and the cockpit is the first consumer that can
// observe the difference.
func (r *Rail) Cursor() *rafikiv1.EventCursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	ords := make(map[string]int32, len(r.nodes))
	for id, n := range r.nodes {
		ords[id] = n.RailCursor
	}
	return &rafikiv1.EventCursor{
		Ordinals:    ords,
		FloorUnixMs: time.Now().Add(-replayFloor).UnixMilli(),
	}
}
