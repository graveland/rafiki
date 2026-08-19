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
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorRow returns the operator's record for a live executor.
//
// The ROW is the authority on what an executor is. Describe and the Provision
// response are the executor's own account of itself, and an executor that could
// name its own isolation could tell a child it is sandboxed when it is not.
func (c *Controller) executorRow(executorID string) (executors.Executor, bool) {
	if c.execPool == nil {
		return executors.Executor{}, false
	}
	for _, le := range c.execPool.Live() {
		if le.Executor.ID == executorID {
			return le.Executor, true
		}
	}
	return executors.Executor{}, false
}

// executorToolsFor returns the served tool set the chosen executor reported in
// its last Describe, or nil when unknown (no pool, no live entry, or no
// Describe yet). The caller treats nil as "leave routing unfiltered", which
// is what keeps the legacy socket path and a pre-Describe join working.
func (c *Controller) executorToolsFor(executorID string) []string {
	if c.execPool == nil {
		return nil
	}
	for _, le := range c.execPool.Live() {
		if le.Executor.ID == executorID && le.Describe != nil {
			return le.Describe.Tools
		}
	}
	return nil
}

// provisionWorkspace provisions a workspace on the executor. The ordering
// matters: provision BEFORE the child is registered, so a provisioning failure
// refuses the spawn with nothing to clean up.
func (c *Controller) provisionWorkspace(
	ctx context.Context,
	req protocol.SpawnRequest, executorID string,
) (workspaceID string, wi *fundi.WorkspaceInfo, exec tools.ExecutorClient, err error) {
	pool := c.execPool
	if pool == nil {
		return "", nil, nil, nil
	}
	pp, ok := pool.(*execpool.Pool)
	if !ok {
		return "", nil, nil, fmt.Errorf("execpool: not a real pool")
	}

	// ChildId only.
	//
	// Mounts, network and workdir are all left unset. The executor serves the
	// filesystem it can see, working from the --root it was started with; for a
	// container executor that view was chosen in `docker run -v ...` by the
	// operator, exactly as they chose the image. rafiki's grant is the label
	// selector plus the row — nothing here composes a path, and a workdir sent
	// from here would be a host path with no meaning inside the container.
	resp, err := pp.Provision(ctx, executorID, &executorpb.ProvisionRequest{ChildId: ""})
	if err != nil {
		return "", nil, nil, fmt.Errorf("provision on executor %s: %w", shortID(executorID), err)
	}

	// Get a workspace-scoped client.
	exec, err = pp.ClientForWorkspace(executorID, resp.WorkspaceId)
	if err != nil {
		_ = pp.Release(ctx, executorID, resp.WorkspaceId)
		return "", nil, nil, fmt.Errorf("workspace client: %w", err)
	}

	// Everything the child is TOLD about its machine comes from the row, not
	// from resp. The executor reports isolation "none" for every workspace —
	// it does not know whether it is in a container, and this branch forbids it
	// from finding out — so believing resp here is what made the "Your machine"
	// block silently vanish for exactly the children that need it.
	row, haveRow := c.executorRow(executorID)
	if !haveRow {
		slog.Warn("provisioned a workspace on an executor with no live row",
			"workspaceId", resp.WorkspaceId, "executorId", shortID(executorID))
	}

	slog.Info("provisioned workspace",
		"workspaceId", resp.WorkspaceId,
		"executorId", shortID(executorID),
		"roots", row.Roots,
		"isolation", row.Isolation,
		"workspaceMode", workspaceModeOrPinned(row.WorkspaceMode),
	)

	return resp.WorkspaceId, workspaceInfoFromRow(row), exec, nil
}

