package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/skills"
)

// executorBinder is everything boundExecutor needs from the Controller.
//
// An interface rather than a *Controller so the binding state machine is
// testable without a pool, a database, or a dialling executor — the reason the
// original selection path shipped untested.
type executorBinder interface {
	// ChooseFor returns the executor childID should bind to right now,
	// or an error whose text explains why none qualifies. The error is
	// surfaced verbatim to the agent, so it must be the operator-legible
	// explainNoMatch diagnostic rather than a bare sentinel.
	ChooseFor(childID string) (executorID string, err error)

	// ProvisionOn creates a workspace on executorID and returns a
	// workspace-scoped client for it.
	ProvisionOn(ctx context.Context, executorID string) (workspaceID string, cl tools.ExecutorClient, err error)

	// ReleaseOn tears down a workspace. Best-effort and idempotent: it is
	// called against executors that may already be gone.
	ReleaseOn(ctx context.Context, executorID, workspaceID string)

	// IsLive reports whether executorID currently has a live, non-draining
	// connection in the pool.
	IsLive(executorID string) bool

	// NoteBinding records the child's current executor and workspace so the
	// rest of the daemon (labels, prompt visibility, teardown) can see it.
	NoteBinding(childID, executorID, workspaceID string)

	// WorkspaceMode reports the workspace_mode of executorID, from the
	// executor's ROW -- never from Describe: the value decides whether losing
	// a machine fails a child or moves it, and a machine that wants children
	// cannot be the one asserting it is interchangeable.
	WorkspaceMode(executorID string) string

	// NotifyMigrated tells childID its workspace was rebuilt on a different
	// executor. Without this the child keeps believing its files are where it
	// left them and reports work as done that no longer exists.
	NotifyMigrated(childID, fromExec, toExec string)
}

// boundExecutor is a child's handle on "an executor", as opposed to a handle
// on one TCP connection.
//
// The type it replaces was an *execpool.workspaceClient captured at spawn,
// whose HTTP/2 transport is glued to a single net.Conn by ClientForConn's
// handedOver latch. When that connection died the child kept calling the
// corpse forever, even after the same executor reconnected and the pool was
// perfectly healthy — the reported "transport requested a second connection"
// failure.
//
// Binding is STICKY: a working executor is held, not re-selected per call. That
// keeps the happy path to one map lookup, stops a newly-connected executor
// stealing a child that is working fine, and leaves room for executor-side
// caching later. Re-binding happens only when a call fails for a LIVENESS
// reason — never when a tool merely reports failure.
type boundExecutor struct {
	childID string
	binder  executorBinder

	mu     sync.Mutex
	execID string
	wsID   string
	cl     tools.ExecutorClient
}

func newBoundExecutor(childID string, b executorBinder) *boundExecutor {
	return &boundExecutor{childID: childID, binder: b}
}

// Current reports the child's binding, for label writing and teardown.
func (b *boundExecutor) Current() (executorID, workspaceID string, bound bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.execID, b.wsID, b.cl != nil
}

// stale returns the current binding key, for tests that then simulate a failure
// against it.
func (b *boundExecutor) stale() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.execID + "/" + b.wsID
}

// bindLocked selects and provisions. Caller holds b.mu.
func (b *boundExecutor) bindLocked(ctx context.Context) error {
	id, err := b.binder.ChooseFor(b.childID)
	if err != nil {
		return err
	}
	wsID, cl, err := b.binder.ProvisionOn(ctx, id)
	if err != nil {
		return fmt.Errorf("workspace on executor %s: %w", shortID(id), err)
	}
	b.execID, b.wsID, b.cl = id, wsID, cl
	b.binder.NoteBinding(b.childID, id, wsID)
	return nil
}

// clientFor returns the current client, binding first if necessary. The
// generation counter lets a caller detect that someone else re-bound while it
// was making its call, which is what coalesces concurrent failures into one
// rebind.
func (b *boundExecutor) clientFor(ctx context.Context) (tools.ExecutorClient, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cl == nil {
		if err := b.bindLocked(ctx); err != nil {
			return nil, "", err
		}
	}
	return b.cl, b.execID + "/" + b.wsID, nil
}

