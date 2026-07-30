package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/internal/server"
	"git.graveland.dev/brent/fundi/protocol"
)

// TestResume_ConcurrentCallsSpawnExactlyOneChild proves the fix for the
// Controller.Resume check-then-act race (task H8): two genuinely concurrent
// ctrl_resume calls for the same exited childID must result in exactly one
// forked OS process and exactly one successful SpawnResult; the loser must
// get a clear, immediate error (protocol.ErrNotResumable), never a second
// live process sharing the ref and never a block.
//
// Why this can't pass against the pre-fix Resume: without the spawnClaims
// guard, both goroutines read the store, see Status == StatusExited, and
// proceed to child.Spawn before either one deletes/replaces the record —
// there is no code path in the unguarded version that produces an error for
// the second caller, so BOTH calls return (SpawnResult, nil), which fails
// the "successes == 1" and "rejections == 1" assertions below. Separately,
// the process-count assertion counts actual forked OS processes via
// FUNDI_TEST_SPAWN_LOG, written by fake-pi.sh itself (an observation outside
// the controller's own bookkeeping) — so even a partial fix that only
// serializes the store mutation but still calls child.Spawn for both
// callers (i.e. a guard placed after the fork instead of around it) would
// still show 2 forked processes here and fail, which is the "a guard that
// only wraps the status check fixes nothing" case called out in the task.
func TestResume_ConcurrentCallsSpawnExactlyOneChild(t *testing.T) {
	ctrl := newTestController(t)

	id := spawnTestChild(t, ctrl, nil)

	killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(killCtx, id, 2000, 500); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, id, 5*time.Second)

	// Every fake-pi.sh invocation from this point on records itself here.
	// Not parallel-safe (t.Setenv forbids it anyway), and by design: this
	// test relies on running to completion before any t.Parallel() sibling
	// in this package resumes past its own Parallel() call and starts
	// spawning, so no other test's fake-pi invocations land in this log.
	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	t.Setenv("FUNDI_TEST_SPAWN_LOG", spawnLog)

	const n = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		res server.SpawnResult
		err error
	}
	results := make([]outcome, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier: both goroutines call Resume as close together as possible
			res, err := ctrl.Resume(context.Background(), id, "")
			results[i] = outcome{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var successes, rejections int
	for i, r := range results {
		if r.err == nil {
			successes++
			if r.res.ChildID != id {
				t.Errorf("result %d: ChildID = %q, want %q", i, r.res.ChildID, id)
			}
			continue
		}
		var ce *server.ControllerError
		if !errors.As(r.err, &ce) {
			t.Fatalf("result %d: error is not *server.ControllerError: %v", i, r.err)
		}
		if ce.Code != protocol.ErrNotResumable {
			t.Errorf("result %d: error code = %q, want %q", i, ce.Code, protocol.ErrNotResumable)
		}
		rejections++
	}

	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 (both concurrent Resume calls must not both spawn)", successes)
	}
	if rejections != 1 {
		t.Errorf("rejections = %d, want exactly 1", rejections)
	}

	lines := readNonEmptyLines(t, spawnLog)
	if len(lines) != 1 {
		t.Errorf("processes actually forked = %d (log: %v), want exactly 1 — a second live agent process sharing this childID's ref is the harm this guard exists to prevent", len(lines), lines)
	}
}

// TestRespawnChild_ConcurrentCallsSpawnExactlyOneChild is RespawnChild's
// counterpart to TestResume_ConcurrentCallsSpawnExactlyOneChild: it has the
// identical check-then-act-around-a-fork shape (see the doc comment on
// Controller.RespawnChild), reachable via two concurrent
// ctrl_send{new_session|switch_session} frames for the same exited childID
// racing through handleInterceptedSend. This calls RespawnChild directly
// (bypassing handleInterceptedSend's kill/subscriber-restore ceremony, which
// is orthogonal to the race) to isolate the same fork-in-window defect.
//
// Same non-pass argument as above: without the shared spawnClaims guard,
// both calls pass the exited-status check and fork before either replaces
// the record, so both would return success and the spawn log would show 2
// processes.
func TestRespawnChild_ConcurrentCallsSpawnExactlyOneChild(t *testing.T) {
	ctrl := newTestController(t)

	id := spawnTestChild(t, ctrl, nil)

	killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(killCtx, id, 2000, 500); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, id, 5*time.Second)

	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	t.Setenv("FUNDI_TEST_SPAWN_LOG", spawnLog)

	const n = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		res server.SpawnResult
		err error
	}
	results := make([]outcome, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := ctrl.RespawnChild(context.Background(), id, "")
			results[i] = outcome{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var successes, rejections int
	for i, r := range results {
		if r.err == nil {
			successes++
			if r.res.ChildID != id {
				t.Errorf("result %d: ChildID = %q, want %q", i, r.res.ChildID, id)
			}
			continue
		}
		var ce *server.ControllerError
		if !errors.As(r.err, &ce) {
			t.Fatalf("result %d: error is not *server.ControllerError: %v", i, r.err)
		}
		if ce.Code != protocol.ErrNotResumable {
			t.Errorf("result %d: error code = %q, want %q", i, ce.Code, protocol.ErrNotResumable)
		}
		rejections++
	}

	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 (both concurrent RespawnChild calls must not both spawn)", successes)
	}
	if rejections != 1 {
		t.Errorf("rejections = %d, want exactly 1", rejections)
	}

	lines := readNonEmptyLines(t, spawnLog)
	if len(lines) != 1 {
		t.Errorf("processes actually forked = %d (log: %v), want exactly 1", len(lines), lines)
	}
}

// TestResumeRespawn_CrossPathClaimIsShared proves the cross-function half of
// the race this task asked to investigate: a ctrl_resume racing an
// intercepted ctrl_send{new_session} for the same exited childID. If Resume
// and RespawnChild each had their own independent claim set, this would
// still race (each guard only sees calls to its own function) — this test
// fails unless both share Controller.spawnClaims.
func TestResumeRespawn_CrossPathClaimIsShared(t *testing.T) {
	ctrl := newTestController(t)

	id := spawnTestChild(t, ctrl, nil)

	killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(killCtx, id, 2000, 500); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, id, 5*time.Second)

	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	t.Setenv("FUNDI_TEST_SPAWN_LOG", spawnLog)

	start := make(chan struct{})
	var wg sync.WaitGroup

	var resumeRes server.SpawnResult
	var resumeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		resumeRes, resumeErr = ctrl.Resume(context.Background(), id, "")
	}()

	var respawnRes server.SpawnResult
	var respawnErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		respawnRes, respawnErr = ctrl.RespawnChild(context.Background(), id, "")
	}()

	close(start)
	wg.Wait()

	successes := 0
	for _, err := range []error{resumeErr, respawnErr} {
		if err == nil {
			successes++
			continue
		}
		var ce *server.ControllerError
		if !errors.As(err, &ce) {
			t.Fatalf("error is not *server.ControllerError: %v", err)
		}
		if ce.Code != protocol.ErrNotResumable {
			t.Errorf("error code = %q, want %q", ce.Code, protocol.ErrNotResumable)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 across Resume+RespawnChild for the same childID (resumeErr=%v respawnErr=%v, resumeRes=%+v respawnRes=%+v)",
			successes, resumeErr, respawnErr, resumeRes, respawnRes)
	}

	lines := readNonEmptyLines(t, spawnLog)
	if len(lines) != 1 {
		t.Errorf("processes actually forked = %d (log: %v), want exactly 1", len(lines), lines)
	}
}

func readNonEmptyLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}
