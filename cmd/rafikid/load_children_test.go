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

	c.replayInbox(ctx, "c_1")

	if len(delivered) != 1 {
		t.Fatalf("want the settle redelivered after restart, got %d batches", len(delivered))
	}
	if delivered[0].Source != "subagents" || delivered[0].Frags[0] != "agent c_2 settled" {
		t.Errorf("replayed batch = %+v", delivered[0])
	}
}

// TestReplayInboxIsScopedToOneChild is the multi-daemon trap in unit form.
// Child rows are shared across every daemon; replaying c_1 must never touch
// c_2's unconfirmed rows, or a restarting daemon would resurrect and
// double-deliver messages belonging to a sibling daemon that is still
// running c_2 fine.
//
// Mutation check: make replayInbox reset unscoped (e.g. loop over every
// known child, or call a bulk ResetSent) and this fails — c_2's row gets
// redelivered even though c_2 was never passed to replayInbox.
func TestReplayInboxIsScopedToOneChild(t *testing.T) {
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

	var delivered []inbox.Batch
	c := newTestController(t)
	c.inbox = inbox.NewQueue(inbox.QueueConfig{
		Store: st,
		Deliver: func(_ context.Context, b inbox.Batch) (bool, error) {
			delivered = append(delivered, b)
			return false, nil
		},
	})

	c.replayInbox(ctx, "c_1")

	if len(delivered) != 1 || delivered[0].ChildID != "c_1" {
		t.Fatalf("want only c_1 redelivered, got %+v", delivered)
	}

	// c_2's row must still be 'sent' (not reset to pending, not delivered):
	// Pending only returns rows in StatePending, so a leaked scope would show
	// up here as c_2's row becoming visible/redelivered.
	pendingC2, err := st.Pending(ctx, "c_2")
	if err != nil {
		t.Fatalf("Pending c_2: %v", err)
	}
	if len(pendingC2) != 0 {
		t.Fatalf("c_2's row was reset to pending by a replay scoped to c_1: %+v", pendingC2)
	}
}

// TestRecoveryDropsInboxWhenChildStaysExited proves recoverOne wires
// planStayExited to dropInboxForForgotten. A child that will not be resumed
// has no turn left to inject into and will never get one, so its queue must
// be terminated and recorded on the durable event log — not left pending
// forever, which is what "wait forever for news that already happened"
// looks like for the coordinator on the OTHER end of one of these rows.
func TestRecoveryDropsInboxWhenChildStaysExited(t *testing.T) {
	st := inbox.NewMemory()
	ctx := context.Background()
	if _, err := st.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "orphaned"}); err != nil {
		t.Fatalf("Accept: %v", err)
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

	rows, err := st.Pending(ctx, "c_1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a child staying exited must have its queue dropped, not left pending: %d rows survived", len(rows))
	}
	// Dropped rows are terminal, which is what lets the retention sweep ever
	// reach them.
	if n, err := st.Sweep(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("swept %d rows (err=%v); want 1 terminal (dropped) row", n, err)
	}

	select {
	case ev := <-events:
		errEv := ev.GetError()
		if errEv == nil || errEv.Code != "inbox_dropped" {
			t.Fatalf("want an inbox_dropped error event, got %+v", ev)
		}
	default:
		t.Fatal("want an inbox_dropped event published for the forgotten child's dropped queue")
	}
}
