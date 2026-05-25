package child_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/child"
	"graveland.dev/pi-controller/internal/protocol"
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
