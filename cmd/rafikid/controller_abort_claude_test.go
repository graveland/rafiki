package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestIsAbortFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"abort", `{"type":"abort"}`, true},
		{"abort with id", `{"type":"abort","id":"x"}`, true},
		{"prompt", `{"type":"prompt","message":"hi"}`, false},
		{"steer", `{"type":"steer","message":"hi"}`, false},
		{"new_session", `{"type":"new_session"}`, false},
		{"garbage", `not json`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAbortFrame([]byte(tc.frame)); got != tc.want {
				t.Fatalf("isAbortFrame(%q) = %v, want %v", tc.frame, got, tc.want)
			}
		})
	}
}

// newClaudeTestChild spawns a claude child (using `script` as the claude binary)
// through the controller and waits for it to reach idle.
func newClaudeTestChild(t *testing.T, script string) (*Controller, string) {
	t.Helper()
	ctrl := newTestController(t)

	req := protocol.SpawnRequest{
		Kind:      "claude",
		Cwd:       t.TempDir(),
		PiBinary:  script,
		NoSession: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := ctrl.Spawn(ctx, req, users.Identity{})
	if err != nil {
		t.Fatalf("spawn claude: %v", err)
	}

	// Wait for SessionID to be sniffed (the fake emits system/init immediately).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := ctrl.cm.Get(res.ChildID); ok && ch.Metadata().SessionID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ctrl, res.ChildID
}

func TestHandleClaudeAbort_InterruptsAndResumes(t *testing.T) {
	// Fake claude: emits system/init, then loops reading stdin (blocking).
	// Does NOT trap SIGINT — relies on default SIGINT termination so the test
	// harness's externally-delivered SIGINT reliably kills the process.
	// Honors --resume by reusing the session id arg.
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude.sh")
	// The id is "fresh" on a cold spawn and "got-<arg>" only when --resume is
	// threaded through. So asserting the resumed child reports "got-fresh" proves
	// the abort path actually passed --resume <first session id>, not that the
	// fake happened to default to the expected value.
	body := "#!/bin/bash\n" +
		"SID=fresh\n" +
		"for a in \"$@\"; do if [ \"$prev\" = \"--resume\" ]; then SID=\"got-$a\"; fi; prev=\"$a\"; done\n" +
		"printf '%s\\n' \"{\\\"type\\\":\\\"system\\\",\\\"subtype\\\":\\\"init\\\",\\\"session_id\\\":\\\"$SID\\\",\\\"model\\\":\\\"claude-opus-4-8\\\"}\"\n" +
		"while IFS= read -r line; do :; done\n" +
		"while true; do sleep 0.05; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	c, childID := newClaudeTestChild(t, script)

	// Capture the live process PID before abort.
	chBefore, ok := c.cm.Get(childID)
	if !ok {
		t.Fatalf("child %s not live before abort", childID)
	}
	pidBefore := chBefore.PID()

	if err := c.Send(childID, []byte(`{"type":"abort"}`)); err != nil {
		t.Fatalf("send abort: %v", err)
	}

	// Child must still be live under the same childID (resumed), with a NEW pid,
	// and must reach idle.
	deadline := time.Now().Add(8 * time.Second)
	var chAfter *child.Child
	for time.Now().Before(deadline) {
		if ch, ok := c.cm.Get(childID); ok && ch.PID() != pidBefore && ch.Status() == protocol.StatusIdle {
			chAfter = ch
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if chAfter == nil {
		t.Fatal("child was not resumed under the same childID after abort")
	}
	if chAfter.PID() == pidBefore {
		t.Fatal("expected a new process after interrupt+resume")
	}

	// SessionID arrives on a later frame than the pid/status transition the loop
	// above waits for, so it needs its own wait. Asserting it immediately made
	// this test flaky under load: it would break out of the loop as soon as the
	// child was idle with a new pid, then fail on a session id that had simply
	// not been delivered yet ("resumed session id = \"\"").
	var gotSession string
	sessionDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(sessionDeadline) {
		if gotSession = chAfter.Metadata().SessionID; gotSession != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gotSession != "got-fresh" {
		t.Fatalf("resumed session id = %q, want got-fresh (proves --resume <id> was threaded)", gotSession)
	}
}

// TestBuildDarajaAbortSpec_UsesFreshSessionNotOriginalLaunch proves the whole
// reason handleDarajaClaudeAbort cannot pass spec=nil to Pool.Restart: nil
// tells daraja to reuse the ORIGINAL launch's ChildSpec, whose
// resume_session is empty for a fresh conversation. This spec must carry the
// freshly-sniffed session id instead, or the restarted process silently
// starts a brand-new conversation and discards history.
func TestBuildDarajaAbortSpec_UsesFreshSessionNotOriginalLaunch(t *testing.T) {
	snap := childstore.Snapshot{
		Model:         "claude-sonnet-5",
		ResumeSession: "", // the ORIGINAL launch's spec — empty for a fresh spawn
	}
	spec := buildDarajaAbortSpec(snap, "sess-live-sniffed")

	if spec.Kind != darajapb.Kind_KIND_CLAUDE {
		t.Fatalf("Kind = %v, want KIND_CLAUDE", spec.Kind)
	}
	if spec.Claude == nil {
		t.Fatal("Claude params must not be nil")
	}
	if spec.Claude.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the child's own model preserved across the restart", spec.Claude.Model)
	}
	if spec.Claude.ResumeSession != "sess-live-sniffed" {
		t.Errorf("ResumeSession = %q, want the freshly sniffed session id, not the original launch's (empty)", spec.Claude.ResumeSession)
	}
	if spec.Claude.PermissionMode != "bypassPermissions" {
		t.Errorf("PermissionMode = %q, want the same default claudeRunner launches with", spec.Claude.PermissionMode)
	}
}

// TestHandleClaudeAbort_DarajaChildTakesTheRestartBranch proves the branch
// predicate: a child whose snapshot carries rafiki/executor (set only by
// claudeRunner's daraja path, never the local-subprocess fallback) must be
// refused for lack of a wired daraja pool rather than silently falling
// through to the local-subprocess Interrupt()+wait+Resume dance, which would
// hang forever waiting on a process that will never report Exited the way a
// daraja-routed child's relay stream does.
func TestHandleClaudeAbort_DarajaChildTakesTheRestartBranch(t *testing.T) {
	ctrl := newTestController(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"sess-1\",\"model\":\"claude-sonnet-5\"}'\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	req := protocol.SpawnRequest{Kind: "claude", Cwd: t.TempDir(), PiBinary: script, NoSession: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req, users.Identity{})
	if err != nil {
		t.Fatalf("spawn claude: %v", err)
	}
	// Stamp the label a real daraja launch would have set (claudeRunner), on
	// a child that is actually a local subprocess — sufficient to drive the
	// branch predicate without standing up a real executor+daraja stack.
	if err := ctrl.st.Update(res.ChildID, func(s *childstore.Session) {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels["rafiki/executor"] = "01test0000000000000000000"
	}); err != nil {
		t.Fatalf("stamp label: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := ctrl.cm.Get(res.ChildID); ok && ch.Metadata().SessionID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	err = ctrl.Send(res.ChildID, []byte(`{"type":"abort"}`))
	if err == nil {
		t.Fatal("want an error: no daraja pool is wired in this test controller")
	}
	if got := err.Error(); got != "daraja pool not wired" {
		t.Fatalf("err = %q, want the daraja-pool-not-wired refusal (proves the label routed to handleDarajaClaudeAbort, not the local-subprocess path)", got)
	}
}
