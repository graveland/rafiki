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
	"time"

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

	// DeclaredMulti reports whether the child's stored executor set names more
	// than one executor: `all`, or two or more ids. It reads the DECLARED set,
	// never the live reach, so a member that disconnects or is disabled never
	// turns an ambiguous child into one with an implicit target.
	DeclaredMulti(childID string) bool

	// CanBeHome reports whether executorID may become a child's home. An
	// executor installed over ssh may not: its lifecycle belongs to the
	// installer, and a relaunch replaces every workspace on that machine.
	CanBeHome(executorID string) bool

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

	// EnsureConnected makes executorID usable before a binding is provisioned on
	// it: live → nil, an executor installed over ssh that is down → relaunch it
	// and wait for the connection, anything else → nil. A successful relaunch
	// records the one-time note the call carries, so the model learns the machine
	// it asked for was restarted (and what that may have cost).
	EnsureConnected(ctx context.Context, executorID string) error

	// NoteBinding records the child's current HOME executor and workspace so
	// the rest of the daemon (labels, prompt visibility, teardown) can see it.
	// Only the home binding is ever noted: the labels name one workspace, and
	// handleChildExit releases that one by label.
	NoteBinding(childID, executorID, workspaceID string)

	// SetHomeLabel records the child's home executor on its own row, so a
	// resumed child agrees with the daemon that migrated it.
	SetHomeLabel(childID, executorID string)

	// ClearBinding removes the labels NoteBinding wrote. Best-effort, and it
	// exists for exactly one caller: a label write that lost a race with the
	// child's exit names a workspace the exit sweep already released, and a
	// resumed child plus teardown both read those labels.
	ClearBinding(childID string)

	// UndoBinding restores the labels NoteBinding wrote for (wroteExecID,
	// wroteWSID) to priorExecID with no workspace, and reports whether it did.
	//
	// It is the rollback half of a home migration that lost its claim (see
	// boundExecutor.rollbackLostMigration), and it writes NOTHING unless the
	// labels still name exactly what that migration wrote: another path may have
	// published a workspace in the meantime, and a blind clear would delete a
	// live binding's only handle.
	UndoBinding(childID, wroteExecID, wroteWSID, priorExecID string) bool

	// DisplayName names an executor for an operator and for the model: its
	// machine label, else a short id.
	DisplayName(executorID string) string

	// WorkspaceMode reports the workspace_mode of executorID, from the
	// executor's ROW -- never from Describe: the value decides whether a child
	// may be RE-PROVISIONED somewhere else after a machine is lost (it never
	// decides whether the loss fails one), and a machine that wants children
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

	// closed is set by the exit sweep. A provision still in flight at that
	// moment publishes a workspace nothing will ever release, so the
	// provisioner checks this after its RPC and releases what it made.
	closed bool

	// migrating is non-nil while ONE home transition is in flight, and is closed
	// when it has settled: a migration's provision + label write + home claim
	// (see migrateHome), or a loss clearing the home (see Forget). It keeps two
	// migrations from interleaving a winner's home claim with a loser's label
	// write, and keeps a loss from clearing the home between a migration's label
	// write and its claim. A channel rather than a lock because b.mu is never
	// held across an RPC or a store write.
	migrating chan struct{}
}

