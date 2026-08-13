package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
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

// HandleExecutorLost is called by the exec pool when a parked executor's
// timeout expires and it is declared permanently lost. For each child on the
// lost executor, it either re-provisions (ephemeral) or fails (pinned).
func (c *Controller) HandleExecutorLost(lostID string) {
	slog.Warn("executor lost — handling children", "executorId", lostID[:12])

	live := c.execPool.Live()
	children := c.st.List()

	for _, snap := range children {
		if snap.Labels["rafiki/executor"] != lostID {
			continue
		}
		wsID := snap.Labels["rafiki/workspace"]
		if wsID == "" {
			continue
		}

		// Check the workspace mode from the child's labels.
		mode := snap.Labels["rafiki/workspace-mode"]
		if mode == "" {
			mode = "ephemeral"
		}

		// Release the dead workspace first.
		c.releaseWorkspace(context.Background(), lostID, wsID)

		if mode == "pinned" {
			// Pinned children cannot move — fail them.
			slog.Warn("pinned child lost with executor",
				"childId", snap.ChildID, "executorId", lostID[:12])
			c.failChild(snap.ChildID, "executor lost — pinned workspace cannot be rescheduled")
			continue
		}

		// Ephemeral: try to re-provision on another matching executor.
		if !c.tryReschedule(snap, live) {
			slog.Error("ephemeral child cannot be rescheduled — no matching executor",
				"childId", snap.ChildID)
			c.failChild(snap.ChildID, "executor lost — no matching executor available for reschedule")
		}
	}
}

// tryReschedule attempts to re-provision an ephemeral child on another
// matching executor. Returns true on success.
func (c *Controller) tryReschedule(snap childstore.Snapshot, live []execpool.LiveExecutor) bool {
	if len(live) == 0 {
		return false
	}

	// Re-provision on the first available matching executor.
	// In a real implementation this would match the child's original selector.
	for _, le := range live {
		if le.Describe.WorkspaceMode == "ephemeral" {
			wsID, _, _, err := c.provisionWorkspace(context.Background(),
				protocol.SpawnRequest{Cwd: snap.Cwd}, le.Executor.ID)
			if err != nil {
				slog.Warn("reschedule provision failed",
					"childId", snap.ChildID, "executorId", le.Executor.ID[:12], "error", err)
				continue
			}

			// Update the child's store labels with the new workspace.
			c.st.SetLabels(snap.ChildID, map[string]string{
				"rafiki/workspace": wsID,
				"rafiki/executor":  le.Executor.ID,
			}, nil)

			// Steer the child about its new workspace.
			c.sendSteer(snap.ChildID, rescheduleSteer)

			slog.Info("rescheduled child on new executor",
				"childId", snap.ChildID, "newExecutorId", le.Executor.ID[:12])
			return true
		}
	}
	return false
}

// failChild sends a fatal steer to the child and transitions it to failed.
func (c *Controller) failChild(childID, reason string) {
	c.sendSteer(childID, reason)
	// Best-effort: the steer tells the child it's done.
	// Force-kill after a short grace period.
	time.AfterFunc(10*time.Second, func() {
		c.Kill(context.Background(), childID, 5000, 1000)
	})
}

// sendSteer injects a steer message into the child's event stream.
func (c *Controller) sendSteer(childID, message string) {
	if c.evbuf != nil {
		c.evbuf.PushSteer(childID, "executor", message)
	}
}

const rescheduleSteer = `YOUR WORKSPACE WAS REBUILT on a different machine. The previous one is gone.
Anything you had NOT committed is lost. Re-check the state of the working tree
before continuing, and do not report as done anything you cannot now see.`