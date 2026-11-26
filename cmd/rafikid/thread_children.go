// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// labelNativeSubagent marks a child the proxy synthesized from a captured
// Claude Code thread rather than one rafiki spawned. Every tree surface reads
// it to avoid implying these are budgeted cross-process agents, and recovery
// reads it to rebuild the child without ever trying to run it.
const labelNativeSubagent = "rafiki/native-subagent"

// threadRefSep joins a parent child id to a thread id, in both the synthetic
// child's id and the branch conversation's external_ref. One constant because
// the cost rollup selects branch conversations by the "<childID>:" prefix
// (subtreeSelector, costsFor) and a separator that drifts from this one
// silently stops matching.
const threadRefSep = ":"

// threadChildID is the deterministic id of a thread's synthetic child. It must
// be a pure function of (parent, thread) because the proxy calls
// EnsureThreadChild on every turn of the thread and must land on the same row.
func threadChildID(parentChildID, threadID string) string {
	return parentChildID + threadRefSep + threadID
}

// EnsureThreadChild creates, idempotently, the child record standing for one
// Claude Code Task subagent. Lineage rides the same labels a real child uses,
// so the TUI rail, rafiki list, agent_list and the cost rollup all see it
// through Descendants and st.List: making native subagents children is the
// whole point.
//
// The child has no process (PID 0, stored NULL) and is marked Native, which
// keeps it out of LiveDescendantCount ONLY: a bounded sidebar returning one
// tool_result into its parent's context is not a budgeted cross-process agent
// and must not consume the parent's MaxChildren grant. Every other descendant
// walk still returns it.
//
// The record is DURABLE. The row is written before the child is announced, and
// a write that fails takes the session back out, publishes nothing and returns
// the error — no row, no child. The row is what carries the child across a
// daemon restart (loadChildren reloads it and never resumes it: it has no
// process to resume), and what keeps its lineage findable after its parent is
// closed.
//
// conversationID is the branch conversation the thread's founding turn landed
// on, and it is the native child's conversation_id: a Task subagent has no
// Claude Code session of its own, so its SessionID slot carries the
// conversation instead (see writeRecordLastStatus).
//
// SCOPE — every rule in this file applies to NATIVE rows only. A spawned child
// keeps the semantics it has: killing its parent leaves it running, Kill stops
// it without closing it, and a killed child can be resumed. A native child is
// a thread INSIDE its parent's claude process, so it cannot outlive that
// process: it exits with it, closes with it, and is never killed, messaged or
// resumed on its own (ErrNativeSubagent names the parent to address instead).
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
	if snap, ok := c.st.Get(id); ok {
		return c.reviveThreadChild(snap, conversationID)
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
		// The thread's conversation, not a Claude Code session: see the doc
		// comment above and writeRecordLastStatus's native arm.
		SessionID: conversationID,
		Cwd:       parent.Cwd,
		Model:     parent.Model,
		// The subagent inherits the parent's once-resolved routing spec the
		// way it inherits Model: its own requests resolve against THIS row
		// (RoutingFor), and a confidentiality flag the parent spawned under
		// must not silently stop applying because the request came from a
		// subagent.
		Routing: parent.Routing,
		// The subagent folds into the parent's subtree, so it inherits the
		// parent's derived-index suppression rather than re-deriving it.
		SkipDerivedIndex: parent.SkipDerivedIndex,
		// Copied, never left nil: nil is a legacy row, which resolves to every
		// executor of the owner, so a subagent of a restricted parent would escape.
		// A legacy parent's nil set is read through its selector, so the selector
		// travels with it and the subagent resolves exactly as the parent does.
		ExecutorSet:      slices.Clone(parent.ExecutorSet),
		ExecutorSelector: parent.ExecutorSelector,
		Labels:           labels,
	})

	// Before the announcement: a child with no row must not appear anywhere,
	// and the failure path below takes the session back out.
	if err := c.persistThreadChild(id, threadID); err != nil {
		c.st.Delete(id)
		// Nothing has been published for this id, but a spawn-shaped span or
		// observer entry may already have been queued against it.
		c.trace.ChildGone(id)
		slog.Warn("ensure thread child: row not persisted",
			"parent", parentChildID, "thread", threadID, "error", err)
		return err
	}

	// The native twin of a real Spawn's child_spawned (controller.go). Without
	// it the row is silent from birth: the TUI rail only ever learns about a
	// new child from the one-time Init seed or a reactive re-seed triggered by
	// seeing an event for an unknown child id, and nothing else is ever
	// published under this id until it exits. A subagent spawned mid-session
	// was therefore invisible in the rail even though `rafiki list` (which
	// queries fresh) already saw it.
	//
	// The name comes off the snapshot, not the thread id: a name conflict
	// inside persistThreadChild may have suffixed it.
	name := "task:" + threadID
	if snap, ok := c.st.Get(id); ok {
		name = snap.Name
	}
	c.publishEvent(id, &rafikiv1.Event{
		ChildId: id,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{
			ChildId:  id,
			ParentId: parentChildID,
			Name:     name,
			Kind:     protocol.KindClaude,
		}},
	})
	return nil
}

