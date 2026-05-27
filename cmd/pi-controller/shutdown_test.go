package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/protocol"
	"git.graveland.dev/brent/pi-controller/internal/store"
)

// fakePiBin returns the path to fake-pi.sh for use in shutdown tests.
func fakePiBin(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")
	p := filepath.Join(repoRoot, "test", "integration", "fake-pi.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fake-pi.sh not found at %s: %v", p, err)
	}
	return p
}

// newTestController creates a Controller wired with temp directories. It does
// not start the sweeper — callers that need it must call startSweeper manually.
func newTestController(t *testing.T) *Controller {
	t.Helper()
	dir := testSocketDir(t)
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")
	for _, d := range []string{stateDir, logsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdirall %s: %v", d, err)
		}
	}
	st := store.New()
	return NewController(st, stateDir, logsDir, filepath.Join(dir, "c.sock"), nil)
}

// spawnTestChild spawns a fake-pi child through the controller and waits for
// it to reach idle. env entries are forwarded to the child process.
func spawnTestChild(t *testing.T, ctrl *Controller, env map[string]string) string {
	t.Helper()

	req := protocol.SpawnRequest{
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
		Env:       env,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := ctrl.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return res.ChildID
}

// TestController_ShutdownAllChildren_Basic spawns two fake children and verifies
// that ShutdownAllChildren drives them both to StatusExited cleanly.
func TestController_ShutdownAllChildren_Basic(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	id1 := spawnTestChild(t, ctrl, nil)
	id2 := spawnTestChild(t, ctrl, nil)

	// Both should be in the child manager as live children.
	for _, id := range []string{id1, id2} {
		if _, ok := ctrl.cm.Get(id); !ok {
			t.Fatalf("child %s not live before shutdown", id)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := ctrl.ShutdownAllChildren(shutdownCtx, 5*time.Second, 2*time.Second); err != nil {
		t.Fatalf("ShutdownAllChildren: %v", err)
	}

	// monitorChild (running in a goroutine) calls handleChildExit asynchronously
	// after the process exits. Poll for StatusExited in the store.
	for _, id := range []string{id1, id2} {
		waitForExited(t, ctrl.st, id, 5*time.Second)
	}
}

// TestController_ShutdownAllChildren_Empty verifies no error and no panic when
// there are no live children.
func TestController_ShutdownAllChildren_Empty(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	ctx := context.Background()
	if err := ctrl.ShutdownAllChildren(ctx, time.Second, time.Second); err != nil {
		t.Fatalf("unexpected error with no children: %v", err)
	}
}

// TestController_ShutdownAllChildren_CtxExpires verifies that ShutdownAllChildren
// returns ctx.Err() when the context deadline is exceeded before all children exit.
//
// We use FAKE_PI_SHUTDOWN_DELAY=60 so the fake-pi lingers for 60 seconds after
// stdin closes. The per-child Shutdown timeouts (50ms + 200ms) are larger than
// the context (10ms), so the goroutines haven't sent a result when ctx fires.
func TestController_ShutdownAllChildren_CtxExpires(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)

	// Spawn a child that takes a long time to exit after stdin close.
	_ = spawnTestChild(t, ctrl, map[string]string{
		"FAKE_PI_SHUTDOWN_DELAY": "60",
	})

	// Very short context — expires before the child can finish shutting down.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := ctrl.ShutdownAllChildren(shutdownCtx, 50*time.Millisecond, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected ctx error, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}

	// The background goroutine will SIGTERM the child shortly after (within the
	// perChildShutdown window). Allow it to finish to avoid leaving orphaned
	// processes from the test suite.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	_ = ctrl.ShutdownAllChildren(cleanupCtx, 200*time.Millisecond, 500*time.Millisecond)
}

// waitForExited polls st until the child has StatusExited or the deadline passes.
func waitForExited(t *testing.T, st *store.Store, childID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if snap, ok := st.Get(childID); ok && snap.Status == protocol.StatusExited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := st.Get(childID)
	t.Errorf("child %s: status=%v after %v, want exited", childID, snap.Status, timeout)
}
