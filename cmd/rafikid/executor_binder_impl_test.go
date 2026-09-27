package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/executors"

	"github.com/multigres/testkit/assert"
)

// The bug this reproduces: recover() calls WorkspaceMode(prevExec) only in
// the branch reached after IsLive(prevExec) already came back false --
// pkg/execpool removes an executor from the live set BEFORE parking it, so by
// the time WorkspaceMode runs the executor is already gone from Live(). A
// lookup that (like executorRow) only scans Live() therefore always misses,
// permanently disabling ephemeral migration on the tool-call path. The
// executors.Store row is durable and must answer regardless of whether the
// executor is currently connected.
func TestWorkspaceModeReadsTheDurableRowWhenTheExecutorIsNotLive(t *testing.T) {
	fes := newFakeExecStore()
	fes.execs["exec-gone"] = executors.Executor{
		ID: "exec-gone", WorkspaceMode: "ephemeral", Enabled: true,
	}
	c := &Controller{
		execPool:  &fakePool{live: nil}, // NOT live -- this is the state recover() sees
		execStore: fes,
	}
	b := &controllerBinder{c: c}

	got := b.WorkspaceMode("exec-gone")
	assert.NewAborting(t).Eq("ephemeral", got, "WorkspaceMode = %q, want %q -- a mode lookup scoped to the "+
		"live pool can never see an executor recover() is already treating "+
		"as not-live, which makes ephemeral tool-call migration dead code", got, "ephemeral")
}

func TestWorkspaceModeFallsBackToPinnedWithNoExecStore(t *testing.T) {
	c := &Controller{execPool: &fakePool{live: nil}}
	b := &controllerBinder{c: c}
	assert.NewAborting(t).Eq("pinned", b.WorkspaceMode("exec-unknown"), "WorkspaceMode")
}

func TestWorkspaceModeFallsBackToPinnedOnStoreError(t *testing.T) {
	fes := newFakeExecStore()
	c := &Controller{execPool: &fakePool{live: nil}, execStore: fes}
	b := &controllerBinder{c: c}
	// exec-missing was never registered: fakeExecStore.Get answers ErrNotFound.
	assert.NewAborting(t).Eq("pinned", b.WorkspaceMode("exec-missing"), "WorkspaceMode")
}
