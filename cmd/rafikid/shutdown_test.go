package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
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
	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, filepath.Join(dir, "c.sock"), nil, nil, nil, t.Context(), nil, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ctrl.ShutdownAllChildren(ctx, time.Second, time.Second); err != nil {
			t.Logf("newTestController cleanup: ShutdownAllChildren: %v", err)
		}
	})
	return ctrl
}

// spawnTestChild spawns a claude child (fake-pi.sh as the binary — the fake is
// ReadyOnSpawn so it reaches idle on process-up regardless of protocol) through
// the controller and waits for it to reach idle. env entries are forwarded to
// the child process.
func spawnTestChild(t *testing.T, ctrl *Controller, env map[string]string) string {
	t.Helper()

	req := protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
		Env:       env,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := ctrl.Spawn(ctx, req, users.Identity{})
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
func waitForExited(t *testing.T, st *childstore.Store, childID string, timeout time.Duration) {
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

// recordingChildStore captures every Upsert so tests can assert on what the
// controller actually wrote, in order. childstore.ChildStore's other two
// methods are bookkeeping this fake does not need to answer.
type recordingChildStore struct {
	mu      sync.Mutex
	upserts []childstore.ChildRecord
}

func (r *recordingChildStore) Upsert(_ context.Context, rec childstore.ChildRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts = append(r.upserts, rec)
	return nil
}

func (r *recordingChildStore) Delete(_ context.Context, _ string) error { return nil }

func (r *recordingChildStore) List(_ context.Context) ([]childstore.ChildRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]childstore.ChildRecord(nil), r.upserts...), nil
}

// lastUpsertFor returns the most recent record written for childID.
func (r *recordingChildStore) lastUpsertFor(childID string) (childstore.ChildRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found *childstore.ChildRecord
	for i := range r.upserts {
		if r.upserts[i].ChildID == childID {
			found = &r.upserts[i]
		}
	}
	if found == nil {
		return childstore.ChildRecord{}, false
	}
	return *found, true
}

// hasUpsertWithStatus reports whether any record for childID was written with
// the given status.
func (r *recordingChildStore) hasUpsertWithStatus(childID, status string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.upserts {
		if rec.ChildID == childID && rec.Status == status {
			return true
		}
	}
	return false
}

// waitForRemoval polls until the child is gone from the manager — the final
// step of handleChildExit, and the only signal that its whole exit sequence
// (including the row persist) has completed.
func waitForRemoval(t *testing.T, cm *ChildManager, childID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, alive := cm.Get(childID); !alive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %s still in the manager after %v", childID, timeout)
}

// TestKillPersistsExitedRow pins the exit path's write discipline: a child
// that ends while the daemon is healthy must persist status=exited — the one
// durable fact recovery reads, "this child ended while a daemon was alive to
// record it" — with last_status carrying the pre-exit state as observability.
// Writing the row BEFORE MarkExited (the old order) recorded the pre-exit
// status in BOTH columns, so the row never said exited and every restart
// resumed every agent that had ever finished.
func TestKillPersistsExitedRow(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	rec := &recordingChildStore{}
	ctrl.children = rec

	id := spawnTestChild(t, ctrl, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(ctx, id, 5_000, 2_000); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// Kill already waits on cm.Remove; this is belt and braces for the reader.
	waitForRemoval(t, ctrl.cm, id, 5*time.Second)

	last, ok := rec.lastUpsertFor(id)
	if !ok {
		t.Fatalf("no row was ever persisted for %s", id)
	}
	if last.Status != string(protocol.StatusExited) {
		t.Errorf("persisted status = %q, want %q — a daemon restart must see this child as terminal", last.Status, protocol.StatusExited)
	}
	if last.LastStatus != string(protocol.StatusIdle) {
		t.Errorf("persisted last_status = %q, want %q (the pre-exit state)", last.LastStatus, protocol.StatusIdle)
	}
}

// TestDaemonShutdownDoesNotPersistExitRows pins the other half of the write
// discipline: the daemon's own shutdown writes NOTHING to child rows. The
// process is dying and its children die with it, so the rows must keep saying
// idle/streaming — that is what lets the next daemon's recovery resume them.
// Persisting exits here is what turned every redeploy into mass terminal exit.
func TestDaemonShutdownDoesNotPersistExitRows(t *testing.T) {
	t.Parallel()

	ctrl := newTestController(t)
	rec := &recordingChildStore{}
	ctrl.children = rec

	id1 := spawnTestChild(t, ctrl, nil)
	id2 := spawnTestChild(t, ctrl, nil)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ctrl.ShutdownAllChildren(shutdownCtx, 5*time.Second, 2*time.Second); err != nil {
		t.Fatalf("ShutdownAllChildren: %v", err)
	}

	// The children DID end — in memory. The rows must not say so.
	for _, id := range []string{id1, id2} {
		waitForExited(t, ctrl.st, id, 5*time.Second)
		waitForRemoval(t, ctrl.cm, id, 5*time.Second)
		if rec.hasUpsertWithStatus(id, string(protocol.StatusExited)) {
			t.Errorf("child %s: an exited row was persisted during daemon shutdown; recovery would never resume it", id)
		}
	}
}