// persistThreadChild writes a thread child's row, working around the one
// failure a native child can hit at birth: a live sibling already holding its
// name.
//
// Every subagent of one parent is named "task:<threadID>", and the name rule is
// the child_sibling_name_live index, not a pre-read. The row's child_id is
// deterministic, so a suffixed retry is an upsert of the SAME row: the suffix
// moves the name, never the identity or the lineage. "task:<thread>", then
// "-2" through "-9", is the documented bound; past it the last name-taken error
// is returned and the caller forgets the child.
func (c *Controller) persistThreadChild(childID, threadID string) error {
	err := c.writeRecord(childID)
	if err == nil || !errors.Is(err, childstore.ErrNameTaken) {
		return err
	}
	for n := 2; n <= 9; n++ {
		if rerr := c.st.Rename(childID, fmt.Sprintf("task:%s-%d", threadID, n)); rerr != nil {
			return rerr
		}
		err = c.writeRecord(childID)
		if err == nil || !errors.Is(err, childstore.ErrNameTaken) {
			return err
		}
	}
	return err
}

// reviveThreadChild re-idles a thread child that already exists but reads as
// exited: a resumed parent continuing a thread it had run before. Claude Code
// keeps a subagent's thread id, so the same (parent, thread) lands on the same
// child, and that child is alive again because its parent's process is.
//
// The row is persisted before returning because the row is what every other
// reader sees: a child left "exited" in the database is a killed child to
// recovery, and rafiki list would show a dead subagent under a running parent.
func (c *Controller) reviveThreadChild(snap childstore.Snapshot, conversationID string) error {
	if snap.Status != protocol.StatusExited {
		return nil
	}
	c.st.SetStatus(snap.ChildID, protocol.StatusIdle)
	if err := c.st.Update(snap.ChildID, func(s *childstore.Session) {
		s.ExitedAt = time.Time{}
		s.ExitCode = nil
		s.ExitSignal = ""
		if conversationID != "" {
			s.SessionID = conversationID
		}
	}); err != nil {
		return err
	}
	if err := c.writeRecord(snap.ChildID); err != nil {
		slog.Warn("ensure thread child: reviving row not persisted",
			"child", snap.ChildID, "error", err)
		return err
	}
	// No state machine drives a native child, so nothing else will tell the rail
	// it is alive again: agent_status is the one event that moves a row's glyph,
	// and a newer non-exited status is also what clears the rail's exited flag.
	c.publishEvent(snap.ChildID, &rafikiv1.Event{
		ChildId: snap.ChildID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: string(protocol.StatusIdle)}},
	})
	return nil
}

// nativeSubagentRefusal is what Kill, Send and Resume answer for a native
// thread child. The child has no process of its own, so there is nothing to
// signal and no session to reopen: the operation the caller wants is an
// operation on the parent, and the message names it.
func nativeSubagentRefusal(snap childstore.Snapshot) error {
	target := "its parent child"
	if parent, ok := childstore.ParentLabel(snap.Labels); ok {
		target = parent
	}
	return &connectapi.ControllerError{
		Code:    protocol.ErrNativeSubagent,
		Message: fmt.Sprintf("child %s is a Claude Code subagent inside %s; kill or resume %s instead", snap.ChildID, target, target),
	}
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
	// Best effort, like every ordinary writer: the row is durability, not
	// delivery, and the hook fires again on the subagent's next frame, so a
	// lost write is retried rather than fatal. A rename onto a name a live
	// sibling already holds lands here as ErrNameTaken; the in-memory name is
	// then newer than the row's until the next successful write.
	if err := c.writeRecord(id); err != nil {
		slog.Warn("noteSubagentToolCall: write record failed", "child", id, "error", err)
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

// nativeChildrenOf returns the ids of parentChildID's synthetic thread
// children. Reads the parent label directly rather than st.Descendants, which
// walks the whole subtree: a native child can only ever be one hop from the
// session it was captured on, and it never has children of its own.
func (c *Controller) nativeChildrenOf(parentChildID string) []string {
	if parentChildID == "" {
		return nil
	}
	var out []string
	for _, snap := range c.st.List() {
		if snap.Native && snap.Labels[childstore.LabelParent] == parentChildID {
			out = append(out, snap.ChildID)
		}
	}
	return out
}

// exitNativeChild ends a synthetic thread child at `at`, the instant its parent
// exited. It reports whether the child existed and was not already exited.
//
// A native child has no process, so ending it IS a pair of writes: the store's
// status and the child's row. That also means none of handleChildExit's work
// applies — no ring to snapshot, no log dump, no MCP secret, no inbox to reset,
// no executor binding, no lease. What does apply is the exit event, because the
// rail learns about the transition from it alone.
//
// Exit code 0 with no signal: the thread ended, and inventing a signal would
// claim a death this child never had.
func (c *Controller) exitNativeChild(childID string, at time.Time) bool {
	snap, ok := c.st.Get(childID)
	if !ok || !snap.Native || snap.Status == protocol.StatusExited {
		return false
	}
	if !c.st.MarkExited(childID, at, 0, "", nil, nil) {
		return false
	}
	// The row records the exit the same way a real child's does, because
	// recovery reads status: a native row left idle is a subagent this daemon
	// claims is still running under a parent that is gone. Best effort — the
	// parent's own exit already succeeded, and a failed row write must not stop
	// the remaining threads from ending.
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("native subagent exit not persisted", "child", childID, "parent", snap.Labels[childstore.LabelParent], "error", err)
	}

	var code int32
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{
			ChildId:  childID,
			ExitCode: &code,
		}},
	})
	return true
}

