package main

import (
	"context"

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

func (b *controllerBinder) NoteBinding(childID, executorID, workspaceID string) {
	mode := "pinned"
	if row, ok := b.c.executorRow(executorID); ok {
		mode = workspaceModeOrPinned(row.WorkspaceMode)
	}
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
