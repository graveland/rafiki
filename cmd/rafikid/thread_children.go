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
		if err := c.reviveThreadChild(snap, conversationID); err != nil {
			// A close that landed between loading this snapshot and writing it back
			// is the same refusal the birth path answers, not an internal error.
			if errors.Is(err, childstore.ErrClosed) {
				return threadChildClosedRefusal(id)
			}
			return err
		}
		return nil
	}
	// A CLOSED ROW IS FINAL, INCLUDING FOR THIS DETERMINISTIC ID. A child id is
	// <parent>:<thread>, so the same thread asks for the same row on every turn —
	// and the row write is an upsert that clears closed_at, which would silently
	// un-close a child an operator deliberately closed (`rafiki close <id>`) and
	// put it back in `rafiki list`. Never that: a tombstone is the daemon's
	// promise that the child can never be resumed, reattached or continued, and a
	// native subagent cannot be a second row (the id IS the join key for its
	// conversation, its lineage and its spend). So the row is refused and NO
	// session is inserted — the thread's traffic is still captured into its
	// branch conversation, it simply has no rail row or lineage entry any more,
	// which is exactly the state the close left it in.
	//
	// This state is reachable, not theoretical: before the close ORDER was fixed,
	// a parent persist failure could close its native children under a live
	// parent, an operator can close a thread's row while the parent process keeps
	// running, and a Close concurrent with this one can land HERE — the read
	// below answers "not closed" and the tombstone arrives before the write. That
	// last window is why the read is not the guard: the WRITE refuses a
	// tombstoned row in the database itself (ChildRecord.RefuseIfClosed over
	// closed_at IS NULL), and a write that is refused is answered with the same
	// failed_precondition this read produces.
	if closed, err := c.threadChildRowClosed(id); err != nil {
		return fmt.Errorf("ensure thread child: %w", err)
	} else if closed {
		return threadChildClosedRefusal(id)
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
		// A tombstone that landed after the IsClosed read above is the SAME
		// refusal, not a write failure: the row is closed and this birth is over.
		if errors.Is(err, childstore.ErrClosed) {
			return threadChildClosedRefusal(id)
		}
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
//
// A childstore.ErrClosed is returned unchanged and NOT retried under a new name:
// the suffix would move the name on a row the database just refused to touch,
// and no name makes a closed row writable again.
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

// threadChildClosedRefusal is the ONE answer for a closed thread row, from either
// guard: the pre-write IsClosed read or the database refusing the write itself.
// Same code and same text, because to the caller they are the same fact.
func threadChildClosedRefusal(childID string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrFailedPrecondition,
		Message: fmt.Sprintf("thread child %s is closed; a closed child is never revived and its id is never reused", childID),
	}
}