// exitNativeChildrenOf ends every synthetic thread child of parentChildID at
// the parent's own exit time. Called when the parent exits: a Task subagent
// runs inside its parent's process, so it cannot outlive it, and leaving it
// idle forever is what made these children unkillable and uncloseable in the
// first place.
func (c *Controller) exitNativeChildrenOf(parentChildID string, at time.Time) {
	for _, id := range c.nativeChildrenOf(parentChildID) {
		if c.exitNativeChild(id, at) {
			slog.Debug("native subagent ended with its parent", "child", id, "parent", parentChildID)
		}
	}
}

// closeNativeChild finalizes one synthetic thread child: persist, announce,
// forget. It is the parent's own close step applied to a child with no process
// — the row is written first so its conversation id, labels and status are
// durable, then tombstoned, then dropped from the store.
//
// A row this daemon does not own is neither written nor tombstoned (it belongs
// to another daemon), and the store entry is still forgotten here, exactly as
// the parent's own close forgets a foreign row locally.
//
// Fail-closed on a persist error, like the parent's: a child whose lineage
// could not be recorded must not silently drop out of its ancestors' spend, so
// the child stays in the store and the caller gets the error.
func (c *Controller) closeNativeChild(childID string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return nil
	}
	// Read BEFORE writeRecord, which stamps this daemon's label onto the
	// snapshot it writes: ownership is what the row said when we found it.
	owns := c.ownsChildRow(snap)
	if owns {
		if err := c.writeRecord(childID); err != nil {
			// The child id and the error TYPE only, never the message — the
			// same rule Close's own persist warning follows.
			slog.Warn("close: native child not persisted", "childId", childID, "errorType", fmt.Sprintf("%T", err))
			return fmt.Errorf("close %s: %w", childID, err)
		}
	}
	// Before the delete, like every close site: the event carries the lineage a
	// subscriber still drawing the row needs.
	c.publishChildClosed(childID)
	c.st.Delete(childID)
	// A thread child is gone with its session, so its observer state (a turn
	// its sub-agent frames opened) goes too.
	c.trace.ChildGone(childID)
	if owns && c.children != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.children.Delete(ctx, childID); err != nil {
			slog.Warn("delete native child row", "childId", childID, "error", err)
		}
		// A closed child's conversations are closed with it, exactly as Close does
		// for a real child: this thread's own branch conversation, and any deeper
		// thread that names it. Best effort — the row is already tombstoned.
		if err := c.stampConversationsClosed(ctx, childID); err != nil {
			slog.Warn("stamp conversation closed (native child)", "childId", childID, "error", err)
		}
	}
	return nil
}

// closeNativeChildrenOf closes every synthetic thread child of parentChildID
// and returns the ids it took. Called when the parent is CLOSED, not merely
// exited: these rows carry a parent label, so a close that left them behind
// would strand them at the top of the rail pointing at a session that no longer
// exists. Their transcripts survive, exactly as the parent's does, because
// nothing references conversations.child.
//
// A failure returns immediately with the ids closed so far: the caller fails
// its own close, so nothing is closed that the parent's row would then
// contradict.
func (c *Controller) closeNativeChildrenOf(parentChildID string) ([]string, error) {
	taken := c.nativeChildrenOf(parentChildID)
	for i, id := range taken {
		if err := c.closeNativeChild(id); err != nil {
			return taken[:i], err
		}
	}
	return taken, nil
}
