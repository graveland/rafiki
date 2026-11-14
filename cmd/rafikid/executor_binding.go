package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/toolmeta"
)

// executorBinder is everything boundExecutor needs from the Controller.
//
// An interface rather than a *Controller so the binding state machine is
// testable without a pool, a database, or a dialling executor — the reason the
// original selection path shipped untested.
type executorBinder interface {
	// ChooseFor returns the executor childID should bind to right now, and
	// errors with a text that explains why none qualifies. The error is
	// surfaced verbatim to the agent, so it must be the operator-legible
	// explainNoMatch diagnostic rather than a bare sentinel.
	ChooseFor(childID string) (executorID string, err error)

	// Reachable returns every executor childID may use RIGHT NOW, freshly
	// computed and ordered as sortCandidates orders. It is never cached: an
	// executor that connects after the child started is usable on the next
	// call, and an executor that is gone is not usable on this one.
	Reachable(childID string) ([]executors.Executor, error)

	// Home returns the executor id recorded in the child's
	// rafiki/home-executor label, or "" when the child has none. The durable
	// label is what makes a resumed child agree with the daemon that wrote it.
	Home(childID string) string

	// ProvisionOn creates a workspace on executorID and returns a
	// workspace-scoped client for it, plus the workdir the executor reports
	// (the child's cwd on the home executor, the executor's root otherwise).
	ProvisionOn(ctx context.Context, executorID string, useCwd bool) (workspaceID, workdir string, cl tools.ExecutorClient, err error)

	// ReleaseOn tears down a workspace. Best-effort and idempotent: it is
	// called against executors that may already be gone.
	ReleaseOn(ctx context.Context, executorID, workspaceID string)

	// IsLive reports whether executorID currently has a live, non-draining
	// connection in the pool.
	IsLive(executorID string) bool

	// NoteBinding records the child's current HOME executor and workspace so
	// the rest of the daemon (labels, prompt visibility, teardown) can see it.
	// Only the home binding is ever noted: the labels name one workspace, and
	// handleChildExit releases that one by label.
	NoteBinding(childID, executorID, workspaceID string)

	// SetHomeLabel records the child's home executor on its own row, so a
	// resumed child agrees with the daemon that migrated it.
	SetHomeLabel(childID, executorID string)

	// DisplayName names an executor for an operator and for the model: its
	// machine label, else a short id.
	DisplayName(executorID string) string

	// WorkspaceMode reports the workspace_mode of executorID, from the
	// executor's ROW -- never from Describe: the value decides whether losing
	// a machine fails a child or moves it, and a machine that wants children
	// cannot be the one asserting it is interchangeable.
	WorkspaceMode(executorID string) string

	// NotifyMigrated tells childID its workspace was rebuilt on a different
	// executor. Without this the child keeps believing its files are where it
	// left them and reports work as done that no longer exists.
	NotifyMigrated(childID, fromExec, toExec string)

	// ForgetRunningJob drops a job the agent's bash_kill just resolved from the
	// daemon's running-jobs set for executorID, so a later ReplayComplete never
	// reports a self-killed job as "gone" — gone means lost to an executor
	// restart, not "you killed it".
	ForgetRunningJob(executorID, handle string)
}

