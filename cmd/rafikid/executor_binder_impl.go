package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
)

// sandboxBindTimeout bounds the ownership lookup every bind runs. It is short:
// the query is one indexed read, and blocking a bind on a wedged store must not
// wedge the child's first tool call.
const sandboxBindTimeout = 5 * time.Second

// controllerBinder is the Controller's executorBinder, bound to ONE spawn
// request and its attested owner.
//
// Bound at construction rather than taking a childID per method for the same
// reason newControllerSpawner is: a value carrying its own subject cannot be
// asked to act for a different one, and selection here is a confinement
// decision.
type controllerBinder struct {
	c     *Controller
	req   protocol.SpawnRequest
	owner executorOwner
}

func (c *Controller) binderFor(req protocol.SpawnRequest, owner executorOwner) executorBinder {
	return &controllerBinder{c: c, req: req, owner: owner}
}

// homeExecutorLabel records, on the child's own row, the executor whose
// workspace the child's cwd and project tier belong to. It is durable precisely
// so a RESUMED child agrees with the daemon that migrated it: b.home is
// in-memory and dies with the process.
const homeExecutorLabel = "rafiki/home-executor"

// ChooseFor re-runs full selection. It is NOT a cached decision: the effective
// set is recomputed from the live pool every time, deliberately, so an
// executor that connects after the child started is usable by it.
//
// The error is explainNoMatch's per-candidate diagnostic, which boundExecutor
// surfaces to the agent verbatim.
func (b *controllerBinder) ChooseFor(childID string) (string, error) {
	// A spawn-block sandbox OWNS this child — its own block, or for a
	// subtree-scoped block one an ancestor holds. Ownership is checked first and
	// is authoritative: a sandboxed child binds to its sandbox and NEVER falls
	// through to ordinary selection, which would run it natively on a host the
	// operator never offered it (and, for a `subtree` descendant, let a whole
	// subtree escape the block).
	sandboxExec, err := b.ownedSandboxExecutor(childID)
	if err != nil {
		return "", err
	}
	if sandboxExec != nil {
		return sandboxExec.ID, nil
	}

	chosen, err := b.c.chooseExecutor(b.req, b.owner)
	if err != nil {
		return "", err
	}
	return chosen.ID, nil
}