// workspaceInfoFromRow builds what the child is TOLD about its machine.
//
// It takes a row and nothing else, and that signature is the point. The
// executor reports isolation "none" for every workspace it provisions — it does
// not know whether it is running in a container, and it is not allowed to find
// out — so a version of this that read the Provision response produced
// Isolation "none" for every child, which silently suppressed the whole "Your
// machine" block for exactly the sandboxed workers it exists to warn.
func workspaceInfoFromRow(row executors.Executor) *fundi.WorkspaceInfo {
	return &fundi.WorkspaceInfo{
		Isolation:     row.Isolation,
		WorkspaceMode: workspaceModeOrPinned(row.WorkspaceMode),
		Roots:         row.Roots,
	}
}

// workspaceModeOrPinned resolves an unset workspace_mode to "pinned".
//
// Pinned is the conservative reading in both places this is used. In the
// system prompt it promises the child less (its tree stays put); on the
// rafiki/workspace-mode label it means an executor loss FAILS the child rather
// than silently moving it onto a machine the operator never marked as
// interchangeable.
func workspaceModeOrPinned(mode string) string {
	if mode == "" {
		return "pinned"
	}
	return mode
}

// releaseWorkspace tears down a workspace on an executor.
// Idempotent and best-effort — it must not wedge teardown.
func (c *Controller) releaseWorkspace(ctx context.Context, executorID, workspaceID string) {
	pool := c.execPool
	if pool == nil || workspaceID == "" {
		return
	}
	pp, ok := pool.(*execpool.Pool)
	if !ok {
		return
	}
	if err := pp.Release(ctx, executorID, workspaceID); err != nil {
		slog.Warn("release workspace failed",
			"workspaceId", workspaceID,
			"executorId", executorID[:12],
			"error", err,
		)
	}
}

// selectExecutorID is like selectExecutor but returns the executor ID alongside
// the client, so callers can provision workspaces. It routes through
// chooseExecutor so the provisioning path enforces the same narrowing as the
// tool path — a workspace provisioned outside the parent's set would be the
// confidentiality boundary leaking through the side door.
func (c *Controller) selectExecutorID(req protocol.SpawnRequest) (id string, cl tools.ExecutorClient, err error) {
	chosen, err := c.chooseExecutor(req)
	if err != nil {
		return "", nil, err
	}
	cl, err = c.execPool.ClientFor(chosen.ID)
	if err != nil {
		return "", nil, fmt.Errorf("executor %s selected but not reachable: %w", shortID(chosen.ID), err)
	}
	return chosen.ID, cl, nil
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

		// Check the workspace mode from the child's labels. The label is
		// daemon-written from the executor's row at provision time; an absent
		// one means a child from before the label existed, and PINNED is the
		// conservative reading — moving a child onto a machine no operator
		// marked interchangeable is worse than failing it where it stood.
		mode := workspaceModeOrPinned(snap.Labels["rafiki/workspace-mode"])

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

// executorAcceptsReschedule reports whether a child may be moved onto le.
//
// Reads the ROW (le.Executor), never le.Describe. Describe is the executor's
// own account of itself; a value that decides where other people's children run
// cannot be asserted by the machine that wants them.
func executorAcceptsReschedule(le execpool.LiveExecutor) bool {
	return le.Executor.WorkspaceMode == "ephemeral"
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
		if executorAcceptsReschedule(le) {
			wsID, _, _, err := c.provisionWorkspace(context.Background(),
				protocol.SpawnRequest{Cwd: snap.Cwd}, le.Executor.ID)
			if err != nil {
				slog.Warn("reschedule provision failed",
					"childId", snap.ChildID, "executorId", le.Executor.ID[:12], "error", err)
				continue
			}

			// Update the child's store labels with the new workspace.
			if _, err := c.st.SetLabels(snap.ChildID, map[string]string{
				"rafiki/workspace": wsID,
				"rafiki/executor":  le.Executor.ID,
			}, nil); err != nil {
				slog.Warn("reschedule label update failed",
					"childId", snap.ChildID, "error", err)
			}

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
		if _, err := c.Kill(context.Background(), childID, 5000, 1000); err != nil {
			slog.Warn("failChild force-kill failed", "childId", childID, "error", err)
		}
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
