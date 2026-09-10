// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

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
// of the same thread can both miss Get and both Insert, but the store's Insert
// is an idempotent overwrite and nothing mutates a synthetic session between
// the miss and the write, so both racers land equivalent records. Do not add
// a mutation path (SetLabels, SetStatus) without revisiting that.
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
