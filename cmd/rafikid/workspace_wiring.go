package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/inject"
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
//
// workdir is the directory the executor resolved for the workspace — the child's
// cwd where it applies, else the executor's own root. It is what the caller can
// honestly tell the model a workspace is "working from"; empty when no workspace
// was made.
func (c *Controller) provisionWorkspace(
	ctx context.Context,
	req protocol.SpawnRequest, executorID string,
) (workspaceID string, wi *fundi.WorkspaceInfo, exec tools.ExecutorClient, workdir string, err error) {
	if c.execPool == nil {
		return "", nil, nil, "", nil
	}

	// ChildId only, plus the one path-shaped field: Workdir.
	//
	// Mounts and network stay unset — the executor serves the filesystem it can
	// see, and for a container executor that view was composed in `docker run`
	// by the operator, exactly as they chose the image. rafiki's grant is the
	// label selector plus the row; nothing here composes a mount.
	//
	// Workdir is the child's cwd, expressed in the EXECUTOR's own filesystem
	// vocabulary. It is what makes cwd mean anything for an executor-bound
	// child: without it every tool call starts in the executor's root while the
	// child's prompt names another directory — the split-brain where a
	// coordinator pointed a worker at a git worktree and got an agent whose
	// bash verified and committed in the main checkout. The executor validates
	// the path exists in ITS view and refuses the provision otherwise, which is
	// the proto's MUST ("rather than silently starting somewhere the child
	// cannot write") and the honest answer for a cwd that lives on another
	// machine or outside the container's mounts.
	//
	// When the executor IS a sandbox, the workdir is the sandbox's own resolved
	// workdir (its container root when the spec names none), NOT req.Cwd: an
	// inherited host path does not exist inside the container, so Provision
	// would refuse every tool call. Spawn rewrites req.Cwd for a spawn block,
	// but a NAMED sandbox (`executor: "<name>"`) is created out of band and
	// Spawn never sees its spec, so this is the one place both cases resolve.
	dir := req.Cwd
	if wd, ok := c.sandboxWorkdirForExecutor(ctx, executorID); ok {
		dir = wd
	}
	resp, err := c.execPool.Provision(ctx, executorID, &executorpb.ProvisionRequest{
		ChildId: "",
		Workdir: dir,
	})
	if err != nil {
		return "", nil, nil, "", fmt.Errorf("provision on executor %s: %w", shortID(executorID), err)
	}

	// Get a workspace-scoped client.
	exec, err = c.execPool.ClientForWorkspace(executorID, resp.WorkspaceId)
	if err != nil {
		_ = c.execPool.Release(ctx, executorID, resp.WorkspaceId)
		return "", nil, nil, "", fmt.Errorf("workspace client: %w", err)
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
		"workdir", resp.Workdir,
		"roots", row.Roots,
		"isolation", row.Isolation,
		"workspaceMode", workspaceModeOrPinned(row.WorkspaceMode),
	)

	return resp.WorkspaceId, workspaceInfoFromRow(row), exec, resp.Workdir, nil
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
// Pinned is the conservative reading in both places this is used. In the system
// prompt it promises the child less (its tree stays put); on the
// rafiki/workspace-mode label it means the child is only ever re-provisioned on
// THAT machine, never moved onto one the operator never marked as
// interchangeable. Losing an executor fails nothing either way: an ephemeral
// child re-binds on its next call, and a pinned one waits for its machine to
// come back.
func workspaceModeOrPinned(mode string) string {
	if mode == "" {
		return "pinned"
	}
	return mode
}

// takeWorkspaceLabels consumes a child's pre-spawn binding stash.
//
// Locked, and it deletes: NoteBinding writes this map from tool-call goroutines
// now that binding is lazy, so the bare read-and-delete Spawn used to do is a
// concurrent map access -- a fatal error, not a data race the runtime survives.
func (c *Controller) takeWorkspaceLabels(childID string) (workspaceLabels, bool) {
	c.wsLabelsMu.Lock()
	defer c.wsLabelsMu.Unlock()
	wl, ok := c.wsLabels[childID]
	if ok {
		delete(c.wsLabels, childID)
	}
	return wl, ok
}

// markUnbound records that a child started without an executor, so rafiki
// list/get can show why its tools are failing. Written through the same
// store-then-stash path NoteBinding uses, because the child record may not
// exist yet when the eager bind fails inside agentRuntimeOptionsFor.
func (c *Controller) markUnbound(childID string) {
	_, err := c.st.SetLabels(childID, map[string]string{
		"rafiki/executor-state": "unbound",
	}, nil)
	if err == nil {
		return
	}
	if !errors.Is(err, childstore.ErrNotFound) {
		slog.Warn("could not mark child as unbound",
			"child", childID, "error", err)
		return
	}
	// The child record does not exist yet. Stash for Spawn.
	c.wsLabelsMu.Lock()
	if c.wsLabels == nil {
		c.wsLabels = make(map[string]workspaceLabels)
	}
	wl := c.wsLabels[childID]
	wl.executorState = "unbound"
	c.wsLabels[childID] = wl
	c.wsLabelsMu.Unlock()
}

// chooseExecutorCandidate returns the first executor whose ROW matches the
// child's selector, from the live pool. It is a best-effort fallback for
// the workspace block when the child is not yet bound: the system prompt is
// fixed for its lifetime, so omitting the block now means never. A nil
// block (no candidate found) is acceptable — naming the wrong machine is
// worse than none.
func (c *Controller) chooseExecutorCandidate(req protocol.SpawnRequest, owner executorOwner) (executors.Executor, error) {
	return c.chooseExecutor(req, owner)
}

// releaseWorkspace tears down a workspace on an executor.
// Idempotent and best-effort — it must not wedge teardown.
func (c *Controller) releaseWorkspace(ctx context.Context, executorID, workspaceID string) {
	if c.execPool == nil || workspaceID == "" {
		return
	}
	if err := c.execPool.Release(ctx, executorID, workspaceID); err != nil {
		// shortID, never executorID[:12]: an executor id shorter than twelve
		// characters is legal (a test id, a hand-registered row), and slicing it
		// would panic on the one path that must stay best-effort.
		slog.Warn("release workspace failed",
			"workspaceId", workspaceID,
			"executorId", shortID(executorID),
			"error", err,
		)
	}
}

// holdsBindingOn reports whether be still holds a workspace binding on
// executorID. Nil-safe: a child with no retained binding holds nothing.
func holdsBindingOn(be *boundExecutor, executorID string) bool {
	if be == nil {
		return false
	}
	_, ok := be.Bindings()[executorID]
	return ok
}

// HandleExecutorLost is called by the exec pool when a parked executor's
// timeout expires and it is declared permanently lost.
//
// Losing an executor NEVER fails a child, whatever its workspace mode. The
// child keeps running, its bindings on the lost machine are forgotten so the
// next call re-binds instead of calling a corpse, and it is steered to re-check
// what it believes is on that machine. Workspace mode is only a REBINDING rule
// from here on — pinned re-provisions on that same executor once it returns,
// ephemeral may re-provision anywhere on the child's next implicit call — so
// nothing here re-selects a machine and nothing here kills.
func (c *Controller) HandleExecutorLost(lostID string) {
	name := lostExecutorName(c, lostID)
	slog.Warn("executor lost — handling children", "executorId", shortID(lostID), "executor", name)

	// Children handled by the LABEL pass, so the binding pass below does not
	// steer one of them twice.
	handled := make(map[string]bool)

	// 1. The children whose HOME binding was on the lost machine. Their labels
	// name its workspace, which is the only workspace the daemon can release
	// without asking the child's binding.
	//
	// EXITED children are skipped: their exit sweep already released the
	// workspace by these same labels, and a steer pushed now would park a row in
	// an inbox nothing reads until a resume — waking that child for news about a
	// machine it is not using.
	for _, snap := range c.st.List() {
		if snap.Labels["rafiki/executor"] != lostID || snap.Status == protocol.StatusExited {
			continue
		}

		be := c.boundExecutorFor(snap.ChildID)
		labelWS := snap.Labels["rafiki/workspace"]

		// Already unbound with nothing left on the lost machine: this is a
		// REPEAT loss notice for the same executor id — the pool can declare an
		// id lost again after it reconnects, parks and expires a second time —
		// and this child was told the first time. Releasing nothing, rewriting
		// the labels it already carries and steering it again would spend a turn
		// on news it already has. A child that re-bound on the lost machine is
		// NOT in this state: NoteBinding clears rafiki/executor-state and
		// restores rafiki/workspace, so it is handled below like any other.
		//
		// "Nothing left" is BOTH things this handler owns: a binding, and a
		// rafiki/workspace label. Testing only the binding leaked a workspace —
		// a row that is unbound but still labels a workspace (a first loss whose
		// label write failed, a recovered row that kept the label) was skipped,
		// so that workspace was never released and the stale label outlived the
		// machine, leaving rafiki list naming a workspace that no longer exists.
		// The label's workspace is the LOST machine's: NoteBinding writes
		// rafiki/workspace and rafiki/executor together, and pass 1 has already
		// matched rafiki/executor to lostID — a child whose tree lives on another
		// machine carries that machine's id in rafiki/executor, so it never
		// reaches this branch (its workspace is either its home, released by its
		// own executor's loss, or a named-call binding, which pass 2 owns).
		if snap.Labels["rafiki/executor-state"] == "unbound" &&
			!holdsBindingOn(be, lostID) && labelWS == "" {
			continue
		}
		handled[snap.ChildID] = true

		// Both ids are released when they differ, deduped so the same workspace
		// is asked for once. Normally NoteBinding keeps them equal; they diverge
		// when the labels and the binding were last written by different paths
		// (a stash carried over the spawn, a failed label write), and then the
		// binding's workspace is the one nothing else would ever release.
		if labelWS != "" {
			c.releaseWorkspace(context.Background(), lostID, labelWS)
		}
		if be != nil {
			if wsID, ok := be.Forget(lostID); ok && wsID != labelWS {
				c.releaseWorkspace(context.Background(), lostID, wsID)
			}
		}

		// The binding is gone with the machine; the labels must stop claiming
		// one. rafiki/executor and rafiki/home-executor STAY, and so does
		// rafiki/workspace-mode: they name the machine the child's cwd lives on,
		// which is where a pinned child is re-provisioned when it comes back, and
		// dropping the mode would silently turn a pinned child ephemeral.
		//
		// WITHOUT a home of its own any more, though. A home that moved to another
		// machine (a migration that ran before this loss took the slot) owns the
		// labels now, and clearing rafiki/workspace would delete a LIVE workspace's
		// only label while rafiki/executor named neither that machine nor the home
		// a resume binds. Those labels belong to the migration's home and are left
		// alone; everything this handler owns — the releases above, the binding's
		// removal, the steer below — still happens.
		//
		// RESIDUAL (A1, partially-enforced invariant): this read-and-rewrite runs
		// AFTER Forget released the home transition slot, so a lazy adoption can
		// claim a home in the window between them. The write below then removes
		// rafiki/workspace and sets executor-state=unbound while that adoption's
		// binding is live. The home labels survive, so a restart resolves onto that
		// executor and provisions a fresh home there; the workspace the live binding
		// held is orphaned on it until that executor restarts. This is recorded, not
		// claimed impossible — do not reword it back into a guarantee.
		home := ""
		if be != nil {
			home = be.homeExecutor()
		}
		if home != "" && home != lostID {
			slog.Info("the child's home moved while this loss was being handled; leaving its "+
				"labels to the migration that established it",
				"childId", snap.ChildID, "lostExecutorId", shortID(lostID), "home", shortID(home))
		} else if _, err := c.st.SetLabels(snap.ChildID, map[string]string{
			"rafiki/executor-state": "unbound",
		}, []string{"rafiki/workspace"}); err != nil {
			slog.Warn("could not mark the child unbound after losing its executor",
				"childId", snap.ChildID, "executorId", shortID(lostID), "error", err)
		}

		c.sendSteer(snap.ChildID, executorLostSteer(name))
	}

	// 2. Every other child holding a binding on the lost machine: a workspace
	// the agent named for a call, which the labels never mention. Best-effort
	// release and forget, so the next call to that machine re-provisions rather
	// than reusing a client whose transport is dead.
	c.boundMu.Lock()
	bound := make(map[string]*boundExecutor, len(c.bound))
	for childID, be := range c.bound {
		bound[childID] = be
	}
	c.boundMu.Unlock()

	for childID, be := range bound {
		if handled[childID] {
			continue
		}
		if _, ok := be.Bindings()[lostID]; !ok {
			continue
		}
		if wsID, ok := be.Forget(lostID); ok {
			c.releaseWorkspace(context.Background(), lostID, wsID)
		}
		c.sendSteer(childID, executorLostSteer(name))
	}
}

// rescheduleSteer is injected into a child whose workspace was rebuilt on a
// different executor, so it re-checks the working tree before continuing
// rather than reporting as done work that no longer exists there.
const rescheduleSteer = `YOUR WORKSPACE WAS REBUILT on a different machine. The previous one is gone.
Anything you had NOT committed is lost. Re-check the state of the working tree
before continuing, and do not report as done anything you cannot now see.`

// sendSteer injects a steer message into the child's event stream.
func (c *Controller) sendSteer(childID, message string) {
	if c.evbuf != nil {
		c.evbuf.PushSteer(childID, string(inject.KindExecutor), message)
	}
}