// ownedSandboxExecutor returns the executor of the spawn-block sandbox that
// owns childID, or nil when the child owns no sandbox.
//
// A missing sandbox store (a DB-less daemon) skips the lookup entirely: there
// are no spawn-block sandboxes without one.
func (b *controllerBinder) ownedSandboxExecutor(childID string) (*executors.Executor, error) {
	if b.c.sandboxStore == nil {
		return nil, nil
	}
	// The ancestry hint for a fresh descendant's eager bind. On a spawn the
	// request carries the parent; on a rebind/recovery the request is rebuilt
	// from the stored snapshot (resumeRequestFromSnapshot), which does NOT set
	// ParentChildID, so fall back to the store's recorded parent — by then the
	// child's own row exists (load_children inserts every recovered row before
	// its runtime is built), so the lookup succeeds. An empty or missing hint
	// is fail-closed: it can only fail to find an owned sandbox.
	parent := b.req.ParentChildID
	if parent == "" && b.c.st != nil {
		parent, _ = b.c.st.ParentOf(childID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxBindTimeout)
	row, owned, err := b.c.ownedSandbox(ctx, childID, parent, b.owner.UserID)
	cancel()
	if err != nil {
		// A store error is an error, never "not owned": treating it as
		// unowned would let a sandboxed child silently rebind natively.
		return nil, err
	}
	if !owned {
		return nil, nil
	}
	// Only a `ready` row is a FULLY live sandbox. A `removing` row is being
	// torn down, a `lost` row has no container, and `creating` is an in-flight
	// create with no container yet — binding any of them would hand the child a
	// workspace that is going away. All take the same not-connected path, never
	// a fall-through to ordinary selection.
	if row.State == sandboxStateReady && b.c.execPool != nil {
		for _, le := range b.c.execPool.Live() {
			if le.Executor.ID == row.ExecutorID && le.Executor.Enabled && le.Executor.OwnerUserID == b.owner.UserID {
				e := le.Executor
				return &e, nil
			}
		}
	}
	// The row is not a fully live sandbox (or its executor is not live,
	// disabled, or was re-owned). This NEVER falls through to chooseExecutor:
	// falling through would let the child bind somewhere else instead of to its
	// sandbox.
	return nil, fmt.Errorf("sandbox %q is not connected", sandboxDisplayName(row))
}

// Reachable is every executor childID may call RIGHT NOW, freshly computed from
// the live pool — never a cached set. It is the same confinement-narrowed
// candidate list chooseExecutor picks a winner from (parent's effective set ∩
// this child's selector, child-owned sandboxes dropped, sorted), so "what I can
// reach" and "what I would bind to" can never disagree.
//
// A spawn-block sandbox is exclusive: it is the only reachable executor, and a
// sandbox that is not fully live is an ERROR rather than an empty set, so an
// implicit call never wanders onto an ordinary machine (or onto a "none is
// reachable, so I may bind anywhere" fall-through).
func (b *controllerBinder) Reachable(childID string) ([]executors.Executor, error) {
	sandboxExec, err := b.ownedSandboxExecutor(childID)
	if err != nil {
		return nil, err
	}
	if sandboxExec != nil {
		return []executors.Executor{*sandboxExec}, nil
	}

	candidates, _, _, _, err := b.c.narrowedExecutorCandidates(b.req, b.owner)
	if err != nil {
		return nil, err
	}
	if b.req.ExecutorRef != "" {
		// An explicit ref bypasses SEARCH, never confinement: the hit must come
		// out of the narrowed set.
		if hit, ok := matchExecutorRef(b.req.ExecutorRef, candidates); ok {
			return []executors.Executor{hit}, nil
		}
		return nil, nil
	}
	// As in chooseExecutor: a caller that named neither a ref nor a selector
	// has not asked for any particular machine, so a sandbox must never be an
	// implicit choice.
	if !hasExplicitSelector(b.req) {
		candidates = dropSandboxCandidates(candidates)
	}
	return candidates, nil
}

// Home reads the child's recorded home executor.
func (b *controllerBinder) Home(childID string) string {
	if b.c.st == nil {
		return ""
	}
	snap, ok := b.c.st.Get(childID)
	if !ok {
		return ""
	}
	return snap.Labels[homeExecutorLabel]
}

// SetHomeLabel writes the child's home executor onto its own row, so a resumed
// child agrees with the daemon that migrated it. Best-effort: a child that
// cannot record its home keeps working on the in-memory one.
func (b *controllerBinder) SetHomeLabel(childID, executorID string) {
	if b.c.st == nil {
		return
	}
	if _, err := b.c.st.SetLabels(childID, map[string]string{homeExecutorLabel: executorID}, nil); err != nil {
		slog.Warn("could not record the child's home executor; a resumed child will "+
			"bind by selection instead",
			"child", childID, "executor", shortID(executorID), "error", err)
	}
}

// DisplayName names an executor the way an operator and the model see it: its
// machine label, else a short id (a row without a machine label is an operator
// error the enrollment flow warns about).
func (b *controllerBinder) DisplayName(executorID string) string {
	if row, ok := b.c.executorRow(executorID); ok {
		if machine := row.Labels["machine"]; machine != "" {
			return machine
		}
	}
	return shortID(executorID)
}

// sandboxDisplayName names a sandbox row for an operator: its name when it has
// one (a named sandbox), else the generated machine name of its spawn block,
// which is what a caller can actually see and type.
func sandboxDisplayName(row sandbox.Row) string {
	if row.Name != "" {
		return row.Name
	}
	return sandboxSpawnMachineName(row.ID)
}

// ProvisionOn creates a workspace on executorID.
//
// useCwd decides whether the child's cwd applies there. It does only on the
// child's HOME executor: its cwd is a path in one machine's filesystem, and a
// workspace at another machine's root is what makes a call addressed to that
// machine land in ITS view of the world rather than in a directory that does
// not exist there (Provision would refuse it outright).
func (b *controllerBinder) ProvisionOn(ctx context.Context, executorID string, useCwd bool) (workspaceID, workdir string, cl tools.ExecutorClient, err error) {
	req := b.req
	if !useCwd {
		req.Cwd = ""
	}
	wsID, _, client, perr, wd := b.c.provisionWorkspace(ctx, req, executorID)
	if perr != nil {
		return "", "", nil, perr
	}
	return wsID, wd, client, nil
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
// It is the HOME binding's call and nobody else's: the three labels name ONE
// workspace, so a binding on another executor that wrote them would both lie
// about where the child's tree is and overwrite the workspace teardown has to
// release. boundExecutor's own clientFor enforces that.
//
// The childstore is the authority, not c.wsLabels: handleChildExit releases the
// workspace by snap.Labels, and HandleExecutorLost finds affected children by
// them. c.wsLabels is only a BRIDGE for the window before Spawn has inserted
// the record -- an eager bind happens inside spawnAgentRuntimeOptions, which runs
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

// ForgetRunningJob drops a job the agent's bash_kill resolved from the
// daemon's running-jobs set, so a later ReplayComplete never reports a
// self-killed job as gone. Nil-safe: a hand-built Controller simply has no
// running set to prune.
func (b *controllerBinder) ForgetRunningJob(executorID, handle string) {
	b.c.forgetRunningJob(executorID, handle)
}