// boundExecutor is a child's handle on the executors it may use, as opposed to
// a handle on one TCP connection.
//
// The type it replaces was an *execpool.workspaceClient captured at spawn,
// whose HTTP/2 transport is glued to a single net.Conn by ClientForConn's
// handedOver latch. When that connection died the child kept calling the
// corpse forever, even after the same executor reconnected and the pool was
// perfectly healthy — the reported "transport requested a second connection"
// failure.
//
// Binding is STICKY per executor: a working executor is held, not re-selected
// per call. That keeps the happy path to one map lookup, stops a newly-
// connected executor stealing a child that is working fine, and leaves room for
// executor-side caching later. Which executor a CALL goes to is a separate,
// per-call decision (resolve); re-binding a binding happens only when a call
// fails for a LIVENESS reason — never when a tool merely reports failure.
//
// bindings is keyed by executor id, and ONE of them is the HOME binding
// (b.home): the one whose workspace the child's labels name, which owns the
// child's cwd and the project tier, and which a pinned child cannot leave. The
// others are ordinary workspaces at the executors' roots, created because the
// agent named that machine for a call. Only the home binding is ever written to
// the child's labels.
type boundExecutor struct {
	childID string
	binder  executorBinder

	mu       sync.Mutex
	bindings map[string]*binding // executor id → live binding
	home     string              // executor id of the binding that owns the labels; "" none

	// homeSeeded records that b.home has been read from the child's label
	// once. Without it a Forget of the home binding would be undone by the
	// label read on the very next call.
	homeSeeded bool
}

// binding is one live (executor, workspace) pair.
type binding struct {
	wsID string
	cl   tools.ExecutorClient

	// inflight is non-nil while a recover() is running its RPCs for THIS
	// binding. A second caller that fails against the same stale binding waits
	// on inflight.done instead of starting its own Provision -- this is what
	// coalesces N concurrent failures into ONE re-provision, without holding
	// b.mu across the RPCs that a rebind requires.
	inflight *recovery
}

// recovery is one in-flight recover() call, shared by every goroutine that
// fails against the same stale binding while it runs.
type recovery struct {
	done chan struct{}
	ok   bool
}

func newBoundExecutor(childID string, b executorBinder) *boundExecutor {
	return &boundExecutor{childID: childID, binder: b, bindings: make(map[string]*binding)}
}

// Current reports the child's HOME binding, for label writing and teardown.
func (b *boundExecutor) Current() (executorID, workspaceID string, bound bool) {
	home := b.homeExecutor()
	if home == "" {
		return "", "", false
	}
	b.mu.Lock()
	bd := b.bindings[home]
	b.mu.Unlock()
	if bd == nil {
		return home, "", false
	}
	return home, bd.wsID, true
}

// Bindings is a copy of every live executor id → workspace id, so teardown can
// release the workspaces the labels never name.
func (b *boundExecutor) Bindings() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]string, len(b.bindings))
	for id, bd := range b.bindings {
		out[id] = bd.wsID
	}
	return out
}

// Forget removes one binding WITHOUT releasing its workspace — the caller owns
// the release. Used when an executor's connection is known to be gone and the
// next call must not find a stale client. If the binding was home, the home
// pointer is cleared too (rewriting the labels is the caller's job).
func (b *boundExecutor) Forget(executorID string) (workspaceID string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bd, ok := b.bindings[executorID]
	if !ok {
		return "", false
	}
	delete(b.bindings, executorID)
	if b.home == executorID {
		b.home = ""
		// The label still names the forgotten executor until the caller
		// rewrites it. Re-seeding home from it would undo this Forget.
		b.homeSeeded = true
	}
	return bd.wsID, true
}

// Multi reports whether the child can reach 2+ executors right now, i.e.
// whether a tool call must name one. An error reads as single: the caller uses
// this only to decide whether to mention an executor in a notice.
func (b *boundExecutor) Multi() bool {
	reach, err := b.binder.Reachable(b.childID)
	if err != nil {
		return false
	}
	return len(reach) >= 2
}

// setHome records the executor whose binding owns the child's labels.
//
// The spawn path calls it before any call: at spawn the child's record does not
// exist yet, so rafiki/home-executor cannot be read back.
func (b *boundExecutor) setHome(execID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.home = execID
	b.homeSeeded = true
}

// homeExecutor returns the child's home executor id, seeding it once from the
// child's own rafiki/home-executor label. "" means the child has no home yet.
func (b *boundExecutor) homeExecutor() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.homeLocked()
}

// homeLocked is homeExecutor for a caller that already holds b.mu.
func (b *boundExecutor) homeLocked() string {
	if !b.homeSeeded {
		b.homeSeeded = true
		if b.home == "" {
			b.home = b.binder.Home(b.childID)
		}
	}
	return b.home
}

