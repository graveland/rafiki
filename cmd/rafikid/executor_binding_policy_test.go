package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
)

// Pinned means the child fails where it stood. Moving it onto a machine no
// operator marked interchangeable is the thing workspace_mode exists to
// prevent, and recover ignored it entirely -- so a pinned child migrated on
// the tool-call path while HandleExecutorLost still failed it on the park
// timeout, whichever fired first.
func TestRecoverRefusesToMigrateAPinnedChild(t *testing.T) {
	f := newFakeBinder()
	f.mode = "pinned"
	f.live = false // the executor is gone, not just its workspace
	b := newBoundExecutor("c1", f)
	if _, _, err := b.clientFor(context.Background()); err != nil {
		t.Fatal(err)
	}

	if b.recover(context.Background(), b.stale(), execpool.ErrExecutorLost, true) {
		t.Fatal("a pinned child must not be re-bound to a different executor")
	}
	if f.chooseCalls > 1 {
		t.Fatalf("selection ran again for a pinned child (%d calls)", f.chooseCalls)
	}
}

// The machine is fine; only the in-memory workspace registry was lost to a
// restart. Re-provisioning in place is not a migration and is allowed for both
// modes.
func TestRecoverReprovisionsAPinnedChildOnItsOwnExecutor(t *testing.T) {
	f := newFakeBinder()
	f.mode = "pinned"
	f.live = true
	b := newBoundExecutor("c1", f)
	if _, _, err := b.clientFor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !b.recover(context.Background(), b.stale(), execpool.ErrExecutorGone, true) {
		t.Fatal("the executor is live; the workspace must be rebuilt in place")
	}
}

func TestRecoverMigratesAnEphemeralChildAndTellsIt(t *testing.T) {
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = false
	b := newBoundExecutor("c1", f)
	if _, _, err := b.clientFor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !b.recover(context.Background(), b.stale(), execpool.ErrExecutorLost, true) {
		t.Fatal("an ephemeral child moves")
	}
	if f.migrations != 1 {
		t.Fatalf("migrations = %d, want 1 -- a child whose workspace was rebuilt "+
			"on another machine and is never told will report work as done that "+
			"no longer exists", f.migrations)
	}
	if !strings.Contains(f.lastSteer, "NOT committed") {
		t.Fatalf("the steer must warn about uncommitted work, got %q", f.lastSteer)
	}
}

// A stream that opened and then broke may have already run the command. Re-running
// `git push && rm -rf build` because the connection died mid-response is the worst
// thing this retry could do, and re-provisioning does not give a fresh filesystem:
// mounts are unset, so the new workspace has the same --root.
func TestExecuteDoesNotRetryASideEffectingToolAfterAStreamBreak(t *testing.T) {
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("read: %w", execpool.ErrStreamBroken)
	b := newBoundExecutor("c1", f)

	if _, err := b.Execute(context.Background(), "bash", nil); err == nil {
		t.Fatal("want the stream error surfaced")
	}
	if f.executeCalls != 1 {
		t.Fatalf("bash ran %d times; a mid-stream break must not re-dispatch a "+
			"side-effecting tool", f.executeCalls)
	}
}

func TestExecuteRetriesAReadOnlyToolAfterAStreamBreak(t *testing.T) {
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("read: %w", execpool.ErrStreamBroken)
	f.failTimes = 1
	b := newBoundExecutor("c1", f)

	if _, err := b.Execute(context.Background(), "grep", nil); err != nil {
		t.Fatalf("grep is idempotent and must be retried: %v", err)
	}
	if f.executeCalls != 2 {
		t.Fatalf("executeCalls = %d, want 2", f.executeCalls)
	}
}

// A pre-dispatch failure never touched the machine, so every tool retries.
func TestExecuteRetriesAnyToolOnAPreDispatchFailure(t *testing.T) {
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("x: %w", execpool.ErrParked)
	f.failTimes = 1
	b := newBoundExecutor("c1", f)

	if _, err := b.Execute(context.Background(), "bash", nil); err != nil {
		t.Fatalf("ErrParked comes from ClientFor, before anything was sent: %v", err)
	}
	if f.executeCalls != 2 {
		t.Fatalf("executeCalls = %d, want 2", f.executeCalls)
	}
}

func TestStartJobIsNeverRetriedAfterAStreamBreak(t *testing.T) {
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("x: %w", execpool.ErrStreamBroken)
	b := newBoundExecutor("c1", f)
	if _, err := b.StartJob(context.Background(), "npm run dev"); err == nil {
		t.Fatal("want the error surfaced")
	}
	if f.startJobCalls != 1 {
		t.Fatalf("StartJob ran %d times; retrying it launches the job twice",
			f.startJobCalls)
	}
}
