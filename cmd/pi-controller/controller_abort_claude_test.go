package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/child"
	"git.graveland.dev/brent/pi-controller/protocol"
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

	res, err := ctrl.Spawn(ctx, req)
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
	body := "#!/bin/bash\n" +
		"SID=sess-abort\n" +
		"for a in \"$@\"; do if [ \"$prev\" = \"--resume\" ]; then SID=\"$a\"; fi; prev=\"$a\"; done\n" +
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
	if got := chAfter.Metadata().SessionID; got != "sess-abort" {
		t.Fatalf("resumed session id = %q, want sess-abort", got)
	}
}

// TestSend_PiAbortForwardedNatively guards the constraint that abort for a
// non-claude (pi) child is NOT intercepted: it must be written to the child's
// stdin (pi handles it natively in --mode rpc) and the process must NOT be
// interrupted/respawned.
func TestSend_PiAbortForwardedNatively(t *testing.T) {
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil) // fake-pi child (kind defaults to pi)

	chBefore, ok := ctrl.cm.Get(childID)
	if !ok {
		t.Fatalf("child %s not live", childID)
	}
	pidBefore := chBefore.PID()

	if err := ctrl.Send(childID, []byte(`{"type":"abort"}`)); err != nil {
		t.Fatalf("send abort: %v", err)
	}

	// The abort frame must reach the child's stdin (forwarded, not intercepted).
	deadline := time.Now().Add(2 * time.Second)
	forwarded := false
	for time.Now().Before(deadline) {
		for _, f := range chBefore.InSnapshot() {
			if isAbortFrame(f) {
				forwarded = true
				break
			}
		}
		if forwarded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !forwarded {
		t.Fatal("pi abort was not forwarded to child stdin")
	}

	// Same child, same process — a pi child must not be interrupted/respawned.
	chAfter, ok := ctrl.cm.Get(childID)
	if !ok {
		t.Fatal("pi child disappeared after abort")
	}
	if chAfter.PID() != pidBefore {
		t.Fatalf("pi child pid changed (%d -> %d): abort must be forwarded, not respawn", pidBefore, chAfter.PID())
	}
}