// reviveThreadChild re-idles a thread child that already exists but reads as
// exited: a resumed parent continuing a thread it had run before. Claude Code
// keeps a subagent's thread id, so the same (parent, thread) lands on the same
// child, and that child is alive again because its parent's process is.
//
// The row is persisted before returning because the row is what every other
// reader sees: a child left "exited" in the database is a killed child to
// recovery, and rafiki list would show a dead subagent under a running parent.
// A failed write therefore ROLLS THE MEMORY BACK to the exited shape it found,
// rather than leaving the store saying idle over a row that still says exited.
func (c *Controller) reviveThreadChild(snap childstore.Snapshot, conversationID string) error {
	if snap.Status != protocol.StatusExited {
		return nil
	}
	wasSessionID := snap.SessionID
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
		// Roll back to exactly the snapshot the revive started from: a store that
		// says idle over a row that still says exited is the disagreement this
		// whole task exists to remove.
		c.st.SetStatus(snap.ChildID, protocol.StatusExited)
		if uerr := c.st.Update(snap.ChildID, func(s *childstore.Session) {
			s.ExitedAt = snap.ExitedAt
			s.ExitCode = snap.ExitCode
			s.ExitSignal = snap.ExitSignal
			s.SessionID = wasSessionID
		}); uerr != nil {
			slog.Warn("ensure thread child: revive rollback failed", "child", snap.ChildID, "error", uerr)
		}
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

// threadChildRowClosed asks the durable store whether this thread's row already
// exists WITH a close tombstone — the one question List cannot answer, because
// List returns live rows only.
//
// A read failure is returned, never swallowed into "not closed": that is the
// answer that resurrects the tombstone this guard exists to protect. No store at
// all is the documented memory-only mode (children do not survive a restart
// anyway), so it answers false.
func (c *Controller) threadChildRowClosed(childID string) (bool, error) {
	if c.children == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closed, err := c.children.IsClosed(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("read thread child %s: %w", childID, err)
	}
	return closed, nil
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
	//
	// NOT during the daemon's own shutdown, exactly like the parent's write
	// (handleChildExit's `if !c.stopping.Load()`): there the rows deliberately
	// keep their LIVE statuses so the next daemon resumes the parent and its
	// threads with it. Writing the native row exited while the parent row still
	// says idle would split the pair — the parent would resume and load a thread
	// it had just declared dead — and would leave no exit to reconcile from.
	if !c.stopping.Load() {
		if err := c.writeRecord(childID); err != nil {
			slog.Warn("native subagent exit not persisted", "child", childID, "parent", snap.Labels[childstore.LabelParent], "error", err)
		}
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

// reconcileNativeChildren ends the native thread children whose parent cannot be
// running any more, at boot. It is the other half of exitNativeChildrenOf:
// that runs from handleChildExit, which a daemon that CRASHED never reached, so
// the parent is recovered as exited while its threads' rows still read idle —
// and no verb can end them (Close demands an exited child, and Kill/Send/Resume
// refuse a native one). Without this, a crash strands them until an operator
// intervenes.
//
// Decided once, after the recovery walk has loaded every row, from the parent's
// SETTLED state and not from the status it happens to hold at this instant:
// recoverOne writes every non-native recovered row as exited BEFORE handing its
// resume to a goroutine, so "exited in the store" is a placeholder for a child
// this boot is about to resume — a local claude re-attach, a fundi engine
// rebuild, or a resume pended for the executor that will fire it. Ending the
// threads of one of those is the bug this parameter exists to prevent: it ends a
// running daraja-hosted subagent (whose process survived the restart) and
// contradicts exitNativeChild's own shutdown skip, which leaves native rows live
// precisely so the next daemon resumes the parent and its threads together.
//
// So a parent is "gone" when its session is absent (its row is closed — List
// skips tombstones — or never existed), or recovery left it exited: it is not in
// `resuming` AND it reads exited AND it is a row this daemon owns. A FOREIGN
// parent is deliberately left alone: recovery loads every other daemon's row as
// exited here, so its status says nothing about its threads, and writing another
// daemon's child rows is not ours to do.
//
// Best effort and never boot-fatal: a failed row write is logged by
// exitNativeChild and the walk carries on.
func (c *Controller) reconcileNativeChildren(resuming map[string]struct{}) {
	var ended int
	for _, snap := range c.st.List() {
		if !snap.Native || snap.Status == protocol.StatusExited {
			continue
		}
		at, gone := c.nativeParentEnded(snap, resuming)
		if !gone {
			continue
		}
		if c.exitNativeChild(snap.ChildID, at) {
			ended++
		}
	}
	if ended > 0 {
		slog.Info("native subagents ended with a parent that is no longer running", "count", ended)
	}
}

// nativeParentEnded reports whether snap's parent cannot be running, and the
// instant to end the thread at. See reconcileNativeChildren for why a foreign
// parent is never "ended" and why a resuming one is not either.
func (c *Controller) nativeParentEnded(snap childstore.Snapshot, resuming map[string]struct{}) (time.Time, bool) {
	parentID, ok := childstore.ParentLabel(snap.Labels)
	if !ok {
		// A native child with no parent label only ever existed as a thread, so
		// there is no process that could still be running it.
		return time.Time{}, true
	}
	parent, present := c.st.Get(parentID)
	if !present {
		return time.Time{}, true
	}
	// The settled answer, before any status read: a parent this boot resumed (or
	// pended for its executor) is alive, and the exited status recoverOne left in
	// the store is only the placeholder the resume writes over.
	if _, keep := resuming[parentID]; keep {
		return time.Time{}, false
	}
	if !c.ownsChildRow(parent) || parent.Status != protocol.StatusExited {
		return time.Time{}, false
	}
	at := parent.ExitedAt
	if at.IsZero() {
		// A recovered row always carries one; a hand-built one may not.
		at = time.Now()
	}
	return at, true
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
// the child stays in the store and the caller gets the error. The ONE exception
// is childstore.ErrClosed — the row is already tombstoned, so there is nothing
// left to persist and the close has reached the state it wanted.
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
			if errors.Is(err, childstore.ErrClosed) {
				// Another writer tombstoned this row first (a daemon sharing the
				// database, or a direct `rafiki close` of the native id). Its snapshot
				// is already durable, so failing the parent's close here would
				// refuse a close that has nothing left to do.
				slog.Debug("close: native child row was already closed", "childId", childID)
			} else {
				// The child id and the error TYPE only, never the message — the
				// same rule Close's own persist warning follows.
				slog.Warn("close: native child not persisted", "childId", childID, "errorType", fmt.Sprintf("%T", err))
				return fmt.Errorf("close %s: %w", childID, err)
			}
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
