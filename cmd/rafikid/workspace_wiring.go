package main

import (
	"context"
	"fmt"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/workspace"
)

// provisionWorkspace derives a grant from the child's cwd and provisions it
// on the executor. The ordering matters: provision BEFORE the child is
// registered, so a provisioning failure refuses the spawn with nothing to
// clean up.
func (c *Controller) provisionWorkspace(
	ctx context.Context,
	req protocol.SpawnRequest, executorID string,
) (workspaceID string, roots []string, exec tools.ExecutorClient, err error) {
	if c.execPool == nil {
		return "", nil, nil, nil
	}

	// Derive the grant from the child's worktree.
	grant, err := workspace.Derive(req.Cwd, workspace.ModeEphemeral)
	if err != nil {
		return "", nil, nil, fmt.Errorf("grant derivation: %w", err)
	}

	// Build provision request.
	provisionReq := &executorpb.ProvisionRequest{
		ChildId:       "", // set by caller
		WorkspaceMode: string(workspace.ModeEphemeral),
		Workdir:       grant.Workdir,
		Network:       grant.Network,
	}
	for _, m := range grant.Mounts {
		provisionReq.Mounts = append(provisionReq.Mounts, &executorpb.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		})
	}

	resp, err := c.execPool.Provision(ctx, executorID, provisionReq)
	if err != nil {
		return "", nil, nil, fmt.Errorf("provision on executor %s: %w", executorID[:12], err)
	}

	// Get a workspace-scoped client.
	exec, err = c.execPool.ClientForWorkspace(executorID, resp.WorkspaceId)
	if err != nil {
		_ = c.execPool.Release(ctx, executorID, resp.WorkspaceId)
		return "", nil, nil, fmt.Errorf("workspace client: %w", err)
	}

	slog.Info("provisioned workspace",
		"workspaceId", resp.WorkspaceId,
		"executorId", executorID[:12],
		"roots", resp.Roots,
		"isolation", resp.Isolation,
	)

	return resp.WorkspaceId, resp.Roots, exec, nil
}

// releaseWorkspace tears down a workspace on an executor.
// Idempotent and best-effort — it must not wedge teardown.
func (c *Controller) releaseWorkspace(ctx context.Context, executorID, workspaceID string) {
	if c.execPool == nil || workspaceID == "" {
		return
	}
	if err := c.execPool.Release(ctx, executorID, workspaceID); err != nil {
		slog.Warn("release workspace failed",
			"workspaceId", workspaceID,
			"executorId", executorID[:12],
			"error", err,
		)
	}
}

// selectExecutorID is like selectExecutor but returns the executor ID alongside
// the client, so callers can provision workspaces.
func (c *Controller) selectExecutorID(req protocol.SpawnRequest) (id string, cl tools.ExecutorClient, err error) {
	if c.execPool == nil {
		return "", nil, fmt.Errorf("executor selector requested but no executor listener is configured")
	}

	sel, err := executors.ParseSelector(req.ExecutorSelector)
	if err != nil {
		return "", nil, fmt.Errorf("invalid executor selector %q: %w", req.ExecutorSelector, err)
	}

	live := c.execPool.Live()
	if len(live) == 0 {
		return "", nil, fmt.Errorf("spawn refused: no executor satisfies %q (0 live executors)", req.ExecutorSelector)
	}

	for _, le := range live {
		if sel.Matches(le.Executor.Labels) {
			cl, err := c.execPool.ClientFor(le.Executor.ID)
			if err != nil {
				return "", nil, fmt.Errorf("executor %s selected but not reachable: %w", le.Executor.ID[:12], err)
			}
			return le.Executor.ID, cl, nil
		}
	}

	return "", nil, fmt.Errorf("spawn refused: no executor satisfies %q", req.ExecutorSelector)
}