// recover reacts to a failed call. It returns true when the caller should
// retry once.
//
// stale is the execID/wsID the failed call ran against. If the current binding
// no longer matches it, another goroutine already recovered and this caller
// simply retries against the new binding — eight concurrent failures produce
// one Provision, not eight.
//
// idempotent says whether the call that failed is safe to re-dispatch even
// when the stream broke mid-call and the tool may already have run on the
// executor -- see retryable and idempotentTools. A tool that ran and reported
// failure is a different case entirely: that is an ANSWER, and migrating a
// child because `bash` exited 1 would be the single worst behaviour this type
// could have. retryable rejects both cases outright.
func (b *boundExecutor) recover(ctx context.Context, stale string, callErr error, idempotent bool) bool {
	if !retryable(callErr, idempotent) {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cl != nil && b.execID+"/"+b.wsID != stale {
		return true // someone else already recovered; retry on theirs
	}

	prevExec, prevWS := b.execID, b.wsID

	// An executor still in the live set means the WORKSPACE went, not the
	// machine: pkg/executor keeps its workspace registry in memory, so a
	// restart loses every id while the connection is fine. Re-provision in
	// place rather than re-running selection.
	if prevExec != "" && b.binder.IsLive(prevExec) {
		wsID, cl, err := b.binder.ProvisionOn(ctx, prevExec)
		if err != nil {
			slog.Warn("re-provision on the same executor failed; re-selecting",
				"child", b.childID, "executor", shortID(prevExec), "error", err)
		} else {
			b.wsID, b.cl = wsID, cl
			b.binder.NoteBinding(b.childID, prevExec, wsID)
			return true
		}
	}

	// The executor itself is gone. Whether the child may follow is the ROW's
	// call, not this code's: pinned fails where it stood.
	if prevExec != "" && !mayMigrate(b.binder.WorkspaceMode(prevExec)) {
		slog.Warn("pinned child lost its executor and will not be migrated",
			"child", b.childID, "executor", shortID(prevExec))
		return false
	}

	b.cl, b.execID, b.wsID = nil, "", ""
	if err := b.bindLocked(ctx); err != nil {
		slog.Warn("could not re-bind the child to any executor",
			"child", b.childID, "error", err)
		return false
	}
	slog.Info("re-bound child to a new executor",
		"child", b.childID, "from", shortID(prevExec), "to", shortID(b.execID))

	// A child whose workspace was rebuilt elsewhere must be TOLD. Otherwise it
	// keeps believing its files are there and reports work as done that no
	// longer exists.
	if prevExec != "" && prevExec != b.execID {
		b.binder.NotifyMigrated(b.childID, prevExec, b.execID)
	}

	// Best-effort, and deliberately after the new binding is installed: the
	// old executor is usually unreachable, and the child must not wait on it.
	if prevExec != "" && prevWS != "" {
		go b.binder.ReleaseOn(ctx, prevExec, prevWS)
	}
	return true
}

// isLivenessFailure reports whether err means "this executor cannot serve
// calls", as opposed to "this call did not work".
//
// These sentinels were produced by pkg/execpool from the start and consumed by
// nobody — departure.go's own comment claims callers check them with errors.Is
// after a failed tool call, and until now no caller did.
func isLivenessFailure(err error) bool {
	switch {
	case errors.Is(err, execpool.ErrExecutorGone),
		errors.Is(err, execpool.ErrExecutorLost),
		errors.Is(err, execpool.ErrParked),
		errors.Is(err, execpool.ErrDraining),
		errors.Is(err, execpool.ErrRedialed):
		return true
	}
	return false
}

// callBound runs fn against the current binding, recovering and retrying at
// most once. A free function rather than a method because Go methods cannot
// be generic.
//
// idempotent tells recover whether fn is safe to re-dispatch after a stream
// broke mid-call, when the tool MAY have already run on the executor -- see
// retryable and idempotentTools.
func callBound[T any](ctx context.Context, b *boundExecutor, idempotent bool, fn func(tools.ExecutorClient) (T, error)) (T, error) {
	var zero T
	cl, stale, err := b.clientFor(ctx)
	if err != nil {
		return zero, err
	}
	out, err := fn(cl)
	if err == nil {
		return out, nil
	}
	if !b.recover(ctx, stale, err, idempotent) {
		return zero, err
	}
	cl2, _, err2 := b.clientFor(ctx)
	if err2 != nil {
		return zero, err
	}
	return fn(cl2)
}

func (b *boundExecutor) Execute(ctx context.Context, tool string, input json.RawMessage) (string, error) {
	return callBound(ctx, b, isIdempotentTool(tool), func(cl tools.ExecutorClient) (string, error) {
		return cl.Execute(ctx, tool, input)
	})
}

// StartJob is never retried after a stream break: retrying it launches the
// job twice.
func (b *boundExecutor) StartJob(ctx context.Context, command string) (string, error) {
	return callBound(ctx, b, false, func(cl tools.ExecutorClient) (string, error) {
		return cl.StartJob(ctx, command)
	})
}

func (b *boundExecutor) JobOutput(ctx context.Context, handle string, since int64) (tools.JobSnapshot, error) {
	return callBound(ctx, b, true, func(cl tools.ExecutorClient) (tools.JobSnapshot, error) {
		return cl.JobOutput(ctx, handle, since)
	})
}

// KillJob is never retried after a stream break: the kill signal may already
// have landed, and there is no way to tell that apart from the stream just
// dying.
func (b *boundExecutor) KillJob(ctx context.Context, handle string) error {
	_, err := callBound(ctx, b, false, func(cl tools.ExecutorClient) (struct{}, error) {
		return struct{}{}, cl.KillJob(ctx, handle)
	})
	return err
}

func (b *boundExecutor) Ping(ctx context.Context) error {
	_, err := callBound(ctx, b, true, func(cl tools.ExecutorClient) (struct{}, error) {
		return struct{}{}, cl.Ping(ctx)
	})
	return err
}

// ProjectContext, ProjectSkills and SkillBody satisfy the daemon's two
// optional fetcher interfaces (projectContextFetcher, projectSkillsFetcher in
// agent_runtime.go) by delegating to whatever client is bound. A bound client
// that cannot answer yields the zero value and no error, matching
// fetchProjectContext's existing contract.
func (b *boundExecutor) ProjectContext(ctx context.Context) (string, error) {
	return callBound(ctx, b, true, func(cl tools.ExecutorClient) (string, error) {
		pf, ok := cl.(interface {
			ProjectContext(context.Context) (string, error)
		})
		if !ok {
			return "", nil
		}
		return pf.ProjectContext(ctx)
	})
}

func (b *boundExecutor) ProjectSkills(ctx context.Context) ([]skills.SkillMeta, error) {
	return callBound(ctx, b, true, func(cl tools.ExecutorClient) ([]skills.SkillMeta, error) {
		pf, ok := cl.(interface {
			ProjectSkills(context.Context) ([]skills.SkillMeta, error)
		})
		if !ok {
			return nil, nil
		}
		return pf.ProjectSkills(ctx)
	})
}

func (b *boundExecutor) SkillBody(ctx context.Context, name string) (string, string, error) {
	type pair struct{ body, dir string }
	p, err := callBound(ctx, b, true, func(cl tools.ExecutorClient) (pair, error) {
		pf, ok := cl.(interface {
			SkillBody(context.Context, string) (string, string, error)
		})
		if !ok {
			return pair{}, nil
		}
		body, dir, err := pf.SkillBody(ctx, name)
		return pair{body, dir}, err
	})
	return p.body, p.dir, err
}
