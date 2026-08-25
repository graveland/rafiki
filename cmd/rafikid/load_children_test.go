package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
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
		{"idle pi does not", childstore.ChildRecord{Kind: protocol.KindPi, LastStatus: "idle"}, false},
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
// pinned child where it stood, and recovery must not become the one door that
// quietly migrates one. An unknown mode is pinned.
func TestRecoveryActionWorkspaceMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want recoveryPlan
	}{
		{"ephemeral rebinds", "ephemeral", planRebindUnbound},
		{"pinned fails", "pinned", planStayExited},
		{"unknown mode is pinned", "", planStayExited},
		{"garbage mode is pinned", "sideways", planStayExited},
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
		{
			"a crashed pi child still does not resume",
			childstore.ChildRecord{Kind: protocol.KindPi, Status: "idle", LastStatus: ""},
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
