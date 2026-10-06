package main

import (
	"context"
	"fmt"
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"

	"github.com/multigres/testkit/assert"
)

// Pinned means the child fails where it stood. Moving it onto a machine no
// operator marked interchangeable is the thing workspace_mode exists to
// prevent, and recover ignored it entirely -- so a pinned child migrated on
// the tool-call path while HandleExecutorLost still failed it on the park
// timeout, whichever fired first.
func TestRecoverRefusesToMigrateAPinnedChild(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "pinned"
	f.live = false // the executor is gone, not just its workspace
	b := newBoundExecutor("c1", f)
	_, _, err := b.clientFor(context.Background())
	c.NoError(err)

	c.False(b.recover(context.Background(), b.stale(), execpool.ErrExecutorLost, true), "a pinned child must not be re-bound to a different executor")
	c.LessOrEqual(1, f.chooseCalls, "selection ran again for a pinned child (")
}

// The machine is fine; only the in-memory workspace registry was lost to a
// restart. Re-provisioning in place is not a migration and is allowed for both
// modes.
func TestRecoverReprovisionsAPinnedChildOnItsOwnExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "pinned"
	f.live = true
	b := newBoundExecutor("c1", f)
	_, _, err := b.clientFor(context.Background())
	c.NoError(err)
	c.True(b.recover(context.Background(), b.stale(), execpool.ErrExecutorGone, true), "the executor is live; the workspace must be rebuilt in place")
}

func TestRecoverMigratesAnEphemeralChildAndTellsIt(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = false
	b := newBoundExecutor("c1", f)
	_, _, err := b.clientFor(context.Background())
	c.NoError(err)
	c.True(b.recover(context.Background(), b.stale(), execpool.ErrExecutorLost, true), "an ephemeral child moves")
	c.Eq(1, f.migrations, "migrations = %d, want 1 -- a child whose workspace was rebuilt "+
		"on another machine and is never told will report work as done that "+
		"no longer exists", f.migrations)
	c.StrContains(f.lastSteer, "NOT committed", "the steer must warn about uncommitted work, got")
}

// A stream that opened and then broke may have already run the command. Re-running
// `git push && rm -rf build` because the connection died mid-response is the worst
// thing this retry could do, and re-provisioning does not give a fresh filesystem:
// mounts are unset, so the new workspace has the same --root.
func TestExecuteDoesNotRetryASideEffectingToolAfterAStreamBreak(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("read: %w", execpool.ErrStreamBroken)
	b := newBoundExecutor("c1", f)

	_, err := b.Execute(context.Background(), "bash", nil)
	c.Error(err, "want the stream error surfaced")
	c.Eq(1, f.executeCalls, "bash ran %d times; a mid-stream break must not re-dispatch a "+
		"side-effecting tool", f.executeCalls)
}

// A dead cached connection surfaces through stream.Err, so Execute wraps it as
// ErrStreamBroken even though the transport refused to dial and nothing was
// sent. ErrRedialed is the proof the request never left, so it must win over the
// "maybe ran" classification -- otherwise a side-effecting tool is never
// re-bound and every later call hits the same corpse.
func TestExecuteRecoversASideEffectingToolWhenTheTransportRedialed(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("executor stream: unavailable: %w: %w", execpool.ErrRedialed, execpool.ErrStreamBroken)
	f.failTimes = 1
	b := newBoundExecutor("c1", f)

	_, err := b.Execute(context.Background(), "bash", nil)
	c.NoError(err, "the request never reached the executor; bash must be re-dispatched on a fresh binding")
	c.Eq(2, f.executeCalls, "executeCalls")
}

func TestExecuteRetriesAReadOnlyToolAfterAStreamBreak(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("read: %w", execpool.ErrStreamBroken)
	f.failTimes = 1
	b := newBoundExecutor("c1", f)

	_, err := b.Execute(context.Background(), "grep", nil)
	c.NoError(err, "grep is idempotent and must be retried")
	c.Eq(2, f.executeCalls, "executeCalls")
}

// A pre-dispatch failure never touched the machine, so every tool retries.
func TestExecuteRetriesAnyToolOnAPreDispatchFailure(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("x: %w", execpool.ErrParked)
	f.failTimes = 1
	b := newBoundExecutor("c1", f)

	_, err := b.Execute(context.Background(), "bash", nil)
	c.NoError(err, "ErrParked comes from ClientFor, before anything was sent")
	c.Eq(2, f.executeCalls, "executeCalls")
}

// This is the bug reported as "fundi agents can't reconnect after an executor
// restart, only a rafikid restart fixes it": boundExecutor caches its client
// and calls it directly, bypassing Pool.ClientFor (and its typed
// ErrParked/ErrExecutorLost/ErrDraining answers) on every call after the
// first bind. When the executor's TCP connection dies -- exactly what an
// executor restart does to the connection a running child is bound to -- the
// FIRST call against the dead cached client fails with a plain
// broken-pipe-shaped error from the transport, wrapped by
// workspaceClient.Execute as execpool.ErrDialFailed (see pool.go). Before that
// sentinel existed this carried no sentinel execpool recognized, so retryable
// refused a side-effecting tool like bash and the binding was never
// invalidated -- every later call hit the identical dead client forever.
func TestExecuteRecoversFromADeadConnectionOnASideEffectingTool(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("executor execute: write tcp: broken pipe: %w", execpool.ErrDialFailed)
	f.failTimes = 1
	b := newBoundExecutor("c1", f)

	if _, err := b.Execute(context.Background(), "bash", nil); err != nil {
		t.Fatalf("a pre-dispatch dead-connection failure never reached the executor "+
			"and must retry even a side-effecting tool: %v", err)
	}
	c.Eq(2, f.executeCalls, "executeCalls")

	// The child must not be stuck on the same dead client for every call after
	// this one either -- the whole point of the reported bug.
	_, err := b.Execute(context.Background(), "bash", nil)
	c.NoError(err, "a later call still failed; the binding never recovered")
}

func TestStartJobIsNeverRetriedAfterAStreamBreak(t *testing.T) {
	c := assert.NewAborting(t)
	f := newFakeBinder()
	f.mode = "ephemeral"
	f.live = true
	f.failWith = fmt.Errorf("x: %w", execpool.ErrStreamBroken)
	b := newBoundExecutor("c1", f)
	_, err := b.StartJob(context.Background(), "npm run dev")
	c.Error(err, "want the error surfaced")
	c.Eq(1, f.startJobCalls, "StartJob ran")
}
