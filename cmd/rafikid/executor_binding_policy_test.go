package main

import (
	"context"
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

	if b.recover(context.Background(), b.stale(), execpool.ErrExecutorLost) {
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
	if !b.recover(context.Background(), b.stale(), execpool.ErrExecutorGone) {
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
	if !b.recover(context.Background(), b.stale(), execpool.ErrExecutorLost) {
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