// binding is one live (executor, workspace) pair.
type binding struct {
	wsID string
	cl   tools.ExecutorClient

	// cwd records that this workspace was provisioned as the child's HOME
	// workspace (its cwd applies, the labels name it). A binding made for a
	// named call is a root-level workspace and can never be PROMOTED: the
	// child's cwd would not apply there, so a promotion re-provisions.
	cwd bool

	// ready is closed once this binding's FIRST provision has settled --
	// successfully or not. It is what lets provisioning happen OUTSIDE b.mu
	// while still guaranteeing one provision per executor and that no caller
	// ever observes a half-built binding.
	ready chan struct{}
	// err is set (before ready is closed) when the first provision failed. The
	// placeholder is dropped from the map, so the NEXT call tries again; every
	// caller already waiting on this one gets this error rather than a second
	// provision attempt.
	err error

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
	defer b.mu.Unlock()
	bd := b.bindings[home]
	// An unprovisioned PLACEHOLDER is not a binding: "bound" promises a
	// workspace exists. The placeholder's executor id is still reported, so a
	// caller can say which machine the child is coming up on.
	if bd == nil || bd.wsID == "" {
		return home, "", false
	}
	// Snapshotted under the lock: a reprovision rewrites wsID in place.
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

// homeTransitionWait bounds how long a path that needs the home-transition slot
// waits for the transition already in flight before going ahead without it.
//
// Every home transition — a migration, a loss, an adoption, a re-provision of the
// home binding — takes the same slot (withHomeTransition), and each one's
// fallback on expiry is the loser's half of the protocol: the migration rolls
// back its label write, the loss proceeds and lets the migration roll back, the
// adoption declines to claim, the re-provision undoes its label write.
//
// A var rather than a const so a test can spend the wait instead of sleeping
// through it: the interesting branch is what happens when it expires.
var homeTransitionWait = 5 * time.Second

// Forget removes one binding WITHOUT releasing its workspace — the caller owns
// the release. Used when an executor's connection is known to be gone and the
// next call must not find a stale client. If the binding was home, the home
// pointer is cleared too (rewriting the labels is the caller's job).
//
// CLEARING THE HOME IS SERIALIZED WITH EVERY OTHER HOME TRANSITION, because they
// write the same thing from opposite directions. A migration publishes its
// target's labels INSIDE its provision and claims the home only afterwards, so a
// loss that cleared the home in that window left the labels naming a target
// nothing had claimed — rafiki/executor/rafiki/workspace on one machine,
// rafiki/home-executor on another, and b.home empty — and no next call or
// migration is guaranteed before a daemon restart, which resumes onto exactly
// those labels. When the loss takes the slot it therefore happens entirely
// BEFORE a migration (which then claims the home it finds) or entirely AFTER
// one, clearing the home that migration established.
//
// The wait is NON-BLOCKING (homeWaitNone), deliberately: Forget's only caller is
// HandleExecutorLost, which runs on the executor pool's park-sweep goroutine and
// handles every affected child in sequence, so a per-child wait would delay every
// other child's teardown in that sweep. The slot's holder is doing an RPC that
// can hang, so a loss declines to wait for it and takes the protocol's recorded
// loser side instead: it still forgets the binding, the migration loses its claim
// and rolls its own label write back (migrateHomeNow), an adoption refuses the
// home because the binding it published was forgotten (adoptHomeNow's `mine`
// check), and a re-provision's label write compensates itself (noteHomeBinding).
// It cannot deadlock: nothing that holds the slot waits on a Forget.
//
// RESIDUAL (A1, partially-enforced invariant): Forget releases the slot before
// HandleExecutorLost rewrites the child's labels. A lazy adoption landing in that
// window can establish a live home binding, and the handler's later write then
// removes rafiki/workspace and sets executor-state=unbound on a bound child. The
// home is still labelled (rafiki/home-executor and rafiki/executor survive), so a
// restart resolves onto the labelled executor and provisions a fresh home there;
// the workspace the live binding held is orphaned on that executor until it
// restarts. Narrow, recorded rather than claimed impossible.
func (b *boundExecutor) Forget(executorID string) (workspaceID string, ok bool) {
	_ = b.withHomeTransition(context.Background(), homeWaitNone, func(held bool) error {
		if !held {
			slog.Warn("a home transition was in flight; forgetting the binding without its slot — "+
				"the migration rolls its label write back if it loses the home",
				"child", b.childID, "executor", shortID(executorID))
		}
		workspaceID, ok = b.forget(executorID)
		return nil
	})
	return workspaceID, ok
}

// forget is Forget's state change, run with the transition slot (held or not).
func (b *boundExecutor) forget(executorID string) (string, bool) {
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

// homeWaitPolicy says how a caller's wait for the home-transition slot ends. The
// slot is ONE state machine (withHomeTransition); this only chooses which
// documented fallback the caller owns when it cannot be taken.
type homeWaitPolicy int

const (
	// homeWaitBounded is the LOSER-side wait: at most homeTransitionWait, then the
	// caller runs WITHOUT the slot and covers it with its own half of the
	// protocol — an adoption declines the claim (adoptHomeNow's `mine` check) and
	// a re-provision undoes its own label write. The only user is adoptHome; it
	// ignores the caller's context on purpose, because an adoption that gave up on
	// a cancelled call would be the same decline by another route. The time budget
	// is the only bound, so this wait can never be unbounded.
	homeWaitBounded homeWaitPolicy = iota

	// homeWaitCaller is a MIGRATION's wait: the caller's own context bounds it, so
	// a tool call's cancellation is honoured. On expiry fn does NOT run and
	// ctx.Err() is returned — a transition that never got the slot must not
	// provision or write labels as if it had.
	homeWaitCaller

	// homeWaitNone never waits, and it is used by the two callers that must not:
	// a PROVISION (which publishes the placeholder other callers wait on — a
	// transition holder can be one of them, so waiting for the slot would let a
	// holder wait on the very goroutine waiting for it) and a LOSS (Forget, which
	// runs on the pool's park-sweep goroutine for every affected child in
	// sequence). When the slot is busy they go ahead without it; the loser side
	// is the protocol's recorded fallback, and a home provision is never failed
	// for losing a race it does not need to win.
	homeWaitNone
)

// withHomeTransition runs fn with the home-transition slot (b.migrating) held,
// waiting for the transition already in flight to settle first. It is the ONE way
// to take the slot: Forget, adoptHome, migrateHome, the spawn-side adopt and every
// home-binding provision/write go through it, so there is one install/release
// ordering to reason about rather than one copy per caller.
//
// held tells fn whether it actually got the slot — false under homeWaitBounded
// and homeWaitNone, where the caller proceeds and compensates. b.mu is never held
// across the wait or across fn: the slot is a channel, not a lock, for exactly
// that reason, and it is released however fn returns.
func (b *boundExecutor) withHomeTransition(ctx context.Context, wait homeWaitPolicy, fn func(held bool) error) error {
	deadline := time.Time{}
	if wait == homeWaitBounded {
		deadline = time.Now().Add(homeTransitionWait)
	}
	for {
		b.mu.Lock()
		inflight := b.migrating
		if inflight == nil {
			done := make(chan struct{})
			b.migrating = done
			b.mu.Unlock()
			return func() error {
				defer func() {
					b.mu.Lock()
					b.migrating = nil
					b.mu.Unlock()
					close(done)
				}()
				return fn(true)
			}()
		}
		b.mu.Unlock()

		switch wait {
		case homeWaitNone:
			return fn(false)
		case homeWaitBounded:
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return fn(false)
			}
			timer := time.NewTimer(remaining)
			select {
			case <-inflight:
				timer.Stop()
			case <-timer.C:
				return fn(false)
			}
		default:
			select {
			case <-inflight:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Multi reports whether a tool call must name an executor: the child's declared
// set names several, or it can reach 2+ right now. An error reads as single: the
// caller uses this only to decide whether to mention an executor in a notice.
func (b *boundExecutor) Multi() bool {
	if b.binder.DeclaredMulti(b.childID) {
		return true
	}
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
//
// The label read is a STORE read and deliberately happens with b.mu released:
// this runs on every path that provisions, and a wedged store must not hold up
// an unrelated executor's binding.
func (b *boundExecutor) homeExecutor() string {
	b.mu.Lock()
	home, seeded := b.home, b.homeSeeded
	b.mu.Unlock()
	if seeded || home != "" {
		return home
	}

	labeled := b.binder.Home(b.childID)

	b.mu.Lock()
	defer b.mu.Unlock()
	// Mark the seeding even when the label was empty: a home the daemon already
	// cleared (Forget) or adopted in memory must not be re-derived from a stale
	// label a moment later.
	b.homeSeeded = true
	if b.home == "" {
		b.home = labeled
	}
	return b.home
}

// resolve decides which executor a call goes to: the one it names, or the only
// one it can reach.
//
// It reads the caller's ExecutorTarget (nil for the daemon's own internal
// calls) and fills in Resolved/Multi on the way out, so the tool wrapper can
// tell the model which machine served the call.
//
// An implicit call is ambiguous whenever the child's DECLARED set names more
// than one executor (`all`, or two or more ids), however many of them are
// connected right now: a member that drops must not turn a call that omitted the
// target into one that lands on the survivor. Only a child whose set names
// exactly one executor, or a legacy row resolved through its selector, decides
// by what it can reach. Home is NOT a default for tool calls, home included.
func (b *boundExecutor) resolve(ctx context.Context) (execID string, multi bool, err error) {
	reach, err := b.binder.Reachable(b.childID)
	if err != nil {
		return "", false, err
	}
	t := tools.ExecutorTargetFrom(ctx)
	// The daemon's own internal calls (a prefill read, a skill sync) carry no
	// target and nobody to ask for one, so only a call that came through the tool
	// wrapper is held to the declared set.
	declaredMulti := t != nil && b.binder.DeclaredMulti(b.childID)
	multi = declaredMulti || len(reach) >= 2
	if declaredMulti && t.Requested == "" && t.HomeDefault {
		// A daemon-initiated read of the child's own cwd (the spawn's pre-fill) has
		// nobody to ask for a target, and the cwd lives on the home executor.
		if home := b.homeExecutor(); home != "" {
			if e, ok := matchExecutorRef(home, reach); ok {
				setTarget(t, b.binder.DisplayName(e.ID), multi)
				return e.ID, multi, nil
			}
		}
	}

	if t != nil && t.Requested != "" {
		if e, ok := matchExecutorRef(t.Requested, reach); ok {
			setTarget(t, b.binder.DisplayName(e.ID), multi)
			return e.ID, multi, nil
		}
		t.Multi = multi
		return "", multi, b.unreachableName(t.Requested, reach)
	}

	if declaredMulti {
		if t != nil {
			t.Multi = true
		}
		return "", true, ambiguousTargetError(b.binder, reach)
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
		// The only executor this child can use becomes home: it is where the
		// child's cwd and project tier live. Adopted only once its workspace
		// exists (see adoptHome).
		if b.homeExecutor() == "" && b.binder.CanBeHome(id) {
			if aerr := b.adoptHome(ctx, id); aerr != nil {
				return "", false, aerr
			}
		}
		setTarget(t, b.binder.DisplayName(id), false)
		return id, false, nil

	case len(reach) == 1:
		e := reach[0]
		home := b.homeExecutor()
		// An executor installed over ssh is never a home, the same exclusion
		// spawn-time resolution applies: adoption and migration skip it, and the
		// call proceeds on it as an ordinary workspace.
		canHome := b.binder.CanBeHome(e.ID)
		switch {
		case home == "":
			// Adopted only once its workspace exists: a failed first provision
			// must leave NO home, not a home with nothing behind it that the
			// next call reports as "pinned there".
			if canHome {
				if aerr := b.adoptHome(ctx, e.ID); aerr != nil {
					return "", false, aerr
				}
			}
		case home != e.ID:
			if !mayMigrate(b.binder.WorkspaceMode(home)) {
				return "", false, fmt.Errorf("executor %s is not connected; this agent's workspace is pinned there, so calls fail until it reconnects", b.binder.DisplayName(home))
			}
			// Ephemeral: the call migrates the child's home to the machine that
			// is actually there, rather than failing work the operator already
			// declared movable.
			if canHome {
				if merr := b.migrateHome(ctx, home, e.ID); merr != nil {
					return "", false, merr
				}
			}
		}
		setTarget(t, b.binder.DisplayName(e.ID), false)
		return e.ID, false, nil
	}

	if t != nil {
		t.Multi = true
	}
	return "", true, ambiguousTargetError(b.binder, reach)
}

// ambiguousTargetError is the refusal of an implicit call by a child that must
// name its target. It lists what is reachable right now, which may be fewer
// than the declared set.
func ambiguousTargetError(binder executorBinder, reach []executors.Executor) error {
	noun := "executors"
	if len(reach) == 1 {
		noun = "executor"
	}
	return fmt.Errorf("this agent can reach %d %s (%s); pass executor: \"<name>\"", len(reach), noun, reachNames(binder, reach))
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
	// homeExecutor, not b.home: a child resumed from a row has its home ONLY in
	// the label until something seeds it, and that label is exactly what makes
	// "the machine I used to be on" a connectivity answer. Seeded before the
	// lock, because seeding is a store read.
	home := b.homeExecutor()
	b.mu.Lock()
	ids := make([]string, 0, len(b.bindings)+1)
	for id := range b.bindings {
		ids = append(ids, id)
	}
	if home != "" {
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

// reachableID reports whether id is in a reachable set.
func reachableID(reach []executors.Executor, id string) bool {
	for _, e := range reach {
		if e.ID == id {
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
func (b *boundExecutor) clientFor(ctx context.Context, execID string) (*binding, error) {
	return b.ensureBinding(ctx, execID, b.homeExecutor() == execID)
}

// callTarget snapshots a binding's client and workspace BEFORE a call is
// dispatched, under the lock.
//
// Both matter as a SNAPSHOT. A concurrent recovery rebuilds a binding IN PLACE
// (same *binding, new wsID/cl), so reading either back after the call would
// attribute the failure to a workspace that was never called -- which turns one
// coalesced recovery into a fresh one per caller -- and reading them without
// the lock is a data race. Every read of a binding's client goes through here
// or through retryBinding, which takes the same snapshot.
func (b *boundExecutor) callTarget(bd *binding) (tools.ExecutorClient, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bd.cl, bd.wsID
}

// ensureBinding returns execID's binding, provisioning it on first use as (or
// not as) the child's HOME workspace. It is the entry point for a caller that
// does NOT hold the home-transition slot; a caller that does (an adoption, a
// migration) uses ensureBindingHeld, because the slot is not re-entrant.
//
// Provisioning happens OUTSIDE b.mu, through a placeholder published under it.
// The placeholder is what keeps the two invariants of a map of bindings: exactly
// one provision per executor, and no caller ever observing a half-built
// binding. A map-wide lock would have bought the same thing at the cost of
// every healthy executor on the child waiting out one slow provision (and
// NoteBinding, and the label read) on an unrelated machine.
//
// wantCwd promotes: a binding that exists as a root-level workspace cannot
// serve as HOME (the child's cwd would never apply), so it is replaced by a
// freshly provisioned home workspace.
func (b *boundExecutor) ensureBinding(ctx context.Context, execID string, wantCwd bool) (*binding, error) {
	return b.ensureBindingHeld(ctx, execID, wantCwd, false, false)
}

// ensureBindingHeld is ensureBinding for a caller that already holds the
// home-transition slot (held == true) — a migration's or an adoption's
// provision. A home workspace's publish and label write are one step inside that
// slot (see provision), and re-taking it here would deadlock on a channel that
// this goroutine already holds.
//
// claimPending says this caller WILL SET b.home to execID once the provision
// returns: true for a migration and an adoption, false for every ordinary call
// (including a promotion, which relabels on the strength of an earlier read of
// the home). It is threaded down to the label write, which judges a home that
// moved under a write that claims nothing (noteHomeBinding).
func (b *boundExecutor) ensureBindingHeld(ctx context.Context, execID string, wantCwd, held, claimPending bool) (*binding, error) {
	if execID == "" {
		return nil, errors.New("no executor to bind to")
	}

	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return nil, errChildExited
		}
		bd := b.bindings[execID]
		if bd == nil {
			bd = &binding{ready: make(chan struct{})}
			b.bindings[execID] = bd
			b.mu.Unlock()
			return b.provision(ctx, bd, execID, wantCwd, held, claimPending)
		}
		ready := bd.ready
		b.mu.Unlock()

		<-ready
		if bd.err != nil {
			// This placeholder's first provision failed. Its error is the
			// answer; a later call (or a concurrent caller that has not yet
			// captured it) provisions afresh. (N2, accepted: a waiter also
			// inherits a LEADER's context cancellation, because the RPC ran on
			// the leader's context. The placeholder is dropped with it, so the
			// next call retries and nothing is leaked or wedged.)
			return nil, bd.err
		}

		b.mu.Lock()
		current := b.bindings[execID] == bd
		// cwd is read under the lock: a reprovisioning recovery rewrites it in
		// place.
		var oldBd *binding
		var oldWS string
		var newBd *binding
		if current && wantCwd && !bd.cwd && !b.closed {
			// PUBLISH SITE (promotion): a root-level workspace cannot serve as
			// home, because the child's cwd would never apply there.
			//
			// Provision-then-release, never destroy-then-provision: the
			// replacement is made FIRST and published under the lock, and the
			// root-level workspace is released only after that succeeded. A
			// failed re-provision therefore KEEPS the workspace the agent was
			// using there — its files, its retained job output — and puts the
			// old binding back. Its own workspace publish is provision's, which
			// takes the same guard.
			oldBd, oldWS = bd, bd.wsID
			newBd = &binding{ready: make(chan struct{})}
			b.bindings[execID] = newBd
		}
		b.mu.Unlock()

		if oldBd != nil {
			nb, perr := b.provision(ctx, newBd, execID, true, held, claimPending)
			if perr != nil {
				b.mu.Lock()
				// The failed provision has already dropped its own placeholder,
				// so "nil" is ours too. Put the old binding back: its workspace
				// is still registered and unreleased.
				cur := b.bindings[execID]
				restore := !b.closed && (cur == newBd || cur == nil)
				if cur == newBd {
					delete(b.bindings, execID)
				}
				if restore {
					b.bindings[execID] = oldBd
				}
				b.mu.Unlock()
				if !restore {
					// The child exited: nothing else will release the
					// workspace the promotion was replacing.
					b.releaseAsync(ctx, execID, oldWS)
				}
				return nil, perr
			}
			b.releaseAsync(ctx, execID, oldWS)
			return nb, nil
		}
		if !current {
			continue // replaced while we waited: use the winner's binding
		}
		return bd, nil
	}
}

// The two ways a binding can stop being the place a workspace belongs, each
// with its own sentence because they mean different things to a caller: the
// child exited (nothing more will run), or its bindings were released / replaced
// under it (a caller may retry, or may not).
var (
	errChildExited       = errors.New("this child exited")
	errBindingSuperseded = errors.New("this child's executor bindings were released")
)

// publishWorkspace installs a just-provisioned workspace into bd, and returns
// false — after RELEASING the workspace — when this binding is no longer the one
// the provision started from, or the child has exited.
//
// EVERY site that makes a workspace publishes through here, and that is the
// point: a workspace installed after the exit sweep, or into a binding a
// promotion has replaced, is a workspace nothing will ever release. The two
// conditions are deliberately ONE guard rather than two, so a new caller cannot
// implement half of it.
//
// The lock is held only for the check and the field write: the RPC that made the
// workspace happened outside it.
// The returned error names why the publish was refused (nil on success).
func (b *boundExecutor) publishWorkspace(ctx context.Context, bd *binding, execID, wsID string, cl tools.ExecutorClient, cwd bool) error {
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		b.releaseAsync(ctx, execID, wsID)
		return errChildExited
	case b.bindings[execID] != bd:
		b.mu.Unlock()
		b.releaseAsync(ctx, execID, wsID)
		return fmt.Errorf("%w (executor %s)", errBindingSuperseded, shortID(execID))
	}
	bd.wsID, bd.cl, bd.cwd = wsID, cl, cwd
	b.mu.Unlock()
	return nil
}

// noteHomeBinding is the ONE place a home binding's durable pair —
// rafiki/executor + rafiki/workspace — is written. claimPending says whether the
// caller WILL SET b.home to this executor when its provision returns: a migration
// (migrateHomeNow) and an adoption (adoptHomeNow) do, and their write legitimately
// PRECEDES that claim, so b.home still names the executor a migration is leaving
// when its target's labels land. A first provision, a promotion and a re-provision
// never claim the home — they label because an earlier read said this executor WAS
// home — so for them claimPending is false even when they hold the slot.
//
// It compensates the three ways that write can still lose, and nothing else:
//
//   - the child EXITED under it. publishWorkspace makes a workspace visible
//     atomically under b.mu, but this write is store I/O after the lock is
//     dropped: the exit sweep can snapshot the labels it releases between the
//     two, so a provision published an instant before an exit labels a child that
//     is gone with a workspace the sweep has already released. Removal is
//     confined to b.closed, which stops every later publish, so nothing
//     legitimate can be writing these labels once it is set.
//   - the binding was FORGOTTEN under it. Forget never waits for the slot
//     (homeWaitNone), so while this transition holds the slot a loss can still
//     run its state change and its caller rewrite the labels it owns
//     (HandleExecutorLost). Writing ours afterwards would leave the durable
//     labels naming a workspace the loss already released while b.home names
//     nothing — the disagreement a resume reads. UndoBinding restores them only
//     while they still name exactly this write's pair, so a replacement's labels
//     are never touched. The workspace needs no release here: Forget returned it
//     to the caller that owns the release.
//   - the HOME named a different executor by the time this write returned, and
//     this caller has no claim of its own coming. A migration publishes its
//     target's labels INSIDE its own provision and claims the home only
//     afterwards, which is why a claim-PENDING write is never judged here. Every
//     other home label write is made on the strength of an earlier read of the
//     home, and that read can go stale before the write lands: another transition
//     may have moved the home, whether or not this caller managed to take the
//     slot first. The settled state is the contract: once this write returns, the
//     home must be empty or name THIS executor. If it names a different one, that
//     home won and this write just overwrote its labels, so the loser takes back
//     exactly its own pair and leaves the winner's claim alone (UndoBinding puts
//     rafiki/executor back to that home and removes rafiki/workspace, which is
//     what a resumed child provisions a fresh home from). The workspace needs no
//     release: the binding is still live in b.bindings, so the exit sweep
//     releases it.
//
// Best-effort throughout: a failed removal is a warning, never a failure of the
// call that made the workspace.
//
// (Residual: if the sweep read the labels BEFORE this write and sets closed
// after it, this check does not see it. That window is narrower than the one
// closed by this compensation and ends the same way — labels naming a released
// workspace — so it is recorded here rather than pretended away.)
func (b *boundExecutor) noteHomeBinding(bd *binding, execID, wsID string, claimPending bool) {
	b.binder.NoteBinding(b.childID, execID, wsID)

	// ONE critical section for the whole judgement: b.home and the binding's
	// identity are read together, so a transition that completes between two
	// separate reads can neither make a moved home look unchanged nor make an
	// unchanged one look moved.
	b.mu.Lock()
	closed := b.closed
	mine := b.bindings[execID] == bd
	home := b.home
	b.mu.Unlock()
	switch {
	case closed:
		b.binder.ClearBinding(b.childID)
		slog.Warn("labelled the home workspace of a child that exited while the label write was in flight; "+
			"removed the labels again so nothing reads a released workspace",
			"child", b.childID, "executor", shortID(execID), "workspace", wsID)
	case !mine:
		if b.binder.UndoBinding(b.childID, execID, wsID, execID) {
			slog.Warn("labelled a home workspace whose binding a concurrent loss had forgotten; "+
				"removed the labels again so nothing resumes onto a workspace the loss released",
				"child", b.childID, "executor", shortID(execID), "workspace", wsID)
		}
	case !claimPending && home != "" && home != execID:
		if b.binder.UndoBinding(b.childID, execID, wsID, home) {
			slog.Warn("labelled a home workspace whose home another transition had already moved, with no "+
				"claim of our own coming; took it back so the durable pair names the home that won",
				"child", b.childID, "executor", shortID(execID), "workspace", wsID,
				"home", shortID(home))
		}
	}
}

// appendExecutorNote adds one line to the one-time note a tool call carries,
// joined with a newline when another line already claimed the slot. The slot is
// ONE field: a relaunch note and a "working from <dir>" note both apply to the
// same first call, and the second one overwriting the first would hide the more
// consequential fact.
func appendExecutorNote(ctx context.Context, note string) {
	t := tools.ExecutorTargetFrom(ctx)
	if t == nil || note == "" {
		return
	}
	if t.Note != "" {
		t.Note += "\n"
	}
	t.Note += note
}

// provision performs one binding's first provision. Called with no lock held;
// bd is the placeholder already published under it.
//
// The binding becomes visible to a caller only when ready is closed, so nothing
// can use a workspace whose labels are not yet written, and a failed provision
// is reported to every caller already waiting rather than to one of them.
//
// held says the caller already holds the home-transition slot (a migration's or
// an adoption's provision); otherwise a HOME workspace's publication and label
// write take it HERE when it is free — as one step, so a Forget cannot slip
// between the publish and the write — and go ahead without it when it is not.
// They never WAIT for it: a provision publishes the placeholder a transition
// holder can be waiting on, so blocking here is the one way this type could
// deadlock (see homeWaitNone). The slot is not re-entrant, which is why the flag
// exists rather than provision taking it unconditionally.
//
// claimPending is the caller's, not the slot's: it says b.home will be set to
// execID after this returns, so the label write must not be judged against a home
// that still names someone else. Taking the slot HERE does not make it true — a
// provision that acquires a free slot may still be labelling on a stale read.
func (b *boundExecutor) provision(ctx context.Context, bd *binding, execID string, wantCwd, held, claimPending bool) (*binding, error) {
	// An installed executor that is down is brought back BEFORE the workspace is
	// asked for: a provision is the first thing any call does on a machine, so it
	// is the one place worth relaunching at. A launch failure is the call's
	// answer, not the child's death — the binding placeholder is dropped so the
	// next call tries again.
	if err := b.binder.EnsureConnected(ctx, execID); err != nil {
		b.failFirstProvision(bd, execID, err)
		return nil, err
	}

	wsID, workdir, cl, err := b.binder.ProvisionOn(ctx, execID, wantCwd)
	if err != nil {
		err = fmt.Errorf("workspace on executor %s: %w", shortID(execID), err)
		b.failFirstProvision(bd, execID, err)
		return nil, err
	}

	// PUBLISH SITE (first provision) plus, for a home workspace, the label write:
	// publishWorkspace takes the lock, confirms this placeholder still owns the
	// executor and the child is still running, and releases the workspace itself
	// when either is false — the exit sweep has already taken its snapshot, so
	// nothing else knows this workspace exists.
	publish := func() error {
		if perr := b.publishWorkspace(ctx, bd, execID, wsID, cl, wantCwd); perr != nil {
			return perr
		}
		// The labels name the HOME workspace; another executor's workspace is none
		// of the daemon's business (handleChildExit releases that one by label).
		// Written before ready closes: a home binding must never be usable before
		// teardown can find it. claimPending is the CALLER's (not the slot's), so
		// a write that claims no home judges the one it leaves behind
		// (noteHomeBinding).
		if wantCwd {
			b.noteHomeBinding(bd, execID, wsID, claimPending)
		}
		return nil
	}

	var perr error
	if wantCwd && !held {
		perr = b.withHomeTransition(ctx, homeWaitNone, func(bool) error { return publish() })
	} else {
		perr = publish()
	}
	if perr != nil {
		b.failFirstProvision(bd, execID, perr)
		return nil, perr
	}
	close(bd.ready)

	if !wantCwd {
		// One-time note, on the first provision of a non-home binding: the
		// child's cwd does NOT apply here, and a model that assumes it does
		// writes into the wrong tree.
		appendExecutorNote(ctx, fmt.Sprintf("[%s] working from %s", b.binder.DisplayName(execID), workdir))
	}
	return bd, nil
}

// failFirstProvision drops a failed placeholder (so the NEXT call gets a fresh
// attempt) and reports its error to every caller already waiting on it.
func (b *boundExecutor) failFirstProvision(bd *binding, execID string, err error) {
	b.mu.Lock()
	if b.bindings[execID] == bd {
		// A dead placeholder left in a closed map would also make the map
		// claim a binding the sweep already decided about.
		delete(b.bindings, execID)
	}
	bd.err = err
	b.mu.Unlock()
	close(bd.ready)
}

// adoptHome makes execID the child's home. The workspace is provisioned AS a
// home workspace first — the child's cwd applies there and the labels name it —
// and only then is the home recorded, so a failed first provision leaves the
// child WITH NO HOME rather than with one that has no workspace behind it.
//
// IT TAKES THE SAME HOME-TRANSITION SLOT as every other home transition
// (withHomeTransition). Adoption is the LAZIEST of the three — it runs
// mid-child, on a live binding, the moment a call finds no home — so it is the
// one most likely to collide with a loss, not the least: a loss landing between
// this provision's label write and the claim below forgot the binding and
// rewrote the labels, leaving the child with a home whose workspace the daemon
// had just released. With the slot held, a loss colliding with the adoption
// either forgets the binding under it (the claim below then sees `!mine` and
// declines the home) or happens after the adoption, clearing the home it
// established.
//
// The slot does NOT cover the loss handler's own label rewrite, which runs after
// Forget releases it (HandleExecutorLost, workspace_wiring.go): an adoption that
// lands in that window can leave a live home binding whose rafiki/workspace label
// the handler has just removed and whose executor-state it has just set to
// `unbound`. That is a recorded residual of a partially-enforced invariant, not
// an ordering this slot makes impossible — the home label survives, so a restart
// resolves onto it, and the live workspace is orphaned on that executor until it
// restarts.
//
// Nothing that holds the slot can reach here, so the slot is never re-entered:
// migrateHomeNow only provisions and claims, Forget only mutates under b.mu,
// and neither calls resolve — which is the one path to adoptHome. Adoption
// waiting behind a migration is therefore bounded work, not a cycle; it waits
// with b.mu released, exactly as every other waiter does. The held flag is
// threaded down to the provision so it writes the labels without trying to take
// the slot again.
//
// The durable home label is written here too. Until this path wrote it, a child
// adopted lazily kept rafiki/home-executor naming the machine it used to be on —
// after a loss, the machine just declared gone — and a restart resumed onto that
// one while the labels named the adopted one. noteHomeExecutor's "never move a
// persisted home" rule governs the SPAWN resolver; this is the one place that
// decides a home for a child that has none, so memory and the label must agree
// on the decision.
//
// (N6, accepted: in the window between that provision and the claim below, a
// concurrent NAMED call to the same executor can provision a root-level
// workspace there, which this adoption then promotes. Narrow, and self-healing
// now that promotion is provision-then-release: the root workspace survives
// until its replacement exists.)
func (b *boundExecutor) adoptHome(ctx context.Context, execID string) error {
	return b.withHomeTransition(ctx, homeWaitBounded, func(held bool) error {
		if !held {
			slog.Warn("a home transition did not settle before the wait expired; adopting without its "+
				"slot — this adoption declines the home if its binding is forgotten under it",
				"child", b.childID, "executor", shortID(execID), "waited", homeTransitionWait)
		}
		return b.adoptHomeNow(ctx, execID, held)
	})
}

// adoptHomeNow is the adoption, run with the transition slot held (or, when the
// bounded wait expired, without it — see adoptHome).
func (b *boundExecutor) adoptHomeNow(ctx context.Context, execID string, held bool) error {
	if home := b.homeExecutor(); home != "" {
		// Another caller made a home while this one waited for the slot. Its home
		// stands; this call proceeds on the executor it resolved, without one.
		return nil
	}

	bd, err := b.ensureBindingHeld(ctx, execID, true, held, true)
	if err != nil {
		return err
	}

	b.mu.Lock()
	exited := b.closed
	// The claim is conditional on the binding this adoption published still being
	// the child's: a loss that could not take the slot (see withHomeTransition)
	// forgets it and its caller rewrites the labels, so recording a home here
	// would name a workspace that rewrite has already removed.
	mine := b.bindings[execID] == bd
	if mine && !exited {
		b.home, b.homeSeeded = execID, true
	}
	b.mu.Unlock()
	switch {
	case exited:
		return errChildExited
	case !mine:
		slog.Info("an adoption lost its binding to a concurrent loss; the child stays unbound",
			"child", b.childID, "executor", shortID(execID))
		return nil
	}

	b.binder.SetHomeLabel(b.childID, execID)
	return nil
}

// migrateHome moves the child's home binding to execID: it provisions (or
// promotes) a workspace there as the HOME workspace, so the child's cwd applies,
// and then tells the child its tree moved.
//
// The order is the interesting part. Home is claimed LAST, under b.mu, and only
// by a caller that still sees the home it set out to move:
//
//   - a failed provision leaves the home exactly where it was, so there is no
//     rollback that could clobber a move another caller completed meanwhile;
//   - concurrent implicit callers all reach ensureBinding, which provisions ONCE
//     per executor, and exactly one of them wins the claim and does the
//     bookkeeping — one label write, one steer;
//   - NoteBinding belongs to the provision (a home workspace is labelled when it
//     is made), so a migration must not write it again.
//
// ONE MIGRATION AT A TIME (b.migrating). The label write above happens inside
// the provision, i.e. BEFORE the home claim, so two migrations running at once
// can interleave the WINNER's claim with the LOSER's label write: the durable
// `rafiki/executor`/`rafiki/workspace` pair then names the loser's executor and
// a workspace nothing will release, while `rafiki/home-executor` — the value a
// resumed child binds by — names the winner. That does not self-heal: the next
// event may be a daemon restart.
//
// A caller that finds a migration in flight WAITS for it (its own ctx bounds the
// wait) and then re-considers: if the home has moved off the executor it set out
// to leave, it does nothing more and its call proceeds on the executor it
// RESOLVED, which ensureBinding binds as an ordinary root-level workspace. That
// path writes no labels, so the waiting caller can never produce a second label
// set, and it is never failed for losing the race. The leader never waits on a
// waiter, and b.mu is never held across either the wait or the RPCs, so no
// arrangement of these calls can deadlock.
//
// A LOSS takes the same slot (Forget), for the same reason and the mirror image
// of it: Forget clears the home, which would otherwise land between this
// migration's label write and its claim. Forget never WAITS for the slot
// (homeWaitNone), so a migration already holding it can have the home cleared
// under a provision that is still running — and then migrateHomeNow rolls its own
// label write back rather than leaving the durable labels naming a home nothing
// claimed.
func (b *boundExecutor) migrateHome(ctx context.Context, from, to string) error {
	return b.withHomeTransition(ctx, homeWaitCaller, func(held bool) error {
		b.mu.Lock()
		moved := b.home != from
		b.mu.Unlock()
		if moved {
			// Another caller already moved the home (here, or somewhere else) while
			// this one waited for the slot. It does nothing more and its call
			// proceeds on the executor it RESOLVED, which ensureBinding binds as an
			// ordinary root-level workspace — that path writes no labels, so a
			// waiter can never produce a second label set.
			return nil
		}
		return b.migrateHomeNow(ctx, from, to, held)
	})
}

// migrateHomeNow is one migration, run with b.migrating held (held is that
// fact, threaded down so the provision's label write does not re-take a slot
// this goroutine already holds).
//
// It can still LOSE the home it set out to claim: a loss clears the home without
// waiting for the slot when the migration's target provision is still running
// (see Forget). Losing is not an error — the caller's call proceeds on the
// executor it resolved — but the provision already published the target's
// labels, so this migration has to roll its own write back, or the durable labels
// name a workspace nothing claims while b.home names nothing at all.
func (b *boundExecutor) migrateHomeNow(ctx context.Context, from, to string, held bool) error {
	bd, err := b.ensureBindingHeld(ctx, to, true, held, true)
	if err != nil {
		return err
	}

	b.mu.Lock()
	won := b.home == from
	homeAfter := b.home
	// ours is the binding THIS migration published; a caller that replaced it
	// owns whatever is there now, labels included.
	ours := b.bindings[to] == bd && bd.wsID != ""
	undoWS := ""
	switch {
	case won:
		b.home, b.homeSeeded = to, true
	case ours && homeAfter != to:
		// The home was cleared while this provision ran, or another path moved it
		// somewhere else entirely. Either way nothing claims this workspace: undo
		// it now rather than leaving a workspace whose cwd semantics nothing
		// records for the exit sweep to find.
		undoWS = bd.wsID
		delete(b.bindings, to)
	}
	b.mu.Unlock()
	if !won {
		b.rollbackLostMigration(ctx, from, to, undoWS, homeAfter)
		return nil
	}

	b.binder.SetHomeLabel(b.childID, to)
	b.binder.NotifyMigrated(b.childID, from, to)
	slog.Info("migrated the child's home workspace",
		"child", b.childID, "from", shortID(from), "to", shortID(to))
	return nil
}

// rollbackLostMigration undoes exactly what a migration that lost its home claim
// wrote, and nothing else.
//
// homeAfter is the home the losing migration found instead of `from`: "" when a
// loss cleared it (the labels are then still the migration's own, and are put
// back to the home that was lost), or another executor when something else moved
// the home and owns the labels by now (they are left strictly alone). wsID is the
// workspace this migration provisioned and has already dropped; empty means the
// binding was replaced under it, so there is nothing of its own left at all.
func (b *boundExecutor) rollbackLostMigration(ctx context.Context, from, to, wsID, homeAfter string) {
	if wsID == "" {
		return
	}
	if homeAfter == "" {
		if b.binder.UndoBinding(b.childID, to, wsID, from) {
			slog.Warn("a home migration lost the home to a concurrent loss; restored the labels it "+
				"had written and released the workspace it had provisioned",
				"child", b.childID, "from", shortID(from), "to", shortID(to), "workspace", wsID)
		} else {
			// The labels name something this migration did not write: another path
			// rewrote them first, so they are left where that path put them.
			slog.Info("a home migration lost the home to a concurrent loss; its labels were "+
				"already rewritten by another path",
				"child", b.childID, "from", shortID(from), "to", shortID(to))
		}
	} else {
		slog.Info("another path moved the child's home while a migration provisioned; "+
			"released the workspace that migration had made",
			"child", b.childID, "from", shortID(from), "to", shortID(to),
			"home", shortID(homeAfter), "workspace", wsID)
	}
	b.releaseAsync(ctx, to, wsID)
}

// UndoBinding is the DURABLE half of rollbackLostMigration, which is why it sits
// here beside the migration protocol rather than with the rest of the
// Controller's binder in executor_binder_impl.go.
//
// The read-then-write is not one store transaction, and the ordering it can lose
// is a cost taken deliberately: a writer that publishes a new pair between the
// check and the write is publishing a workspace it holds live, so losing there
// leaves a label set that names NO workspace — a resume binds priorExecID and
// provisions a fresh home — never one naming a workspace nothing will release.
func (b *controllerBinder) UndoBinding(childID, wroteExecID, wroteWSID, priorExecID string) bool {
	if b.c.st == nil {
		return false
	}
	snap, ok := b.c.st.Get(childID)
	if !ok {
		return false
	}
	if snap.Labels["rafiki/executor"] != wroteExecID || snap.Labels["rafiki/workspace"] != wroteWSID {
		return false
	}
	set := map[string]string{}
	remove := []string{"rafiki/workspace"}
	if priorExecID == "" {
		remove = append(remove, "rafiki/executor")
	} else {
		set["rafiki/executor"] = priorExecID
	}
	if _, err := b.c.st.SetLabels(childID, set, remove); err != nil {
		slog.Warn("could not roll back a lost home migration's labels; a resumed child "+
			"would bind the home it moved to",
			"child", childID, "executor", shortID(wroteExecID),
			"workspace", wroteWSID, "error", err)
		return false
	}
	return true
}

// retryBinding returns the CLIENT and the executor a retry should run on after
// a recovery, as a snapshot taken under b.mu: a binding can be rebuilt IN PLACE
// while the retry is being decided, and reading its client afterwards would both
// race with that rebuild and run the call on a workspace it never named.
//
// ok is false when nothing serves the call any more. allowMigrated permits the
// fallback to the home binding, and exists for exactly ONE case: an IMPLICIT
// call whose recovery migrated the child to a new home. A NAMED call never
// follows home — it either retries on the executor it named or fails, which is
// what "a named call never migrates" has to mean when a concurrent implicit
// caller is the one that moved the home.
func (b *boundExecutor) retryBinding(execID string, allowMigrated bool) (cl tools.ExecutorClient, servedExec string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur := b.bindings[execID]; cur != nil && cur.cl != nil {
		return cur.cl, execID, true
	}
	if allowMigrated && b.home != "" && b.home != execID {
		if cur := b.bindings[b.home]; cur != nil && cur.cl != nil {
			return cur.cl, b.home, true
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
		// now serves this call — and for a NAMED call that means the executor it
		// named, never the home another caller migrated to.
		b.mu.Unlock()
		_, _, ok := b.retryBinding(execID, implicitCall(ctx))
		return ok
	}

	r := &recovery{done: make(chan struct{})}
	bd.inflight = r
	b.mu.Unlock()

	ok := b.doRecover(ctx, bd, execID, staleWS)

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
func (b *boundExecutor) doRecover(ctx context.Context, bd *binding, execID, staleWS string) bool {
	// An executor still in the live set means the WORKSPACE went, not the
	// machine: pkg/executor keeps its workspace registry in memory, so a
	// restart loses every id while the connection is fine. Re-provision in
	// place rather than re-running selection.
	if execID != "" && b.binder.IsLive(execID) {
		if b.reprovisionInPlace(ctx, bd, execID, staleWS) {
			return true
		}
	}

	// Still not live — but "not live" is not the same as "gone" for an executor
	// this daemon installed over ssh: its remote process can be restarted on the
	// same machine, and a NAMED call must not fail just because the process
	// ended. EnsureConnected is a no-op for every other executor, and a failed
	// relaunch falls through to the lost-executor path below rather than failing
	// the child.
	if execID != "" {
		if rerr := b.binder.EnsureConnected(ctx, execID); rerr != nil {
			slog.Warn("could not relaunch the installed executor a child was using; "+
				"treating it as lost",
				"child", b.childID, "executor", shortID(execID), "error", rerr)
		} else if b.binder.IsLive(execID) {
			if b.reprovisionInPlace(ctx, bd, execID, staleWS) {
				return true
			}
		}
	}

	// The executor itself is gone. Whether the child may follow is the ROW's
	// call, not this code's: the loss never fails the child, a pinned home is
	// only ever re-provisioned on that same executor, and a call that NAMED an
	// executor never migrates (the model asked for that machine).
	home := b.homeExecutor()
	if !implicitCall(ctx) || execID == "" || home != execID || !mayMigrate(b.binder.WorkspaceMode(execID)) {
		slog.Warn("child lost its executor and will not be migrated",
			"child", b.childID, "executor", shortID(execID),
			"implicit", implicitCall(ctx), "home", home == execID)
		b.dropBinding(ctx, bd, execID, staleWS)
		return false
	}

	// Migration is resolution's job — it applies the home rule, provisions on
	// the machine that is there, rewrites the labels and tells the child.
	next, _, err := b.resolve(ctx)
	if err != nil || next == "" || next == execID {
		slog.Warn("could not re-bind the child to any executor",
			"child", b.childID, "error", err)
		b.dropBinding(ctx, bd, execID, staleWS)
		return false
	}
	b.dropBinding(ctx, bd, execID, staleWS)
	slog.Info("re-bound child to a new executor",
		"child", b.childID, "from", shortID(execID), "to", shortID(next))
	return true
}

// reprovisionInPlace rebuilds the workspace on an executor whose connection is
// still live. Not a migration: the machine is the same one.
//
// It IS a home transition when the binding is the child's HOME binding, though:
// the workspace being replaced is the one its labels name, so the re-provision
// publishes a new pair. That path takes the home-transition slot and re-checks
// under it (reprovisionBinding), because a loss that could not take the slot must
// not be able to delete the binding between this publication and its label write
// — the defect this split closes.
//
// A root-level binding has no labels to keep in step, so it takes no slot and can
// never block behind a home transition on another machine.
func (b *boundExecutor) reprovisionInPlace(ctx context.Context, bd *binding, execID, staleWS string) bool {
	if b.homeExecutor() != execID {
		return b.reprovisionBinding(ctx, bd, execID, staleWS, false)
	}

	ok := false
	if err := b.withHomeTransition(ctx, homeWaitCaller, func(held bool) error {
		// Re-decide under the slot. A transition this one waited for may have moved
		// the home off execID, and then this rebuild is an ordinary root-level one
		// that writes no labels: provisioning it as HOME would re-label an executor
		// that no longer owns the child's tree.
		ok = b.reprovisionBinding(ctx, bd, execID, staleWS, b.homeExecutor() == execID)
		return nil
	}); err != nil {
		slog.Warn("re-provision could not take the home-transition slot; the call is not retried on this executor",
			"child", b.childID, "executor", shortID(execID), "error", err)
		return false
	}
	return ok
}

// reprovisionBinding performs one in-place re-provision. home says the rebuilt
// workspace is the child's HOME workspace: its cwd applies and the labels name
// it, which is only true when this caller holds the home-transition slot.
func (b *boundExecutor) reprovisionBinding(ctx context.Context, bd *binding, execID, staleWS string, home bool) bool {
	wsID, _, cl, err := b.binder.ProvisionOn(ctx, execID, home)
	if err != nil {
		slog.Warn("re-provision on the same executor failed; re-selecting",
			"child", b.childID, "executor", shortID(execID), "error", err)
		return false
	}
	// PUBLISH SITE (re-provision in place). The same guard as every other
	// publisher: this must still be the binding the recovery began on, and the
	// child must still be running. A re-provision that lands after the exit
	// sweep — a liveness failure on a LIVE executor is exactly that window —
	// would install a workspace nothing will ever release, and would label a
	// child that is gone.
	if perr := b.publishWorkspace(ctx, bd, execID, wsID, cl, home); perr != nil {
		slog.Warn("re-provision landed after the binding was released; discarding it",
			"child", b.childID, "executor", shortID(execID), "reason", perr)
		return false
	}
	if home {
		// The ONE home-label writer, inside the slot this caller holds. It undoes
		// its own write if a loss that could not take the slot forgot this binding
		// while the write was in flight, or if the home moved off execID while it
		// re-provisioned (noteHomeBinding).
		b.noteHomeBinding(bd, execID, wsID, false)
	}

	// The old workspace is still registered on a LIVE executor, so unlike the
	// migration path this release will actually land -- do it now rather than
	// stranding it and its retained job output until the executor exits.
	if staleWS != "" && staleWS != wsID {
		b.binder.ReleaseOn(ctx, execID, staleWS)
	}
	return true
}

// releaseTimeout bounds ONE best-effort workspace release. Each release gets
// its own: a dead executor must never spend the budget a live one needs.
const releaseTimeout = 5 * time.Second

// releaseAsync tears a workspace down best-effort, on its own deadline and
// detached from the caller's cancellation. A tool call that failed has already
// returned by the time this runs, and its context is dead with it.
func (b *boundExecutor) releaseAsync(ctx context.Context, execID, wsID string) {
	if wsID == "" {
		return
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		b.binder.ReleaseOn(rctx, execID, wsID)
	}()
}

// dropBinding forgets one binding and releases its workspace best-effort: the
// executor it lived on is usually unreachable, and the child must not wait on
// it.
func (b *boundExecutor) dropBinding(ctx context.Context, bd *binding, execID, wsID string) {
	b.mu.Lock()
	if b.bindings[execID] != bd {
		// Not the binding this recovery began on: something else owns it now,
		// and it will release its own workspace.
		b.mu.Unlock()
		return
	}
	delete(b.bindings, execID)
	b.mu.Unlock()
	b.releaseAsync(ctx, execID, wsID)
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
// call: the same executor if recover rebuilt it in place, else — for an IMPLICIT
// call only — the home binding a migration installed. There is no second
// recover.
func retryCall[T any](ctx context.Context, b *boundExecutor, execID string, callErr error, fn func(context.Context, tools.ExecutorClient) (T, error)) (T, error) {
	var zero T
	cl, _, ok := b.retryBinding(execID, implicitCall(ctx))
	if !ok {
		return zero, callErr
	}
	return fn(ctx, cl)
}

// callHome is callBound for the daemon's own fetches.
//
// They carry no tool target, because they are not tool calls: the project tier,
// the skill bodies and the skill files all describe the workspace the child's
// cwd lives in, which is the HOME binding and only ever the home binding.
//
// Home is subject to the SAME live set as every tool call. The home can go
// stale in ways the child's own record cannot show — a narrowed grant, a
// re-owned executor, a label written before the grant moved — and a binding
// made on that machine would put the child's project instructions and skill
// files on an executor it is no longer admitted to. An unreachable home is
// therefore "not connected": an error, never a provision target.
func callHome[T any](ctx context.Context, b *boundExecutor, idempotent bool, fn func(context.Context, tools.ExecutorClient) (T, error)) (T, error) {
	var zero T
	home := b.homeExecutor()
	if home == "" {
		return zero, errors.New("this agent has no home executor")
	}
	reach, err := b.binder.Reachable(b.childID)
	if err != nil {
		return zero, err
	}
	if !reachableID(reach, home) {
		return zero, fmt.Errorf("executor %s is not connected; this agent's home workspace is unreachable, so its project context and skills cannot be read", b.binder.DisplayName(home))
	}
	ctx = execpool.WithOwner(ctx, b.childID)
	bd, err := b.ensureBinding(ctx, home, true)
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
		rebuilt, rebuiltOn, ok := b.retryBinding(served, false)
		if !ok {
			return kerr
		}
		served = rebuiltOn
		kerr = rebuilt.KillJob(ctx, handle)
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
//
// The releases run CONCURRENTLY, each on its own deadline. Sequentially they
// shared one budget in map order, so a single dead executor's release could eat
// the whole deadline and starve every live machine behind it — the exact leak
// this sweep exists to close. It waits for them, so a caller that returns has
// released what it could.
func (b *boundExecutor) ReleaseOthers(ctx context.Context, keepExec, keepWS string) {
	type pair struct{ execID, wsID string }
	b.mu.Lock()
	// Closed first: a provision still in flight must know to release what it
	// is about to make rather than publish a workspace nothing will release.
	b.closed = true
	extra := make([]pair, 0, len(b.bindings))
	for execID, bd := range b.bindings {
		if execID == keepExec && bd.wsID == keepWS {
			continue
		}
		if bd.wsID == "" {
			// An unfinished PLACEHOLDER: no workspace exists yet, and the
			// provisioner releases whatever it lands (b.closed above).
			continue
		}
		extra = append(extra, pair{execID, bd.wsID})
	}
	b.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range extra {
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			b.binder.ReleaseOn(rctx, p.execID, p.wsID)
		}(p)
	}
	wg.Wait()
}
