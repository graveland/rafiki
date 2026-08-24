package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// controllerBinder is the Controller's executorBinder, bound to ONE spawn
// request and its attested owner.
//
// Bound at construction rather than taking a childID per method for the same
// reason newControllerSpawner is: a value carrying its own subject cannot be
// asked to act for a different one, and selection here is a confinement
// decision.
type controllerBinder struct {
	c         *Controller
	req       protocol.SpawnRequest
	ownerName string
}

func (c *Controller) binderFor(req protocol.SpawnRequest, ownerName string) executorBinder {
	return &controllerBinder{c: c, req: req, ownerName: ownerName}
}

// ChooseFor re-runs full selection. It is NOT a cached decision: the effective
// set is recomputed from the live pool every time, deliberately, so an
// executor that connects after the child started is usable by it.
//
// The error is explainNoMatch's per-candidate diagnostic, which boundExecutor
// surfaces to the agent verbatim.
func (b *controllerBinder) ChooseFor(string) (string, error) {
	chosen, err := b.c.chooseExecutor(b.req, b.ownerName)
	if err != nil {
		return "", err
	}
	return chosen.ID, nil
}

func (b *controllerBinder) ProvisionOn(ctx context.Context, executorID string) (string, tools.ExecutorClient, error) {
	wsID, _, cl, err := b.c.provisionWorkspace(ctx, b.req, executorID)
	if err != nil {
		return "", nil, err
	}
	return wsID, cl, nil
}

func (b *controllerBinder) ReleaseOn(ctx context.Context, executorID, workspaceID string) {
	b.c.releaseWorkspace(ctx, executorID, workspaceID)
}

// IsLive distinguishes "the workspace went" from "the machine went". The
// executor's workspace registry is in-memory, so a restart loses every id
// while the connection is healthy.
func (b *controllerBinder) IsLive(executorID string) bool {
	if b.c.execPool == nil {
		return false
	}
	for _, le := range b.c.execPool.Live() {
		if le.Executor.ID == executorID && le.Executor.Enabled {
			return true
		}
	}
	return false
}

// NoteBinding records where the child actually is.
//
// The childstore is the authority, not c.wsLabels: handleChildExit releases the
// workspace by snap.Labels, and HandleExecutorLost finds affected children by
// them. c.wsLabels is only a BRIDGE for the window before Spawn has inserted
// the record -- an eager bind happens inside agentRuntimeOptions, which runs
// before the session is stored -- and Spawn consumes it exactly once.
func (b *controllerBinder) NoteBinding(childID, executorID, workspaceID string) {
	mode := "pinned"
	if row, ok := b.c.executorRow(executorID); ok {
		mode = workspaceModeOrPinned(row.WorkspaceMode)
	}

	_, err := b.c.st.SetLabels(childID, map[string]string{
		"rafiki/workspace":      workspaceID,
		"rafiki/executor":       executorID,
		"rafiki/workspace-mode": mode,
	}, []string{"rafiki/executor-state"})
	if err == nil {
		return
	}
	if !errors.Is(err, childstore.ErrNotFound) {
		slog.Warn("could not record the child's executor binding; teardown will "+
			"not release this workspace",
			"child", childID, "executor", shortID(executorID), "error", err)
		return
	}

	// The child record does not exist yet. Stash for Spawn.
	b.c.wsLabelsMu.Lock()
	if b.c.wsLabels == nil {
		b.c.wsLabels = make(map[string]workspaceLabels)
	}
	b.c.wsLabels[childID] = workspaceLabels{
		workspaceID: workspaceID,
		executorID:  executorID,
		mode:        mode,
	}
	b.c.wsLabelsMu.Unlock()
}

// WorkspaceMode reads the mode from the executor's DURABLE row, never from the
// live pool and never from Describe.
//
// This runs from recover()'s tool-call path specifically in the branch where
// IsLive(executorID) already came back false -- pkg/execpool removes an
// executor from the live set BEFORE parking it (pool.go's removeLive-then-Park
// ordering), so by the time this is called the executor is already gone from
// Live() and a lookup through executorRow (which only scans Live()) would
// always miss, permanently disabling ephemeral migration on this path. The
// executors.Store row exists independently of whether the executor is
// currently connected, which is the whole point of asking it here.
//
// An absent store, a lookup error, or a not-found row all fall back to
// "pinned": unknown mode is pinned, and moving a child onto a machine no
// operator marked interchangeable is worse than failing it where it stood.
// Never the executor's self-report either way -- a machine that wants
// children must not be the one asserting it is interchangeable.
func (b *controllerBinder) WorkspaceMode(executorID string) string {
	if b.c.execStore == nil {
		return "pinned"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row, err := b.c.execStore.Get(ctx, executorID)
	if err != nil {
		slog.Warn("could not read executor row for workspace_mode; treating as pinned",
			"executor", shortID(executorID), "error", err)
		return "pinned"
	}
	return workspaceModeOrPinned(row.WorkspaceMode)
}

func (b *controllerBinder) NotifyMigrated(childID, fromExec, toExec string) {
	slog.Warn("child migrated to another executor",
		"child", childID, "from", shortID(fromExec), "to", shortID(toExec))
	b.c.sendSteer(childID, rescheduleSteer)
}
