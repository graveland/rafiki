package child_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/child"
	"git.graveland.dev/brent/pi-controller/protocol"
)

func fakePiPath(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")
	return filepath.Join(repoRoot, "test", "integration", "fake-pi.sh")
}

func TestChild_SpawnAndCleanShutdown(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	// Wait for the supervise loop to enter the read/write loop.
	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Graceful shutdown should exit cleanly without escalation.
	res, err := c.Shutdown(time.Second*5, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated {
		t.Fatal("clean exit should not need SIGTERM")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code: %d", res.ExitCode)
	}
}

func TestChild_StuckProcess_Escalates(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SHUTDOWN_DELAY=999"},
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Short shutdown timeout; expect escalation to SIGTERM.
	res, err := c.Shutdown(100*time.Millisecond, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatal("expected escalation")
	}
}

func TestChild_BinaryMissing_SpawnFails(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: "/this/path/does/not/exist",
	}
	_, err := child.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("expected spawn failure")
	}
}

func TestChild_KickstartAndMetadata(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SESSION_ID=test-sid", "FAKE_PI_SESSION_NAME=initial"},
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	// Wait for the kickstart get_state response to arrive.
	select {
	case <-c.Idle():
	case <-time.After(2 * time.Second):
		t.Fatalf("did not transition to idle: %v", c.Status())
	}

	if c.Status() != protocol.StatusIdle {
		t.Fatalf("status after idle: %v", c.Status())
	}

	md := c.Metadata()
	if md.SessionID != "test-sid" {
		t.Fatalf("sessionId: got %q, want %q", md.SessionID, "test-sid")
	}
	if md.SessionName != "initial" {
		t.Fatalf("sessionName: got %q, want %q", md.SessionName, "initial")
	}
	if md.SessionFile == "" {
		t.Errorf("SessionFile not extracted")
	}
	if md.Model == "" {
		t.Errorf("Model not extracted")
	}
}

func TestChild_BeginShutdown(t *testing.T) {
	// BeginShutdown should drive the SM from idle to shutting_down and report
	// the previous status correctly.
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	// Wait until the child is idle (SM is in StatusIdle).
	select {
	case <-c.Idle():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not reach idle")
	}

	if c.Status() != protocol.StatusIdle {
		t.Fatalf("pre-shutdown status: %v", c.Status())
	}

	// First call: should transition idle → shutting_down.
	changed, prev := c.BeginShutdown()
	if !changed {
		t.Fatal("BeginShutdown: expected transition to occur")
	}
	if prev != protocol.StatusIdle {
		t.Fatalf("BeginShutdown: prev=%v, want idle", prev)
	}
	if c.Status() != protocol.StatusShuttingDown {
		t.Fatalf("status after BeginShutdown: %v", c.Status())
	}

	// Second call: already shutting_down, should be a no-op.
	changed2, _ := c.BeginShutdown()
	if changed2 {
		t.Fatal("BeginShutdown: second call should not report a transition")
	}
}

func TestChild_ProcessExits_ReadyStillFires(t *testing.T) {
	// If pi exits immediately Done() must still close — the supervise loop
	// must reap the process and signal completion even without any output.
	spec := child.SpawnSpec{
		ChildID:   "c_test",
		Cwd:       t.TempDir(),
		PiBinary:  "/bin/sh",
		ExtraArgs: []string{"-c", "exit 0"},
	}
	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Shutdown(100*time.Millisecond, 100*time.Millisecond)
	})

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed for instantly-exiting child")
	}
}

func TestChild_InterruptSendsSIGINT(t *testing.T) {
	// The fake child installs NO signal trap, so a default-disposition SIGINT
	// terminates it and is recorded as exit signal "interrupt". Asserting on the
	// recorded exit signal proves Interrupt() delivers SIGINT specifically,
	// without depending on a shell trap firing — trapped-SIGINT delivery to bash
	// subprocesses is unreliable under the `go test` harness (an externally sent
	// SIGINT terminates the shell but its INT trap does not run, while a
	// self-sent one does). The graceful claude-interrupt behavior (the
	// "[Request interrupted by user]" result) is covered by the live smoke test.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	body := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s1\"}'\n" +
		"while true; do sleep 0.05; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cwd, _ := os.Getwd()
	ch, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_int",
		Cwd:      cwd,
		PiBinary: script,
		Provider: child.ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = ch.Shutdown(100*time.Millisecond, 100*time.Millisecond) })

	select {
	case <-ch.Idle():
	case <-time.After(3 * time.Second):
		t.Fatal("never idle")
	}
	time.Sleep(100 * time.Millisecond) // ensure the process is fully running

	if err := ch.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sig := ch.ExitResult().Signal; sig != "" {
			if sig != syscall.SIGINT.String() {
				t.Fatalf("child exit signal = %q, want %q", sig, syscall.SIGINT.String())
			}
			return // exited via SIGINT — Interrupt() delivered the right signal
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("child did not exit after Interrupt()")
}

func TestChild_InterruptAfterExitIsNoOp(t *testing.T) {
	// Interrupt() on an already-exited child must be a no-op returning nil.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	body := "#!/bin/bash\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cwd, _ := os.Getwd()
	ch, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_int2",
		Cwd:      cwd,
		PiBinary: script,
		Provider: child.ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	// Wait for the process to exit and be reaped (Done closes after the supervise
	// loop sets closed), so Interrupt hits the already-closed branch.
	select {
	case <-ch.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
	if err := ch.Interrupt(); err != nil {
		t.Fatalf("Interrupt() on exited child = %v, want nil", err)
	}
}
