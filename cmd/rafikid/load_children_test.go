package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestShouldAutoResume pins the recovery predicate. It is the rule loadOrphans
// applied and it is easy to invert: a child is resumable when its LAST status
// says it was alive, and "shutting_down" means a deliberate stop, NOT alive.
// There is no "running" status — see pkg/protocol/types.go.
func TestShouldAutoResume(t *testing.T) {
	cases := []struct {
		name string
		rec  childstore.ChildRecord
		want bool
	}{
		{"idle fundi resumes", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "idle"}, true},
		{"streaming fundi resumes", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "streaming"}, true},
		{"tool_running fundi resumes", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "tool_running"}, true},
		{"compacting fundi resumes", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "compacting"}, true},
		{"blocked_ui fundi resumes", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "blocked_ui"}, true},
		{"spawning fundi resumes", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "spawning"}, true},
		{"exited fundi does not", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "exited"}, false},
		{"shutting_down fundi does not", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: "shutting_down"}, false},
		{"row with neither status does not", childstore.ChildRecord{Kind: protocol.KindFundi, LastStatus: ""}, false},
		{"idle claude does not", childstore.ChildRecord{Kind: protocol.KindClaude, LastStatus: "idle"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoResume(tc.rec); got != tc.want {
				t.Errorf("shouldAutoResume = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRecoveryActionWorkspaceMode pins design §3.1. A pinned child must NOT be
// moved to another machine by the restart path — HandleExecutorLost fails a
// pinned child where it stood, and boundExecutor.recover refuses to re-select
// for one. But a pinned child CAN resume on the SAME machine when it restarts
// alongside the daemon — planResumeBound strips only the stale workspace id
// while keeping the executor identity so doRecover can re-provision in place.
// An unknown mode is pinned.
func TestRecoveryActionWorkspaceMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want recoveryPlan
	}{
		{"ephemeral rebinds", "ephemeral", planRebindUnbound},
		{"pinned resumes on same machine", "pinned", planResumeBound},
		{"unknown mode resumes on same machine", "", planResumeBound},
		{"garbage mode resumes on same machine", "sideways", planResumeBound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := childstore.ChildRecord{
				Kind: protocol.KindFundi, LastStatus: "idle", WorkspaceMode: tc.mode,
				Labels: map[string]string{"rafiki/workspace": "w1", "rafiki/executor": "e1"},
			}
			if got := recoveryAction(rec); got != tc.want {
				t.Errorf("recoveryAction = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShouldAutoResumeAfterDaemonCrash is the regression test for the defect
// that broke the design's whole motivating scenario.
//
// last_status is written ONLY by handleChildExit. A daemon killed by SIGKILL,
// OOM or a node eviction never runs it, so the column stays NULL — and NULL is
// therefore the STRONGEST evidence the child was alive, not the weakest. The
// original predicate read NULL as "do not resume", which is exactly backwards
// and meant a crashed pod recovered nothing.
func TestShouldAutoResumeAfterDaemonCrash(t *testing.T) {
	cases := []struct {
		name string
		rec  childstore.ChildRecord
		want bool
	}{
		{
			"crashed while idle resumes",
			childstore.ChildRecord{Kind: protocol.KindFundi, Status: "idle", LastStatus: ""},
			true,
		},
		{
			"crashed while streaming resumes",
			childstore.ChildRecord{Kind: protocol.KindFundi, Status: "streaming", LastStatus: ""},
			true,
		},
		{
			"crashed while running a tool resumes",
			childstore.ChildRecord{Kind: protocol.KindFundi, Status: "tool_running", LastStatus: ""},
			true,
		},
		{
			"cleanly exited does not resume",
			childstore.ChildRecord{Kind: protocol.KindFundi, Status: "exited", LastStatus: "exited"},
			false,
		},
		{
			"recorded exit wins over a stale status",
			childstore.ChildRecord{Kind: protocol.KindFundi, Status: "idle", LastStatus: "exited"},
			false,
		},
		{
			"row with neither status resumes nothing",
			childstore.ChildRecord{Kind: protocol.KindFundi, Status: "", LastStatus: ""},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoResume(tc.rec); got != tc.want {
				t.Errorf("shouldAutoResume = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStripStaleBindings proves the ephemeral path actually clears the labels.
// A resumed child that keeps rafiki/workspace points at a workspace id that
// died with the old executor process — the registry is in memory.
func TestStripStaleBindings(t *testing.T) {
	labels := map[string]string{
		"rafiki/workspace": "w1",
		"rafiki/executor":  "e1",
		"owner":            "brent",
	}
	got := stripStaleBindings(labels)
	if _, ok := got["rafiki/workspace"]; ok {
		t.Error("rafiki/workspace survived")
	}
	if _, ok := got["rafiki/executor"]; ok {
		t.Error("rafiki/executor survived")
	}
	if got["owner"] != "brent" {
		t.Error("an unrelated label was dropped")
	}
	if got["rafiki/executor-state"] != "unbound" {
		t.Errorf("executor-state = %q, want %q", got["rafiki/executor-state"], "unbound")
	}
}

// TestStripStaleWorkspace proves the pinned-recovery path strips only the
// workspace id — the executor identity survives so boundExecutor.doRecover can
// check IsLive and re-provision on the same machine.
func TestStripStaleWorkspace(t *testing.T) {
	labels := map[string]string{
		"rafiki/workspace": "w1",
		"rafiki/executor":  "e1",
		"owner":            "brent",
	}
	got := stripStaleWorkspace(labels)
	if _, ok := got["rafiki/workspace"]; ok {
		t.Error("rafiki/workspace survived")
	}
	if got["rafiki/executor"] != "e1" {
		t.Errorf("rafiki/executor = %q, want %q — must survive so doRecover can re-provision on the same machine", got["rafiki/executor"], "e1")
	}
	if got["owner"] != "brent" {
		t.Error("an unrelated label was dropped")
	}
	if got["rafiki/executor-state"] != "unbound" {
		t.Errorf("executor-state = %q, want %q", got["rafiki/executor-state"], "unbound")
	}
}

// TestRecoveryReplaysUnconfirmedMessages is design §8's test #1 in unit form:
// a message written to a child that died before consuming it must be delivered
// again, not silently retired.
func TestRecoveryReplaysUnconfirmedMessages(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	rec, _ := st.Accept(ctx, inbox.Inbound{
		ChildID: "c_1", Mode: inbox.ModePrompt, Source: "subagents",
		Key: "c_2", Text: "agent c_2 settled",
	})
	if err := st.MarkSent(ctx, []string{rec.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	var delivered []inbox.Batch
	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{
		Store: st,
		Deliver: func(_ context.Context, b inbox.Batch) (bool, error) {
			delivered = append(delivered, b)
			return false, nil
		},
	})

	// The two-step pipeline, in the order recovery actually runs it:
	// ownership established (resetUnconfirmedOnOwnership, normally called
	// from OnConversationResolved) THEN delivery (replayInbox, normally
	// called after resumeWithAutoRecovery returns). See I2 in the final
	// review — running Reset and Deliver together, after the child has
	// already gone idle, is what caused a duplicate delivery; see
	// TestReplayDoesNotDuplicateARowTheIdleDrainAlreadyDelivered for that.
	c.resetUnconfirmedOnOwnership("c_1")
	c.replayInbox(ctx, "c_1")

	if len(delivered) != 1 {
		t.Fatalf("want the settle redelivered after restart, got %d batches", len(delivered))
	}
	if delivered[0].Source != "subagents" || delivered[0].Frags[0] != "agent c_2 settled" {
		t.Errorf("replayed batch = %+v", delivered[0])
	}
}

// TestResetUnconfirmedOnOwnershipIsScopedToOneChild is the multi-daemon trap
// in unit form, now aimed at the function that actually does the resetting
// (resetUnconfirmedOnOwnership moved the Reset out of replayInbox — see I2 in
// the final review). Child rows are shared across every daemon; establishing
// ownership of c_1 must never touch c_2's unconfirmed rows, or a resuming
// daemon would resurrect and double-deliver messages belonging to a sibling
// daemon that is still running c_2 fine.
//
// Mutation check: make resetUnconfirmedOnOwnership reset unscoped (e.g. loop
// over every known child, or call a bulk ResetSent) and this fails — c_2's
// row becomes visible to Pending even though c_2 was never passed in.
func TestResetUnconfirmedOnOwnershipIsScopedToOneChild(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()

	rec1, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "for c_1"})
	if err != nil {
		t.Fatalf("Accept c_1: %v", err)
	}
	rec2, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_2", Mode: inbox.ModePrompt, Text: "for c_2"})
	if err != nil {
		t.Fatalf("Accept c_2: %v", err)
	}
	if err := st.MarkSent(ctx, []string{rec1.ID, rec2.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})

	c.resetUnconfirmedOnOwnership("c_1")

	pendingC1, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending c_1: %v", err)
	}
	if len(pendingC1) != 1 {
		t.Fatalf("c_1's row was not reset to pending: %+v", pendingC1)
	}

	// c_2's row must still be 'sent' (not reset to pending): Pending only
	// returns rows in StatePending, so a leaked scope would show up here as
	// c_2's row becoming visible.
	pendingC2, err := st.Pending(ctx, "c_2")
	if err != nil {
		t.Fatalf("Pending c_2: %v", err)
	}
	if len(pendingC2) != 0 {
		t.Fatalf("c_2's row was reset to pending by a call scoped to c_1: %+v", pendingC2)
	}
}

// TestRecoveryStaysExitedResetsRatherThanDrops pins the final review's C1
// correction: recoverOne's planStayExited branch used to call
// dropInboxForForgotten (a terminal, unrepairable Drop) with no ownership
// check at all — this Ruling reversed that outright rather than merely
// gating it, because the rationale that used to justify a drop here
// ("no turn to inject into and there will not be one") described a branch
// recoveryAction no longer produces: a pinned child whose executor comes
// back CAN be resumed later, manually, and this phase's rule is that exit
// RESETS. A Drop here would have permanently discarded a row a later manual
// `rafiki resume` should have been able to deliver.
func TestRecoveryStaysExitedResetsRatherThanDrops(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	rec0, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "orphaned"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := st.MarkSent(ctx, []string{rec0.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: st})

	events, cancel := c.native.Subscribe("c_1")
	defer cancel()

	rec := childstore.ChildRecord{
		ChildID: "c_1",
		Kind:    protocol.KindFundi,
		Status:  "exited",
		// LastStatus "exited" -> shouldAutoResume false -> planStayExited.
		LastStatus: "exited",
	}
	c.recoverOne(ctx, rec)

	// Reset, not dropped: the row must now be visible to Pending (a later
	// resume's idle drain can only ever redeliver 'pending' rows, never
	// 'sent' ones), and it must NOT be terminal.
	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a child staying exited must have its unconfirmed row reset to pending, not dropped: %d rows pending, want 1", len(rows))
	}
	if n, err := st.Sweep(ctx, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("swept %d rows (err=%v); want 0 — a reset row is not terminal and must survive the retention sweep", n, err)
	}

	// No inbox_dropped event: nothing was dropped.
	select {
	case ev := <-events:
		t.Fatalf("want no event published for a reset (not dropped) queue, got %+v", ev)
	default:
	}
}

// TestReplayDoesNotDuplicateARowTheIdleDrainAlreadyDelivered is I2 from the
// final review, reproduced deterministically without a live engine.
//
// The real race: resumeWithAutoRecovery returns only after the child reaches
// idle, and that idle transition fires its own drain (drainInbox) BEFORE
// recoverOne's goroutine gets around to calling replayInbox. If replayInbox
// still did its own Reset (the old design), it could not tell "a row this
// engine's own idle drain just delivered and marked 'sent' moments ago" apart
// from "a row genuinely stale from before the crash" — both read 'sent' — so
// it flipped both back to 'pending' and DeliverAll sent the fresh one again:
// a duplicate prompt into a duplicate turn, on this phase's headline path.
//
// This test drives the same three steps in the same order, standing in for
// the pieces recoverOne/OnConversationResolved orchestrate live:
//  1. resetUnconfirmedOnOwnership (now called from OnConversationResolved,
//     BEFORE the engine can process a frame) resets the pre-crash 'sent' row.
//  2. The ordinary idle-transition drain (Queue.Deliver with source "",
//     exactly what drainInbox does after I4) delivers it and marks it 'sent'
//     again -- this is the row genuinely, correctly in flight.
//  3. replayInbox (now Deliver-only) runs, as it does after
//     resumeWithAutoRecovery returns.
//
// The row must be delivered EXACTLY ONCE: step 3 must find nothing pending,
// because step 1 already ran before anything could mark a fresh 'sent' row,
// and step 2 is what did that marking.
func TestReplayDoesNotDuplicateARowTheIdleDrainAlreadyDelivered(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	rec, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "pre-crash message"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := st.MarkSent(ctx, []string{rec.ID}); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	var deliveries int
	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{
		Store: st,
		Deliver: func(_ context.Context, b inbox.Batch) (bool, error) {
			deliveries++
			return true, nil // awaitAck: mark 'sent', matching a fundi child
		},
	})

	// Step 1: ownership established.
	c.resetUnconfirmedOnOwnership("c_1")

	// Step 2: the ordinary idle-transition drain, direct messages only.
	if err := c.inbox.Deliver(ctx, "c_1", ""); err != nil {
		t.Fatalf("Deliver (idle drain): %v", err)
	}
	if deliveries != 1 {
		t.Fatalf("deliveries after the idle drain = %d, want 1", deliveries)
	}

	// Step 3: recoverOne's post-resume replay.
	c.replayInbox(ctx, "c_1")

	if deliveries != 1 {
		t.Fatalf("deliveries after replayInbox = %d, want 1 (still) — "+
			"the row was redelivered a second time, exactly the duplicate I2 describes", deliveries)
	}
}
