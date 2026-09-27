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

	"github.com/multigres/testkit/assert"
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
			got := isAbortFrame([]byte(tc.frame))
			assert.NewAborting(t).Eq(tc.want, got, "isAbortFrame(%q) = %v, want", tc.frame, got)
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
	assert.NewAborting(t).NoError(err, "spawn claude")

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
	ck := assert.NewAborting(t)
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
	ck.NoError(os.WriteFile(script, []byte(body), 0o755), "write fake")

	c, childID := newClaudeTestChild(t, script)

	// Capture the live process PID before abort.
	chBefore, ok := c.cm.Get(childID)
	ck.True(ok, "child %s not live before abort", childID)
	pidBefore := chBefore.PID()

	ck.NoError(c.Send(childID, []byte(`{"type":"abort"}`)), "send abort")

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
	ck.NotNil(chAfter, "child was not resumed under the same childID after abort")
	ck.NotEq(pidBefore, chAfter.PID(), "expected a new process after interrupt+resume")

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
	ck.Eq("got-fresh", gotSession, "resumed session id")
}

// TestBuildDarajaAbortSpec_UsesFreshSessionNotOriginalLaunch proves the whole
// reason handleDarajaClaudeAbort cannot pass spec=nil to Pool.Restart: nil
// tells daraja to reuse the ORIGINAL launch's ChildSpec, whose
// resume_session is empty for a fresh conversation. This spec must carry the
// freshly-sniffed session id instead, or the restarted process silently
// starts a brand-new conversation and discards history.
func TestBuildDarajaAbortSpec_UsesFreshSessionNotOriginalLaunch(t *testing.T) {
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{
		Model:         "claude-sonnet-5",
		ResumeSession: "", // the ORIGINAL launch's spec — empty for a fresh spawn
	}
	spec := buildDarajaAbortSpec(snap, "sess-live-sniffed")

	c.Require().Eq(darajapb.Kind_KIND_CLAUDE, spec.Kind, "Kind")
	c.Require().NotNil(spec.Claude, "Claude params must not be nil")
	c.Eq("claude-sonnet-5", spec.Claude.Model, "Model")
	c.Eq("sess-live-sniffed", spec.Claude.ResumeSession, "ResumeSession")
	c.Eq("bypassPermissions", spec.Claude.PermissionMode, "PermissionMode")
}

// TestHandleClaudeAbort_DarajaChildTakesTheRestartBranch proves the branch
// predicate: a child whose snapshot carries rafiki/executor (set only by
// claudeRunner's daraja path, never the local-subprocess fallback) must be
// refused for lack of a wired daraja pool rather than silently falling
// through to the local-subprocess Interrupt()+wait+Resume dance, which would
// hang forever waiting on a process that will never report Exited the way a
// daraja-routed child's relay stream does.
func TestHandleClaudeAbort_DarajaChildTakesTheRestartBranch(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude.sh")
	c.NoError(os.WriteFile(script, []byte("#!/bin/bash\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"sess-1\",\"model\":\"claude-sonnet-5\"}'\ncat >/dev/null\n"), 0o755), "write fake claude")

	req := protocol.SpawnRequest{Kind: "claude", Cwd: t.TempDir(), PiBinary: script, NoSession: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req, users.Identity{})
	c.NoError(err, "spawn claude")
	// Stamp the label a real daraja launch would have set (claudeRunner), on
	// a child that is actually a local subprocess — sufficient to drive the
	// branch predicate without standing up a real executor+daraja stack.
	c.NoError(ctrl.st.Update(res.ChildID, func(s *childstore.Session) {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels["rafiki/executor"] = "01test0000000000000000000"
	}), "stamp label")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := ctrl.cm.Get(res.ChildID); ok && ch.Metadata().SessionID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	err = ctrl.Send(res.ChildID, []byte(`{"type":"abort"}`))
	c.Error(err, "want an error: no daraja pool is wired in this test controller")
	c.Eq("daraja pool not wired", err.Error(), "err")
}

// TestBuildDarajaAbortSpec_CarriesArgvShapedRestartFields pins the two
// argv-shaped restart fields the abort spec must round-trip from the snapshot:
// daraja rebuilds the child's argv from this spec on every Restart, so
// dropping AppendSystemPrompt or ExtraArgs here silently restarts the child
// without the system-prompt appendix and operator flags it was launched with.
// (The launch-only block — proxy URL/token, passthrough, auto-compact,
// record-requests — stays deliberately omitted: Restart never rebuilds the
// environment, only argv.)
func TestBuildDarajaAbortSpec_CarriesArgvShapedRestartFields(t *testing.T) {
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{
		Model:              "claude-sonnet-5",
		AppendSystemPrompt: "be terse",
		ExtraArgs:          []string{"--foo", "bar"},
	}
	spec := buildDarajaAbortSpec(snap, "sess-live-sniffed")

	c.Require().NotNil(spec.Claude, "Claude params must not be nil")
	c.Eq("be terse", spec.Claude.AppendSystemPrompt, "AppendSystemPrompt")
	c.EqDiff([]string{"--foo", "bar"}, spec.Claude.ExtraArgs, "ExtraArgs")
}
