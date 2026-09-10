// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// labelNativeSubagent marks a child the proxy synthesized from a captured
// Claude Code thread rather than one rafiki spawned. Every tree surface reads
// it to avoid implying these are budgeted cross-process agents.
const labelNativeSubagent = "rafiki/native-subagent"

// threadChildID is the deterministic id of a thread's synthetic child. It must
// be a pure function of (parent, thread) because the proxy calls
// EnsureThreadChild on every turn of the thread and must land on the same row.
func threadChildID(parentChildID, threadID string) string {
	return parentChildID + ":" + threadID
}

// EnsureThreadChild creates, idempotently, the synthetic child record standing
// for one Claude Code Task subagent. Lineage rides the same labels a real child
// uses, so the TUI rail, rafiki list, agent_list and the cost rollup all see it
// through Descendants and st.List: making native subagents children is the
// whole point.
//
// The child has no process (PID 0, stored NULL) and is marked Native, which
// keeps it out of LiveDescendantCount ONLY: a bounded sidebar returning one
// tool_result into its parent's context is not a budgeted cross-process agent
// and must not consume the parent's MaxChildren grant. Every other descendant
// walk still returns it.
//
// The Get-then-Insert below is deliberately not locked: two concurrent turns
// of the same thread can both miss Get and both Insert, and the store's Insert
// is an idempotent overwrite, so both racers land equivalent records.
// noteSubagentToolCall's SetLabels/Rename IS a mutation path in the sense this
// comment warned about: an Insert racing it can transiently revert the name
// and the spawned-by-tool label to the thread-uuid form. That is bounded by
// the supervisor hook, which fires on every frame the subagent emits and
// re-applies both on the next one; a silent loss would need the whole turn to
// end inside that window.
func (c *Controller) EnsureThreadChild(parentChildID, threadID, conversationID string) error {
	if parentChildID == "" || threadID == "" {
		return fmt.Errorf("ensure thread child: parent and thread are both required")
	}
	id := threadChildID(parentChildID, threadID)
	if _, ok := c.st.Get(id); ok {
		return nil
	}
	parent, ok := c.st.Get(parentChildID)
	if !ok {
		return fmt.Errorf("ensure thread child: parent %q not found", parentChildID)
	}
	labels := map[string]string{
		childstore.LabelParent: parentChildID,
		childstore.LabelRoot:   c.st.RootOf(parentChildID),
		labelNativeSubagent:    "1",
	}
	c.st.Insert(&childstore.Session{
		ChildID: id,
		Name:    "task:" + threadID,
		Kind:    protocol.KindClaude,
		Native:  true,
		Status:  protocol.StatusIdle,
		Cwd:     parent.Cwd,
		Model:   parent.Model,
		Labels:  labels,
	})
	return nil
}

// labelSpawnedByTool names the Task tool call that spawned a native subagent.
// The value is a tool_use id (toolu_...), which a human can find in the
// parent's own transcript. Written from the child supervisor's stream-json,
// which is the only witness that knows it.
const labelSpawnedByTool = "rafiki/spawned-by-tool"

// noteSubagentToolCall records which Task call spawned a thread's subagent and
// renames the synthetic child after it. Idempotent: the supervisor hook fires
// on every frame the subagent emits.
func (c *Controller) noteSubagentToolCall(parentChildID, threadID, toolUseID string) {
	if parentChildID == "" || threadID == "" || toolUseID == "" {
		return
	}
	id := threadChildID(parentChildID, threadID)
	snap, ok := c.st.Get(id)
	if !ok {
		return
	}
	if snap.Labels[labelSpawnedByTool] == toolUseID {
		return
	}
	if _, err := c.st.SetLabels(id, map[string]string{labelSpawnedByTool: toolUseID}, nil); err != nil {
		slog.Warn("noteSubagentToolCall: set labels failed", "child", id, "error", err)
		return
	}
	if err := c.st.Rename(id, "task:"+toolUseID); err != nil {
		slog.Warn("noteSubagentToolCall: rename failed", "child", id, "error", err)
	}
}

// HandleSubagentObservation resolves a supervisor observation to a thread and
// records its spawning tool call. The message id is the join key: task 2.1
// stored it as conversation_turn.response_message_id, and
// ThreadOfPredecessorInSession maps a message id to its thread.
//
// A miss is normal and silent: the frame may arrive before the proxy has
// recorded that turn, and the hook fires again on the subagent's next frame.
func (c *Controller) HandleSubagentObservation(parentChildID string, obs child.SubagentObservation) {
	if c.captureStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	threadID, err := c.captureStore.ThreadOfPredecessorInSession(ctx, parentChildID, obs.MessageID)
	if err != nil {
		slog.Warn("subagent observation: thread lookup failed", "child", parentChildID, "error", err)
		return
	}
	if threadID == "" {
		return
	}
	c.noteSubagentToolCall(parentChildID, threadID, obs.ParentToolUseID)
}