// stale returns the home binding's key, for tests that then simulate a failure
// against it.
func (b *boundExecutor) stale() (execID, wsID string) {
	home := b.homeExecutor()
	b.mu.Lock()
	defer b.mu.Unlock()
	if bd := b.bindings[home]; bd != nil {
		return home, bd.wsID
	}
	return home, ""
}

// resolve decides which executor a call goes to: the one it names, or the only
// one it can reach.
//
// It reads the caller's ExecutorTarget (nil for the daemon's own internal
// calls) and fills in Resolved/Multi on the way out, so the tool wrapper can
// tell the model which machine served the call.
//
// An empty ExecutorRef in the binding's spawn request means "any executor my
// grant admits", so an implicit call is ambiguous the moment the child can
// reach more than one: home is NOT a default for tool calls, home included.
func (b *boundExecutor) resolve(ctx context.Context) (execID string, multi bool, err error) {
	reach, err := b.binder.Reachable(b.childID)
	if err != nil {
		return "", false, err
	}
	multi = len(reach) >= 2
	t := tools.ExecutorTargetFrom(ctx)

	if t != nil && t.Requested != "" {
		if e, ok := matchExecutorRef(t.Requested, reach); ok {
			setTarget(t, b.binder.DisplayName(e.ID), multi)
			return e.ID, multi, nil
		}
		t.Multi = multi
		return "", multi, b.unreachableName(t.Requested, reach)
	}

	switch {
	case len(reach) == 0:
		// Nothing is reachable: ask selection for its per-candidate diagnostic,
		// which is the operator-legible answer the agent needs. A selection that
		// succeeds anyway is honoured.
		id, cerr := b.binder.ChooseFor(b.childID)
		if cerr != nil {
			return "", false, cerr
		}
		if id == "" {
			return "", false, errors.New("no executor is reachable and none could be selected")
		}
		// The only executor this child can use becomes home for the binding
		// being created: it is where the child's cwd and project tier live.
		if b.homeExecutor() == "" {
			b.setHome(id)
		}
		setTarget(t, b.binder.DisplayName(id), false)
		return id, false, nil

	case len(reach) == 1:
		e := reach[0]
		home := b.homeExecutor()
		switch {
		case home == "":
			b.setHome(e.ID)
		case home != e.ID:
			if !mayMigrate(b.binder.WorkspaceMode(home)) {
				return "", false, fmt.Errorf("executor %s is not connected; this agent's workspace is pinned there, so calls fail until it reconnects", b.binder.DisplayName(home))
			}
			// Ephemeral: the call migrates the child's home to the machine that
			// is actually there, rather than failing work the operator already
			// declared movable.
			if merr := b.migrateHome(ctx, home, e.ID); merr != nil {
				return "", false, merr
			}
		}
		setTarget(t, b.binder.DisplayName(e.ID), false)
		return e.ID, false, nil
	}

	if t != nil {
		t.Multi = true
	}
	return "", true, fmt.Errorf("this agent can reach %d executors (%s); pass executor: \"<name>\"", len(reach), reachNames(b.binder, reach))
}

// setTarget fills the tool wrapper's view of a successful resolution. Nil when
// the call did not come through the registry wrapper.
func setTarget(t *tools.ExecutorTarget, resolved string, multi bool) {
	if t == nil {
		return
	}
	t.Resolved = resolved
	t.Multi = multi
}

// reachNames is the sorted, comma-joined display names of a reachable set, or
// "none" for an empty one.
func reachNames(binder executorBinder, reach []executors.Executor) string {
	if len(reach) == 0 {
		return "none"
	}
	names := make([]string, 0, len(reach))
	for _, e := range reach {
		names = append(names, binder.DisplayName(e.ID))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// unreachableName explains a named call that matched nothing reachable.
//
// Two different failures read alike to the caller but mean opposite things: an
// executor this child is ALREADY bound to (or its home) that is not in the
// reachable set right now is a connection that dropped, and the call will work
// again when it returns; anything else is a name this grant cannot reach at
// all.
func (b *boundExecutor) unreachableName(requested string, reach []executors.Executor) error {
	if b.namesABoundExecutor(requested) {
		return fmt.Errorf("executor %s is not connected; calls to it fail until it reconnects", requested)
	}
	return fmt.Errorf("executor %q is not reachable by this agent; reachable: %s", requested, reachNames(b.binder, reach))
}

// namesABoundExecutor reports whether requested names an executor this child
// was bound to, or its home — i.e. a machine it knows that is not reachable now.
func (b *boundExecutor) namesABoundExecutor(requested string) bool {
	b.mu.Lock()
	ids := make([]string, 0, len(b.bindings)+1)
	for id := range b.bindings {
		ids = append(ids, id)
	}
	// homeLocked, not b.home: a child resumed from a row has its home ONLY in
	// the label until something seeds it, and that label is exactly what makes
	// "the machine I used to be on" a connectivity answer.
	if home := b.homeLocked(); home != "" {
		ids = append(ids, home)
	}
	b.mu.Unlock()
	for _, id := range ids {
		if id == requested || b.binder.DisplayName(id) == requested {
			return true
		}
	}
	return false
}

// implicitCall reports whether ctx's call named no executor. The daemon's own
// internal calls carry no target at all and count as implicit.
func implicitCall(ctx context.Context) bool {
	t := tools.ExecutorTargetFrom(ctx)
	return t == nil || t.Requested == ""
}

// clientFor returns the binding for execID, provisioning it on first use.
//
// Only the first bind of a GIVEN executor runs under b.mu, exactly as the
// single-binding version did: at that point there is no binding to hold other
// callers away from, so there is nothing to wedge. Every later call is a map
// lookup.
func (b *boundExecutor) clientFor(ctx context.Context, execID string) (*binding, error) {
	if execID == "" {
		return nil, errors.New("no executor to bind to")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bd := b.bindings[execID]; bd != nil {
		return bd, nil
	}

	useCwd := b.homeLocked() == execID
	wsID, workdir, cl, err := b.binder.ProvisionOn(ctx, execID, useCwd)
	if err != nil {
		return nil, fmt.Errorf("workspace on executor %s: %w", shortID(execID), err)
	}
	bd := &binding{wsID: wsID, cl: cl}
	b.bindings[execID] = bd
	if useCwd {
		// The labels name the HOME workspace; another executor's workspace is
		// none of the daemon's business (see handleChildExit).
		b.binder.NoteBinding(b.childID, execID, wsID)
	} else if t := tools.ExecutorTargetFrom(ctx); t != nil {
		// One-time note, on the first provision of a non-home binding: the
		// child's cwd does NOT apply here, and a model that assumes it does
		// writes into the wrong tree.
		t.Note = fmt.Sprintf("[%s] working from %s", b.binder.DisplayName(execID), workdir)
	}
	return bd, nil
}

// callTarget snapshots a binding's client and workspace BEFORE a call is
// dispatched, under the lock.
//
// Both matter as a SNAPSHOT. A concurrent recovery rebuilds a binding IN PLACE
// (same *binding, new wsID/cl), so reading either back after the call would
// attribute the failure to a workspace that was never called -- which turns one
// coalesced recovery into a fresh one per caller -- and reading them without
// the lock is a data race.
func (b *boundExecutor) callTarget(bd *binding) (tools.ExecutorClient, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bd.cl, bd.wsID
}

// bindImplicit resolves a call that names no executor and binds it, which is
// what the spawn path wants: the project tier and skills need a live workspace,
// and a child that CAN bind should start bound.
func (b *boundExecutor) bindImplicit(ctx context.Context) error {
	execID, _, err := b.resolve(ctx)
	if err != nil {
		return err
	}
	_, err = b.clientFor(ctx, execID)
	return err
}

// migrateHome moves the child's home binding to execID: it provisions a
// workspace there as the HOME workspace (so the child's cwd applies), rewrites
// the labels, and tells the child its tree moved.
//
// Concurrent implicit calls can all decide to migrate at once. Only the one
// that WINS the home transition (under b.mu) provisions as home and writes the
// labels; the rest find the binding clientFor just made and return quietly, so
// the child is told once and no second workspace is minted.
func (b *boundExecutor) migrateHome(ctx context.Context, from, to string) error {
	b.mu.Lock()
	won := b.homeLocked() == from
	b.home = to
	b.homeSeeded = true
	b.mu.Unlock()

	bd, err := b.clientFor(ctx, to)
	if err != nil {
		if won {
			b.setHome(from)
		}
		return err
	}
	if !won {
		return nil
	}
	b.binder.NoteBinding(b.childID, to, bd.wsID)
	b.binder.SetHomeLabel(b.childID, to)
	b.binder.NotifyMigrated(b.childID, from, to)
	slog.Info("migrated the child's home workspace",
		"child", b.childID, "from", shortID(from), "to", shortID(to))
	return nil
}

// retryBinding returns the binding a retry should run on after a recovery:
// the failed binding if it was rebuilt in place, else the home binding a
// migration installed. ok is false when nothing serves the call any more.
func (b *boundExecutor) retryBinding(execID string) (bd *binding, servedExec string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur := b.bindings[execID]; cur != nil {
		return cur, execID, true
	}
	if b.home != "" && b.home != execID {
		if cur := b.bindings[b.home]; cur != nil {
			return cur, b.home, true
		}
	}
	return nil, "", false
}

// recover reacts to a failed call. It returns true when the caller should
// retry once.
//
// execID/staleWS identify the binding the failed call ran against. If that
// binding has since been rebuilt, another goroutine already recovered and this
// caller simply retries on the new one — eight concurrent failures against the
// same binding produce one Provision, not eight, while a failure against a
// DIFFERENT binding is neither blocked nor re-provisioned.
//
// idempotent says whether the call that failed is safe to re-dispatch even
// when the stream broke mid-call and the tool may already have run on the
// executor -- see retryable and idempotentTools. A tool that ran and reported
// failure is a different case entirely: that is an ANSWER, and migrating a
// child because `bash` exited 1 would be the single worst behaviour this type
// could have.
//
// recover does its RPCs (IsLive/ProvisionOn/ReleaseOn) with b.mu RELEASED.
// Those calls target an executor that is by definition unwell, and every other
// caller on this child -- Current(), a concurrent Execute on another binding
// -- must not block behind them for the full RPC timeout.
func (b *boundExecutor) recover(ctx context.Context, execID, staleWS string, callErr error, idempotent bool) bool {
	if !retryable(callErr, idempotent) {
		return false
	}

	b.mu.Lock()
	bd := b.bindings[execID]
	if bd != nil && bd.wsID != staleWS {
		// Another goroutine already rebuilt this binding; retry on theirs.
		b.mu.Unlock()
		return true
	}
	if bd != nil && bd.inflight != nil {
		// A recovery for this exact binding is already running. Wait for its
		// result rather than starting a second Provision.
		r := bd.inflight
		b.mu.Unlock()
		<-r.done
		return r.ok
	}
	if bd == nil {
		// The binding is already gone: a concurrent recovery dropped it, and
		// either rebuilt it elsewhere or gave up. Retry only if something else
		// now serves this call.
		b.mu.Unlock()
		_, _, ok := b.retryBinding(execID)
		return ok
	}

	r := &recovery{done: make(chan struct{})}
	bd.inflight = r
	b.mu.Unlock()

	ok := b.doRecover(ctx, execID, staleWS)

	b.mu.Lock()
	bd.inflight = nil
	b.mu.Unlock()
	r.ok = ok
	close(r.done)

	return ok
}

// doRecover performs the actual re-provision or re-bind. Called with b.mu
// released; it takes the lock only to install its result, because every RPC
// here targets an executor that is by definition unwell and no caller on this
// child may block behind it for a full RPC timeout.
func (b *boundExecutor) doRecover(ctx context.Context, execID, staleWS string) bool {
	// An executor still in the live set means the WORKSPACE went, not the
	// machine: pkg/executor keeps its workspace registry in memory, so a
	// restart loses every id while the connection is fine. Re-provision in
	// place rather than re-running selection.
	if execID != "" && b.binder.IsLive(execID) {
		if b.reprovisionInPlace(ctx, execID, staleWS) {
			return true
		}
	}

	// The executor itself is gone. Whether the child may follow is the ROW's
	// call, not this code's: a pinned home fails where it stood, and a call
	// that NAMED an executor never migrates (the model asked for that machine).
	home := b.homeExecutor()
	if !implicitCall(ctx) || execID == "" || home != execID || !mayMigrate(b.binder.WorkspaceMode(execID)) {
		slog.Warn("child lost its executor and will not be migrated",
			"child", b.childID, "executor", shortID(execID),
			"implicit", implicitCall(ctx), "home", home == execID)
		b.dropBinding(ctx, execID, staleWS)
		return false
	}

	// Migration is resolution's job — it applies the home rule, provisions on
	// the machine that is there, rewrites the labels and tells the child.
	next, _, err := b.resolve(ctx)
	if err != nil || next == "" || next == execID {
		slog.Warn("could not re-bind the child to any executor",
			"child", b.childID, "error", err)
		b.dropBinding(ctx, execID, staleWS)
		return false
	}
	b.dropBinding(ctx, execID, staleWS)
	slog.Info("re-bound child to a new executor",
		"child", b.childID, "from", shortID(execID), "to", shortID(next))
	return true
}

// reprovisionInPlace rebuilds the workspace on an executor whose connection is
// still live. Not a migration: the machine is the same one.
func (b *boundExecutor) reprovisionInPlace(ctx context.Context, execID, staleWS string) bool {
	useCwd := b.homeExecutor() == execID
	wsID, _, cl, err := b.binder.ProvisionOn(ctx, execID, useCwd)
	if err != nil {
		slog.Warn("re-provision on the same executor failed; re-selecting",
			"child", b.childID, "executor", shortID(execID), "error", err)
		return false
	}
	b.mu.Lock()
	if bd := b.bindings[execID]; bd != nil {
		bd.wsID, bd.cl = wsID, cl
	} else {
		b.bindings[execID] = &binding{wsID: wsID, cl: cl}
	}
	b.mu.Unlock()
	if useCwd {
		b.binder.NoteBinding(b.childID, execID, wsID)
	}

	// The old workspace is still registered on a LIVE executor, so unlike the
	// migration path this release will actually land -- do it now rather than
	// stranding it and its retained job output until the executor exits.
	if staleWS != "" && staleWS != wsID {
		b.binder.ReleaseOn(ctx, execID, staleWS)
	}
	return true
}

// dropBinding forgets one binding and releases its workspace best-effort in a
// goroutine: the executor it lived on is usually unreachable, and the child
// must not wait on it.
func (b *boundExecutor) dropBinding(ctx context.Context, execID, wsID string) {
	b.mu.Lock()
	if bd := b.bindings[execID]; bd != nil && bd.wsID != wsID {
		// Something else rebuilt this binding while we were deciding.
		b.mu.Unlock()
		return
	}
	delete(b.bindings, execID)
	b.mu.Unlock()
	if wsID == "" {
		return
	}
	go b.binder.ReleaseOn(ctx, execID, wsID)
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
		errors.Is(err, execpool.ErrRedialed),
		errors.Is(err, execpool.ErrDialFailed):
		return true
	}
	return false
}

// callBound resolves the call's executor, runs fn against its binding, and
// recovers at most once. A free function rather than a method because Go
// methods cannot be generic.
//
// idempotent tells recover whether fn is safe to re-dispatch after a stream
// broke mid-call, when the tool MAY have already run on the executor -- see
// retryable and idempotentTools.
//
// Every call carries the child's id as the executor call's owner
// (execpool.WithOwner): the executor attributes a background job to the child
// that started it and echoes the owner on every job event. The wrapped ctx is
// passed TO fn — fn takes the ctx as a parameter for exactly this reason: a
// closure that closed over the CALLER's ctx instead would silently drop the
// owner, because the wrap only rebinds callBound's local copy. Both attempts
// (the first call and the recover retry) receive the wrapped ctx.
func callBound[T any](ctx context.Context, b *boundExecutor, idempotent bool, fn func(context.Context, tools.ExecutorClient) (T, error)) (T, error) {
	var zero T
	ctx = execpool.WithOwner(ctx, b.childID)
	execID, _, err := b.resolve(ctx)
	if err != nil {
		return zero, err
	}
	bd, err := b.clientFor(ctx, execID)
	if err != nil {
		return zero, err
	}
	cl, staleWS := b.callTarget(bd)
	out, err := fn(ctx, cl)
	if err == nil {
		return out, nil
	}
	if !b.recover(ctx, execID, staleWS, err, idempotent) {
		return zero, err
	}
	return retryCall(ctx, b, execID, err, fn)
}

// retryCall re-dispatches fn once against whatever binding now serves the
// call: the same executor if recover rebuilt it in place, else the home
// binding a migration installed. There is no second recover.
func retryCall[T any](ctx context.Context, b *boundExecutor, execID string, callErr error, fn func(context.Context, tools.ExecutorClient) (T, error)) (T, error) {
	var zero T
	bd, _, ok := b.retryBinding(execID)
	if !ok {
		return zero, callErr
	}
	return fn(ctx, bd.cl)
}

// callHome is callBound for the daemon's own fetches.
//
// They carry no tool target, because they are not tool calls: the project tier,
// the skill bodies and the skill files all describe the workspace the child's
// cwd lives in, which is the HOME binding and only ever the home binding.
func callHome[T any](ctx context.Context, b *boundExecutor, idempotent bool, fn func(context.Context, tools.ExecutorClient) (T, error)) (T, error) {
	var zero T
	home := b.homeExecutor()
	if home == "" {
		return zero, errors.New("this agent has no home executor")
	}
	ctx = execpool.WithOwner(ctx, b.childID)
	bd, err := b.clientFor(ctx, home)
	if err != nil {
		return zero, err
	}
	cl, staleWS := b.callTarget(bd)
	out, err := fn(ctx, cl)
	if err == nil {
		return out, nil
	}
	if !b.recover(ctx, home, staleWS, err, idempotent) {
		return zero, err
	}
	return retryCall(ctx, b, home, err, fn)
}

func (b *boundExecutor) Execute(ctx context.Context, tool string, input json.RawMessage) (toolmeta.Result, error) {
	return callBound(ctx, b, isIdempotentTool(tool), func(ctx context.Context, cl tools.ExecutorClient) (toolmeta.Result, error) {
		return cl.Execute(ctx, tool, input)
	})
}

// StartJob is never retried after a stream break: retrying it launches the
// job twice. The exit notification is no longer armed here — the executor
// pushes job lifecycle events over WatchJobs and the daemon's handler
// (Controller.handleJobEvent) routes them by owner, so a successful start
// needs no bookkeeping on this side.
func (b *boundExecutor) StartJob(ctx context.Context, command string) (string, error) {
	return callBound(ctx, b, false, func(ctx context.Context, cl tools.ExecutorClient) (string, error) {
		return cl.StartJob(ctx, command)
	})
}

func (b *boundExecutor) JobOutput(ctx context.Context, handle string, since int64) (tools.JobSnapshot, error) {
	return callBound(ctx, b, true, func(ctx context.Context, cl tools.ExecutorClient) (tools.JobSnapshot, error) {
		return cl.JobOutput(ctx, handle, since)
	})
}

// KillJob is never retried after a stream break: the kill signal may already
// have landed, and there is no way to tell that apart from the stream just
// dying. A successful kill also drops the job from the daemon's running set for
// THE EXECUTOR THAT SERVED IT: the agent resolved the job on purpose, so a later
// ReplayComplete must not report it "gone" — gone means an executor restart lost
// it.
func (b *boundExecutor) KillJob(ctx context.Context, handle string) error {
	ctx = execpool.WithOwner(ctx, b.childID)
	served, _, err := b.resolve(ctx)
	if err != nil {
		return err
	}
	bd, err := b.clientFor(ctx, served)
	if err != nil {
		return err
	}
	cl, staleWS := b.callTarget(bd)
	kerr := cl.KillJob(ctx, handle)
	if kerr != nil {
		if !b.recover(ctx, served, staleWS, kerr, false) {
			return kerr
		}
		rebuilt, rebuiltOn, ok := b.retryBinding(served)
		if !ok {
			return kerr
		}
		served = rebuiltOn
		kerr = rebuilt.cl.KillJob(ctx, handle)
	}
	if kerr == nil {
		b.binder.ForgetRunningJob(served, handle)
	}
	return kerr
}

func (b *boundExecutor) Ping(ctx context.Context) error {
	_, err := callBound(ctx, b, true, func(ctx context.Context, cl tools.ExecutorClient) (struct{}, error) {
		return struct{}{}, cl.Ping(ctx)
	})
	return err
}

// ProjectContext, ProjectSkills and SkillBody satisfy the daemon's two
// optional fetcher interfaces (projectContextFetcher, projectSkillsFetcher in
// agent_runtime.go) by delegating to the HOME binding. A bound client that
// cannot answer yields the zero value and no error, matching
// fetchProjectContext's existing contract.
func (b *boundExecutor) ProjectContext(ctx context.Context) (string, error) {
	return callHome(ctx, b, true, func(ctx context.Context, cl tools.ExecutorClient) (string, error) {
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
	return callHome(ctx, b, true, func(ctx context.Context, cl tools.ExecutorClient) ([]skills.SkillMeta, error) {
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
	p, err := callHome(ctx, b, true, func(ctx context.Context, cl tools.ExecutorClient) (pair, error) {
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

// MaterializeSkill writes one stored skill's supporting files onto the HOME
// executor and returns the directory holding them. A client that cannot answer
// is an ERROR rather than an empty directory: an empty dir means "this skill
// has no files", and silently reporting that for a skill that does would leave
// the model reading a body that points at files it cannot open.
func (b *boundExecutor) MaterializeSkill(ctx context.Context, treeHash string, files []skills.File) (string, error) {
	return callHome(ctx, b, true, func(ctx context.Context, cl tools.ExecutorClient) (string, error) {
		mf, ok := cl.(interface {
			MaterializeSkill(context.Context, string, []skills.File) (string, error)
		})
		if !ok {
			return "", errors.New("executor cannot materialize skill files")
		}
		return mf.MaterializeSkill(ctx, treeHash, files)
	})
}

// ReleaseOthers releases every binding except the (executor, workspace) pair
// the child's labels name, which the label-driven release already handled.
//
// Called on exit: a child with bindings on two machines has workspaces the
// labels never name, and a release that stops at the labels strands them.
func (b *boundExecutor) ReleaseOthers(ctx context.Context, keepExec, keepWS string) {
	type pair struct{ execID, wsID string }
	b.mu.Lock()
	extra := make([]pair, 0, len(b.bindings))
	for execID, bd := range b.bindings {
		if execID == keepExec && bd.wsID == keepWS {
			continue
		}
		extra = append(extra, pair{execID, bd.wsID})
	}
	b.mu.Unlock()
	for _, p := range extra {
		b.binder.ReleaseOn(ctx, p.execID, p.wsID)
	}
}
