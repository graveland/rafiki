package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
)

// executorPool is the slice of *execpool.Pool the controller uses. An
// interface so selection is testable without a listener, a database, or a
// dialling executor — the reason this whole path shipped untested.
type executorPool interface {
	Live() []execpool.LiveExecutor
	ClientFor(executorID string) (tools.ExecutorClient, error)
	// Tickets, Evict and DisconnectOwner serve transient executors and the
	// user-removal cut: mint a one-shot ticket, take an executor down when
	// the control connection that owns it closes, and disconnect every
	// executor a removed user owns. On the interface rather than reached by
	// type assertion so the selection tests' fake pool can satisfy them.
	Tickets() *execpool.TicketRegistry
	Evict(executorID string)
	DisconnectOwner(userID string) int
	// Release tears down a workspace on an executor. It is here, not on the
	// concrete *execpool.Pool only, because teardown is what both the
	// lost-executor handler (workspace_wiring.go) and the binder's rollback
	// must be able to DO — a fake pool that silently dropped the call left a
	// missing release green in every test.
	Release(ctx context.Context, executorID, workspaceID string) error
	// Provision creates a workspace on an executor, and ClientForWorkspace
	// returns a client whose calls are scoped to that workspace. Both are on
	// the interface rather than reached through a *execpool.Pool type
	// assertion so provisionWorkspace is drivable by a fake: its rollback —
	// release the workspace it just made when the scoped client cannot be
	// obtained — was unreachable from any test while the assertion stood, and
	// a leak nothing can observe is a leak nothing prevents.
	Provision(ctx context.Context, executorID string, req *executorpb.ProvisionRequest) (*executorpb.ProvisionResponse, error)
	ClientForWorkspace(executorID, workspaceID string) (tools.ExecutorClient, error)
}

// isBareExecutorRef reports whether s is a bare word — a machine name or an
// executor id — rather than selector syntax. A selector term always carries an
// operator (=, !=, in/notin, leading !) or a comma joining terms; anything
// else cannot match an executor's labels as a key-presence term in any useful
// way, and every spawn surface that accepts a bare word means "that one
// machine" (the CLI's --executor). A lone key like "env" is therefore
// promoted to a REF and refused as "no executor named \"env\"" rather than
// matching executors that carry an env label — the grammar loses one obscure
// spelling and the common case (the machine's name) stops being a
// silent-zero-match trap.
func isBareExecutorRef(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, "=,!()&|")
}

// promoteBareExecutorRef rewrites a bare-word selector into an ExecutorRef so
// it resolves through matchExecutorRef — machine label first, then id —
// against the spawner's executor set (resolveSpawnExecutorSet). This is
// the spawn-time normalization for the incident where an MCP caller passed
// executor: "greyshift" and ParseSelector read it as "must carry a label named
// greyshift", a guaranteed zero match against labels {owner, machine}.
//
// Deliberately called from Controller.Spawn ONLY, never from chooseExecutor:
// the binder re-runs selection on stored state, and a row that legitimately
// stored a bare-word selector before this rule existed (an executor that
// really does carry that label key) must keep binding exactly as it did.
// Promotion is a fresh-spawn reading of the caller's intent; stored grants are
// read as written.
func promoteBareExecutorRef(req protocol.SpawnRequest) protocol.SpawnRequest {
	if req.ExecutorRef == "" && isBareExecutorRef(req.ExecutorSelector) {
		req.ExecutorRef, req.ExecutorSelector = req.ExecutorSelector, ""
	}
	return req
}

// executorOwner is the spawning identity's display name and durable id,
// threaded through executor selection so the two can never be confused: Name
// is what admissionLabels stamps into the child's "owner" label (matched by
// an executor's Admits selector), and UserID is the value that becomes
// childstore.Session.OwnerUserID for this spawn — sourced by the caller
// (Controller.Spawn for a fresh spawn, resumeOwnerUserID for a resume) and
// never re-derived here. effectiveExecutorSetFor compares UserID against
// each live executor's own OwnerUserID; Name never enters that comparison.
type executorOwner struct{ Name, UserID string }

// chooseExecutor returns the executor the request's selector admits, computed
// by narrowing the parent's effective set. It is the single place the choice
// is made, so selectExecutor (client) and selectExecutorID (provisioning,
// prompt visibility) can never disagree about which executor won.
func (c *Controller) chooseExecutor(req protocol.SpawnRequest, owner executorOwner) (executors.Executor, error) {
	candidates, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, owner)
	if err != nil {
		return executors.Executor{}, err
	}
	if req.ExecutorRef != "" {
		return c.resolveRef(req, req.ExecutorRef, candidates, nil, "", sel, childLabels, parentSet, owner.UserID)
	}
	// A sandbox is never an implicit choice: with neither a ref nor a selector,
	// the caller has not asked for any particular machine, so a sandbox row must
	// never be the fallback. (sortCandidates already orders sandboxes last, but
	// that only helps when an ordinary executor also survives; a sandbox-only
	// set must still be refused rather than silently spawned into.)
	if !hasExplicitSelector(req) {
		candidates = dropSandboxCandidates(candidates)
	}
	if len(candidates) == 0 {
		return executors.Executor{}, c.explainNoMatch(req, sel, parentSet, childLabels, owner.UserID)
	}
	return candidates[0], nil
}

// narrowedExecutorCandidates computes chooseExecutor's candidate list without
// picking a winner or explaining a failure — chooseLaunchExecutor narrows
// this further by launch kind before doing either, so a change to admission
// or executor-set narrowing can never happen in only one of the two callers.
func (c *Controller) narrowedExecutorCandidates(req protocol.SpawnRequest, owner executorOwner) (
	candidates []executors.Executor, parentSet []executors.Executor, childLabels map[string]string, sel executors.Selector, err error,
) {
	sel, err = executors.ParseSelector(req.ExecutorSelector)
	if err != nil {
		return nil, nil, nil, executors.Selector{}, fmt.Errorf("invalid executor selector %q: %w", req.ExecutorSelector, err)
	}

	// The child does not exist yet, so evaluate the PARENT's set and narrow
	// with the request's selector — which is exactly what
	// effectiveExecutorSet will compute for the child once it is stored.
	childLabels = c.admissionLabels(req, owner)
	parentSet, err = c.effectiveExecutorSetFor(req.ParentChildID, childLabels, owner.UserID)
	if err != nil {
		return nil, nil, nil, executors.Selector{}, err
	}

	// A resolved request carries the child's own set (see
	// resolveSpawnExecutorSet, and the stored row on a resume); nil means not
	// resolved yet and restricts nothing. An empty non-nil set admits nothing.
	if all, ids, _ := childstore.ResolveSetSpec(req.Executors, ""); req.Executors != nil && !all {
		parentSet = filterBySet(parentSet, false, ids, "")
	}

	candidates = executors.Narrow(parentSet, sel)
	candidates = narrowByWorkspaceMode(candidates, req.WorkspaceMode)
	// A child-owned sandbox is never an ordinary candidate: it is reached only
	// through the ownership lookup (controllerBinder.ChooseFor), never by
	// selector, ref or implicit choice. Dropping it HERE covers both choose
	// functions (and launch selection), since both narrow through this method.
	candidates = dropChildOwnedSandboxes(candidates)
	sortCandidates(candidates)
	return candidates, parentSet, childLabels, sel, nil
}

// hasExplicitSelector reports whether req NAMES an executor via a selector that
// says anything at all. The comparison is against the TrimSpace'd value, not
// the raw string: a whitespace-only selector (" ") is not `== ""` yet parses
// to the EMPTY selector, so on the paths that do not run promoteBareExecutorRef
// (stored-selector re-selection via ChooseFor, MCP/Connect paths) a raw `== ""`
// check would skip the implicit-sandbox drop and let a sandbox-only match be
// returned. TrimSpace closes that hole.
func hasExplicitSelector(req protocol.SpawnRequest) bool {
	return strings.TrimSpace(req.ExecutorSelector) != ""
}

// isSandboxRow reports whether an executor's row is a sandbox row, by presence
// of sandbox.RowLabelSandbox. Presence, not value, so a row written with an
// empty value is still treated as a sandbox (fail closed).
func isSandboxRow(e executors.Executor) bool {
	_, ok := e.Labels[sandbox.RowLabelSandbox]
	return ok
}

// isChildOwnedSandboxRow reports whether an executor's row is a child-owned
// sandbox, by presence of sandbox.RowLabelOwnerChild — an empty value counts
// too (fail closed), since the key is only ever written together with a value.
func isChildOwnedSandboxRow(e executors.Executor) bool {
	_, ok := e.Labels[sandbox.RowLabelOwnerChild]
	return ok
}

// dropSandboxCandidates removes every sandbox row from candidates, preserving
// order. Used only when a caller has named neither a ref nor a selector.
func dropSandboxCandidates(candidates []executors.Executor) []executors.Executor {
	kept := candidates[:0:0]
	for _, e := range candidates {
		if isSandboxRow(e) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// dropChildOwnedSandboxes removes every child-owned sandbox row from
// candidates, preserving order.
func dropChildOwnedSandboxes(candidates []executors.Executor) []executors.Executor {
	kept := candidates[:0:0]
	for _, e := range candidates {
		if isChildOwnedSandboxRow(e) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// chooseLaunchExecutor is chooseExecutor's sibling for a kind that must be
// LAUNCHED (currently only "claude") rather than reached as an already-running
// executor's tool surface. It runs the SAME admission/set/workspace-mode
// pipeline chooseExecutor does, then further requires the executor to
// advertise launchKind among the DescribeResponse.LaunchKinds it self-reported
// at connect time. Self-reporting is safe here for the same reason it is safe
// for pkg/connectapi/daraja_handlers.go's DarajaLaunch RPC (and for
// LiveExecutor.Proxies more generally): it only ever NARROWS what the
// executor will accept, so a lying executor can only be chosen from a set its
// admission selector already admitted — never a way to widen it.
func (c *Controller) chooseLaunchExecutor(req protocol.SpawnRequest, owner executorOwner, launchKind string) (executors.Executor, error) {
	candidates, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, owner)
	if err != nil {
		return executors.Executor{}, err
	}
	launchable := launchKindSet(c.execPool.Live(), launchKind)
	var kept []executors.Executor
	for _, e := range candidates {
		if launchable[e.ID] {
			kept = append(kept, e)
		}
	}
	if req.ExecutorRef != "" {
		return c.resolveRef(req, req.ExecutorRef, kept, launchable, launchKind, sel, childLabels, parentSet, owner.UserID)
	}
	// As in chooseExecutor: no ref and no selector means no executor was named,
	// so a sandbox must never be the implicit launch target.
	if !hasExplicitSelector(req) {
		kept = dropSandboxCandidates(kept)
	}
	if len(kept) == 0 {
		return executors.Executor{}, c.explainNoLaunchMatch(req, sel, parentSet, childLabels, launchKind, owner.UserID)
	}
	return kept[0], nil
}

// launchKindSet returns the set of executor IDs whose live Describe
// advertises launchKind among LaunchKinds.
func launchKindSet(live []execpool.LiveExecutor, launchKind string) map[string]bool {
	out := make(map[string]bool)
	for _, le := range live {
		if le.Describe == nil {
			continue
		}
		if slices.Contains(le.Describe.LaunchKinds, launchKind) {
			out[le.Executor.ID] = true
		}
	}
	return out
}

// explainNoLaunchMatch mirrors explainNoMatch, adding "does not support
// launching <kind>" as a fourth exclusion reason alongside disabled/
// admission/workspace-mode/selector.
func (c *Controller) explainNoLaunchMatch(req protocol.SpawnRequest, childSel executors.Selector, parentSet []executors.Executor, childLabels map[string]string, launchKind, ownerUserID string) error {
	launchable := launchKindSet(c.execPool.Live(), launchKind)
	var sb strings.Builder
	fmt.Fprintf(&sb, "spawn refused: no executor can launch a %q child for selector %q.\n", launchKind, req.ExecutorSelector)
	fmt.Fprintf(&sb, "  %d live executor(s), %d in your parent's set:\n", len(c.execPool.Live()), len(parentSet))
	for _, le := range c.execPool.Live() {
		e := le.Executor
		reason := executorReason(e, req, launchable, launchKind, childSel, childLabels, parentSet, ownerUserID)
		fmt.Fprintf(&sb, "    %-12s  %s\n", shortID(e.ID), reason)
	}
	return errors.New(sb.String())
}

// admissionLabels returns the labels an executor's Admits selector should be
// evaluated against for req. For a non-top-level spawn this is the PARENT's
// already-stored labels — a live session already carries its own daemon-
// attested owner (see Spawn), so reading it back from the childstore is
// exactly right there. A top-level spawn has no parent AND no childstore
// entry of its own yet — Spawn mints childID and inserts the session only
// after this selection succeeds — so its labels are built the same way Spawn
// is about to build them: the request's own labels plus the owner the daemon
// is attesting for it. Getting this wrong is not cosmetic: an executor
// enrolled with Admits "owner=<name>" (every session executor — see
// ExecutorSession) would refuse EVERY top-level spawn, since the fallback of
// an empty label map can never match a non-empty Admits selector.
func (c *Controller) admissionLabels(req protocol.SpawnRequest, owner executorOwner) map[string]string {
	if req.ParentChildID != "" {
		if snap, ok := c.st.Get(req.ParentChildID); ok {
			return snap.Labels
		}
		return map[string]string{}
	}
	labels := copyLabels(req.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	if owner.Name != "" {
		labels["owner"] = owner.Name
	}
	return labels
}

// narrowByWorkspaceMode drops executors whose ROW does not offer the requested
// workspace mode. An empty request matches everything.
//
// This is where the mode is enforced. It used to be enforced by Provision, on
// the executor, from a flag the executor was started with — which stopped being
// true when the executor stopped declaring anything about itself. Nothing
// replaced it for one commit, and in that window a child that inherited
// "ephemeral" from a parent confined to disposable machines would land happily
// on a pinned one: the grant widened silently, which is the direction that
// matters.
//
// An executor whose row leaves the mode unset counts as pinned, matching
// workspaceModeOrPinned. It is the reading that offers the child less.
func narrowByWorkspaceMode(candidates []executors.Executor, want string) []executors.Executor {
	if want == "" {
		return candidates
	}
	var kept []executors.Executor
	for _, e := range candidates {
		if workspaceModeOffered(e.WorkspaceMode, want) {
			kept = append(kept, e)
		}
	}
	return kept
}

// workspaceModeOffered reports whether an executor whose row says rowMode may
// serve a request asking for want. An empty want matches every mode, and an
// unset row mode counts as pinned (see narrowByWorkspaceMode).
//
// It is its own predicate because two different paths ask this question:
// narrowByWorkspaceMode (the live pipeline) and the offline installed rows of
// reachableInstalledCandidates. A second spelling of the comparison is how a
// path comes to skip the check altogether.
func workspaceModeOffered(rowMode, want string) bool {
	return want == "" || workspaceModeOrPinned(rowMode) == want
}

// executorAdmitsChild reports whether an executor's OWN row lets this child use
// it at all: it is enabled, it belongs to the child's owner, and its Admits
// selector matches the child's labels.
//
// This is the confinement half of admission, applied by the one narrowing
// pipeline every candidate row runs through (effectiveExecutorSetOver).
//
// It deliberately does not consult the requesting agent's selectors (those are
// executors.Narrow, applied by the callers) or the workspace mode (that is
// workspaceModeOffered): which of those a caller applies is what distinguishes
// rule 1 from rule 2.
func executorAdmitsChild(e executors.Executor, childLabels map[string]string, ownerUserID string) bool {
	if !e.Enabled {
		return false
	}
	// Ownership is checked BEFORE Admits, and in addition to it, never instead
	// of it: an executor's owner_user_id must equal the child's own (empty on
	// both sides counts as equal — an unowned executor serves only unowned
	// children), with no admin exception. Admits can only narrow this further,
	// never widen past it.
	if e.OwnerUserID != ownerUserID {
		return false
	}
	admits, err := executors.ParseSelector(e.Admits)
	if err != nil {
		// A malformed admission selector must EXCLUDE, never admit. An operator
		// typo that silently opens a machine to every child is the failure this
		// ordering exists to prevent.
		return false
	}
	return admits.Matches(childLabels)
}

// effectiveExecutorSet is every live executor childID may use: the owner's
// executors that admit it, intersected with the child's own stored set. A
// top-level agent's set is every live executor that admits it.
//
// An agent-written annotation can help a child FIND an executor it was already
// permitted to use, but cannot manufacture permission.
//
// No caching. The store holds tens of children and the pool holds a handful of
// executors; a cache here would be a second source of truth for an
// authority decision.
func (c *Controller) effectiveExecutorSet(childID string) ([]executors.Executor, error) {
	childLabels := map[string]string{}
	ownerUserID := ""
	if snap, ok := c.st.Get(childID); ok {
		childLabels = snap.Labels
		ownerUserID = snap.OwnerUserID
	}
	return c.effectiveExecutorSetFor(childID, childLabels, ownerUserID)
}

// effectiveExecutorSetFor is effectiveExecutorSet with the admission labels
// supplied by the caller instead of read from the childstore — the case
// chooseExecutor needs for a top-level spawn, whose childstore entry does not
// exist yet (see admissionLabels). ownerUserID is the value that becomes
// childstore.Session.OwnerUserID for the child this set is being computed
// for; effectiveExecutorSet reads it back from the stored snapshot, and
// narrowedExecutorCandidates carries the caller's own executorOwner.UserID
// for a child that has no row yet.
//
// The candidate source is the LIVE pool. effectiveExecutorSetOver is the
// narrowing itself, so a second candidate source (Reachable's offline
// installed rows) can never drift from this one.
func (c *Controller) effectiveExecutorSetFor(childID string, childLabels map[string]string, ownerUserID string) ([]executors.Executor, error) {
	if c.execPool == nil {
		return nil, errors.New("no executor pool is configured (requires RAFIKI_DB; also requires RAFIKI_EXECUTORS_ENABLED=1 when RAFIKI_CONTROL_LISTEN is set)")
	}
	live := c.execPool.Live()
	rows := make([]executors.Executor, 0, len(live))
	for _, le := range live {
		rows = append(rows, le.Executor)
	}
	return c.effectiveExecutorSetOver(rows, childID, childLabels, ownerUserID)
}

// effectiveExecutorSetOver is the whole narrowing, applied to a candidate ROW
// set the caller supplies: the owner and the executor's own admission selector,
// then childID's own executor set.
//
// Ancestors are not consulted: a child's set was fixed to a subset of its
// parent's at spawn, so the parent's reach is already inside it.
//
// It exists as a separate function so Reachable can run the executors installed
// over ssh through EXACTLY the pipeline the live ones get. A second copy of this
// ordering would be a second place for the confinement rule to be wrong.
func (c *Controller) effectiveExecutorSetOver(rows []executors.Executor, childID string, childLabels map[string]string, ownerUserID string) ([]executors.Executor, error) {
	all, ids, selector := c.executorSetSpec(childID)
	return admittedBySet(rows, childLabels, ownerUserID, all, ids, selector), nil
}

// admittedBySet is effectiveExecutorSetOver with the set spec supplied, for the
// one caller whose child has no stored row yet (a fresh spawn's eager bind).
func admittedBySet(rows []executors.Executor, childLabels map[string]string, ownerUserID string, all bool, ids []string, selector string) []executors.Executor {
	// Start from every candidate that ADMITS the child. The executor-side
	// selector is evaluated once, against the child's own labels, because a set
	// alone says what the agent wants, not what the executor will take.
	var admitted []executors.Executor
	for _, e := range rows {
		if executorAdmitsChild(e, childLabels, ownerUserID) {
			admitted = append(admitted, e)
		}
	}
	return filterBySet(admitted, all, ids, selector)
}

// installLookupTimeout bounds the install-table reads the reachable set and the
// home resolver make. Each is one indexed read, and a wedged store must not
// wedge a tool call — the same shape (and reason) as sandboxBindTimeout.
const installLookupTimeout = 3 * time.Second

// installRow returns the LIVE install row for executorID, i.e. the row that says
// this executor was installed over ssh and has not been taken away again.
//
// A store error reads as "no row" and is logged. Every caller asks a NARROWING
// question (may this row add reach? may this executor be a home?), so failing to
// find a row withholds reach or a home rather than granting either.
func (c *Controller) installRow(ctx context.Context, executorID string) (executorinstall.Row, bool) {
	if c.install == nil || c.install.store == nil || executorID == "" {
		return executorinstall.Row{}, false
	}
	row, ok, err := c.install.store.ByExecutor(ctx, executorID)
	if err != nil {
		slog.Warn("could not read an executor's install row; treating it as not installed",
			"executor", shortID(executorID), "error", err)
		return executorinstall.Row{}, false
	}
	return row, ok
}

// isInstalledExecutor reports whether executorID carries a live install row.
//
// It owns a bounded context of its own rather than taking the caller's: the
// callers are a reach computation and a home resolution, and neither may spend
// a spawn's whole budget on one store read.
func (c *Controller) isInstalledExecutor(executorID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), installLookupTimeout)
	defer cancel()
	_, ok := c.installRow(ctx, executorID)
	return ok
}

// executorRowByID reads an executor's DURABLE row, live or not.
//
// c.executorRow scans the live pool and cannot answer for an executor that is
// down, which is exactly the row the installed-executor reach needs.
func (c *Controller) executorRowByID(ctx context.Context, executorID string) (executors.Executor, bool) {
	if c.execStore == nil || executorID == "" {
		return executors.Executor{}, false
	}
	e, err := c.execStore.Get(ctx, executorID)
	if err != nil {
		if !errors.Is(err, executors.ErrNotFound) {
			slog.Warn("could not read an executor's row for the reachable set",
				"executor", shortID(executorID), "error", err)
		}
		return executors.Executor{}, false
	}
	return e, true
}

// offlineInstalledRows is the owner's installed executors that are not live,
// read from their durable rows. A read failure adds NOTHING and is logged: this
// only ever WIDENS reach, so a failure must never widen it.
func (c *Controller) offlineInstalledRows(owner executorOwner) []executors.Executor {
	if c.install == nil || c.install.store == nil || c.execStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), installLookupTimeout)
	defer cancel()

	// The owner's OWN rows, never every owner's: an install is caller-bound, and
	// ListLive("") means the ownerless rows, not all of them.
	rows, err := c.install.store.ListLive(ctx, owner.UserID)
	if err != nil {
		slog.Warn("could not list installed executors; only live ones are reachable",
			"owner", owner.Name, "error", err)
		return nil
	}
	live := map[string]bool{}
	if c.execPool != nil {
		for _, le := range c.execPool.Live() {
			live[le.Executor.ID] = true
		}
	}
	var offline []executors.Executor
	for _, row := range rows {
		if live[row.ExecutorID] {
			continue
		}
		if e, ok := c.executorRowByID(ctx, row.ExecutorID); ok {
			offline = append(offline, e)
		}
	}
	return offline
}

// reachableInstalledCandidates is the part of an agent's reachable set the live
// pool cannot answer for: installed executors that are not live. A down machine
// stays a name the agent can call when it is a member of the child's OWN
// executor set and the row admits the child; the call relaunches it
// (controllerBinder.EnsureConnected). They go through the same narrowing the live
// rows get (owner, admission selector, the child's set), then the workspace mode
// and the child-owned-sandbox drop.
//
// Ordering is the caller's (sortCandidates over the combined slice).
func (c *Controller) reachableInstalledCandidates(
	req protocol.SpawnRequest, childID string, childLabels map[string]string, owner executorOwner,
) []executors.Executor {
	offline := c.offlineInstalledRows(owner)
	if len(offline) == 0 {
		return nil
	}
	all, ids, selector := c.boundSetSpec(req, childID)
	narrowed := admittedBySet(offline, childLabels, owner.UserID, all, ids, selector)
	narrowed = narrowByWorkspaceMode(narrowed, req.WorkspaceMode)
	return dropChildOwnedSandboxes(narrowed)
}

// appendExecutorRows appends the rows whose id the set does not already hold,
// preserving both orders.
//
// A duplicate would make one reachable machine look like two and turn every
// implicit call on that host into "pass executor".
func appendExecutorRows(set []executors.Executor, rows ...executors.Executor) []executors.Executor {
	have := make(map[string]bool, len(set)+len(rows))
	for _, e := range set {
		have[e.ID] = true
	}
	for _, e := range rows {
		if have[e.ID] {
			continue
		}
		have[e.ID] = true
		set = append(set, e)
	}
	return set
}

// inheritExecutorGrant fills in the workspace mode a request left silent, from
// the SPAWNER's stored record. Only the workspace mode is inherited; the
// executor set is resolved by resolveSpawnExecutorSet.
//
// Read from the store, never from the request: a caller that could name its own
// parent's grant could widen it. An inherited mode no live executor's ROW offers
// excludes every candidate in narrowByWorkspaceMode, refusing the spawn, which
// is the safe direction.
func (c *Controller) inheritExecutorGrant(req protocol.SpawnRequest) protocol.SpawnRequest {
	if req.ParentChildID == "" {
		return req
	}
	parent, ok := c.st.Get(req.ParentChildID)
	if !ok {
		// An unknown parent is refused by computeLineageLabels and
		// checkSpawnLimits; inheriting nothing here is not the enforcement.
		return req
	}
	if req.WorkspaceMode == "" {
		req.WorkspaceMode = parent.WorkspaceMode
	}
	return req
}

// lineageChain returns the stored executor selectors from the root down to
// childID inclusive.
func (c *Controller) lineageChain(childID string) ([]string, error) {
	if childID == "" {
		return nil, nil // top-level: no selector inherited
	}
	var reversed []string
	cur := childID
	for range maxLineageWalk {
		snap, ok := c.st.Get(cur)
		if !ok {
			break
		}
		reversed = append(reversed, snap.ExecutorSelector)
		parent, ok := c.st.ParentOf(cur)
		if !ok {
			slices.Reverse(reversed)
			return reversed, nil
		}
		cur = parent
	}
	return nil, fmt.Errorf("lineage chain for %s exceeds %d links", childID, maxLineageWalk)
}

// maxLineageWalk mirrors childstore's maxChainDepth: lineage labels are
// daemon-written and cannot legitimately cycle, but a corrupt record must not
// wedge the spawn path.
const maxLineageWalk = 64

// executorReason explains, for one live executor, why chooseExecutor (when
// launchKind == "") or chooseLaunchExecutor (when launchKind names the
// required kind) would exclude it from req — or "" when it would NOT be
// excluded. This is the SAME reasoning explainNoMatch and explainNoLaunchMatch
// built inline per call site, extracted so ListExecutorRows' eligible/reason
// fields (Task 3) can never disagree with what a real spawn attempt would
// decide.
//
// The two branches are NOT the same shape and must stay that way: the launch
// path's admission-exclusion message carries no child-labels suffix and has
// no separate set-membership message in its admission branch (an existing,
// preserved asymmetry — not something this refactor corrects), and the
// non-launch path checks workspace_mode, which the launch path never has.
//
// ownerUserID is the spawning identity's durable id — the same value
// effectiveExecutorSetFor compared against e.OwnerUserID, which is why the
// owner branch runs before the admission one in BOTH branches here too: an
// executor owned by someone else is excluded by the ownership rule before
// its Admits selector is ever consulted. The message deliberately names no
// other user.
func executorReason(
	e executors.Executor,
	req protocol.SpawnRequest,
	launchable map[string]bool,
	launchKind string,
	childSel executors.Selector,
	childLabels map[string]string,
	parentSet []executors.Executor,
	ownerUserID string,
) string {
	if launchKind != "" {
		switch {
		case !e.Enabled:
			return "disabled"
		case e.OwnerUserID != ownerUserID:
			return "belongs to another user"
		case !launchable[e.ID]:
			return fmt.Sprintf("does not support launching %q children", launchKind)
		}
		admitsSel, aerr := executors.ParseSelector(e.Admits)
		switch {
		case aerr != nil:
			return fmt.Sprintf("unparseable admission selector %q", e.Admits)
		case !admitsSel.Matches(childLabels):
			return fmt.Sprintf("excluded by ITS admission selector %q", e.Admits)
		}
		// parentSet already reflects the executor set, which the three checks
		// above do not: they only re-check e's OWN admits selector against
		// childLabels. An executor outside the set passes
		// enabled/launchable/admits/childSel.Explain fine and must still be
		// caught here, or it reads as eligible when it is not.
		inParentSet := false
		for _, p := range parentSet {
			if p.ID == e.ID {
				inParentSet = true
				break
			}
		}
		if !inParentSet {
			// The only case explainNoLaunchMatch's loop can ever actually reach:
			// kept-empty means no live executor is truly eligible, so none can be
			// both in the set and unexcluded by the child's own selector.
			return "not in your executor set"
		}
		if explain := childSel.Explain(e.Labels); explain != "" {
			return fmt.Sprintf("excluded by your selector:   %s", explain)
		}
		// Enabled, launch-capable, admits this child, in the executor set, and
		// the child's own selector does not exclude it: nothing here excludes e. This branch was unreachable
		// from explainNoLaunchMatch's original call site (it only enumerates
		// AFTER the overall attempt already failed, so no live executor
		// could reach here with an empty explain), but ListExecutorRows
		// calls executorReason per-executor regardless of whether the
		// OVERALL attempt would succeed, so this path is very much reachable
		// now and must honor the "" contract above.
		return ""
	}

	if !e.Enabled {
		return "disabled"
	}
	if e.OwnerUserID != ownerUserID {
		return "belongs to another user"
	}
	admitsSel, aerr := executors.ParseSelector(e.Admits)
	if aerr != nil {
		return fmt.Sprintf("unparseable admission selector %q", e.Admits)
	}
	if !admitsSel.Matches(childLabels) {
		return fmt.Sprintf("excluded by ITS admission selector %q: this child is %v", e.Admits, childLabels)
	}
	if req.WorkspaceMode != "" && workspaceModeOrPinned(e.WorkspaceMode) != req.WorkspaceMode {
		return fmt.Sprintf("offers workspace_mode=%s; you asked for %s",
			workspaceModeOrPinned(e.WorkspaceMode), req.WorkspaceMode)
	}
	for _, p := range parentSet {
		if p.ID == e.ID {
			if explain := childSel.Explain(e.Labels); explain != "" {
				return fmt.Sprintf("excluded by your selector:   %s", explain)
			}
			// e is in the parent's set (already enabled, admitting, and
			// workspace-mode-compatible) and the child's own selector does
			// not exclude it either: e would be in `candidates`. See the
			// launch branch's matching comment above for why this was dead
			// code from explainNoMatch alone but is live from
			// ListExecutorRows.
			return ""
		}
	}
	return "not in your executor set"
}

// explainNoMatch builds a refusal message naming the excluding predicate per
// candidate, so the reason is legible to the caller.
//
// Five exclusion reasons are distinguished:
//   - The executor is owned by a different user (the ownership rule — the
//     message never names the other user).
//   - The child's own selector excludes the executor (the parent's set would
//     have allowed it).
//   - The parent's set excludes the executor (the child never got to evaluate
//     it).
//   - The executor's admission selector refused the child.
//   - The executor's row does not offer the requested workspace mode.
//
// childLabels is the same admission label set chooseExecutor computed via
// admissionLabels — passed in rather than recomputed here so a top-level
// spawn's refusal names the labels it was ACTUALLY evaluated against (its own
// request labels plus the attested owner), not an empty childstore lookup
// that no longer reflects what chooseExecutor used.
func (c *Controller) explainNoMatch(req protocol.SpawnRequest, childSel executors.Selector, parentSet []executors.Executor, childLabels map[string]string, ownerUserID string) error {
	var sb strings.Builder
	if req.WorkspaceMode != "" {
		fmt.Fprintf(&sb, "spawn refused: no executor satisfies %q with workspace_mode=%s.\n",
			req.ExecutorSelector, req.WorkspaceMode)
	} else {
		fmt.Fprintf(&sb, "spawn refused: no executor satisfies %q.\n", req.ExecutorSelector)
	}
	fmt.Fprintf(&sb, "  %d live executor(s), %d in your parent's set:\n",
		len(c.execPool.Live()), len(parentSet))

	// Report each live executor with the reason it was excluded.
	for _, le := range c.execPool.Live() {
		e := le.Executor
		reason := executorReason(e, req, nil, "", childSel, childLabels, parentSet, ownerUserID)
		fmt.Fprintf(&sb, "    %-12s  %s\n", shortID(e.ID), reason)
	}
	return fmt.Errorf("%s", sb.String())
}

// resolveRef finds ref among the already-narrowed eligible set and, on a
// miss, explains WHY using the same per-row reasoning explainNoMatch/
// explainNoLaunchMatch use — distinguishing "no such executor" from "it
// exists but is not usable for this spawn" by checking every live executor,
// not just the eligible ones.
func (c *Controller) resolveRef(
	req protocol.SpawnRequest,
	ref string,
	eligible []executors.Executor,
	launchable map[string]bool,
	launchKind string,
	sel executors.Selector,
	childLabels map[string]string,
	parentSet []executors.Executor,
	ownerUserID string,
) (executors.Executor, error) {
	if e, ok := matchExecutorRef(ref, eligible); ok {
		return e, nil
	}
	for _, le := range c.execPool.Live() {
		e := le.Executor
		if e.Labels["machine"] != ref && e.ID != ref {
			continue
		}
		reason := executorReason(e, req, launchable, launchKind, sel, childLabels, parentSet, ownerUserID)
		if reason == "" {
			reason = "excluded for an unknown reason" // unreachable in practice: a match with no reason would already be in `eligible`
		}
		return executors.Executor{}, fmt.Errorf("executor %q exists but is not usable for this spawn: %s", ref, reason)
	}
	return executors.Executor{}, fmt.Errorf("no executor named %q (checked machine label and id)", ref)
}

// matchExecutorRef finds the executor in candidates whose machine label or ID
// equals ref. Machine label is checked FIRST and is an exact match by
// construction (executors_owner_machine_unique), so it can never itself be
// ambiguous the way a raw id fragment could be — this deliberately does NOT
// support id-suffix matching the way the management-command resolver
// (Controller.resolveExecutorRef) does; a spawn-time ref is typed once and
// tab-completed, so the forgiving-fragment UX that resolver exists for does
// not apply here.
func matchExecutorRef(ref string, candidates []executors.Executor) (executors.Executor, bool) {
	for _, e := range candidates {
		if e.Labels["machine"] == ref {
			return e, true
		}
	}
	for _, e := range candidates {
		if e.ID == ref {
			return e, true
		}
	}
	return executors.Executor{}, false
}

// shortID truncates an executor id for display without panicking on short ids
// (test fixtures use human-readable names, not ULIDs).
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// sortCandidates imposes a deterministic winner on a candidate set.
//
// chooseExecutor returns candidates[0], and the set is built by ranging
// Pool.Live(), a Go map — so without this the winner is randomized per call.
// That was latent while selectors matched exactly one executor and becomes
// load-bearing the moment a machine offers both a durable and a session
// executor.
//
// Durable before session: a child outlives the terminal that started it, so it
// should bind to something that also does. The session executor is a
// zero-config fallback for a machine with no durable one, not the preferred
// home.
//
// Sandbox last: a sandbox row is an explicit, narrowly-scoped machine, so a
// BROAD selector (owner=x) must prefer an ordinary executor over a sandbox even
// though both match. This orders sandboxes after every non-sandbox row; among
// sandboxes (and among non-sandboxes) the durable-before-session, then-ID order
// still applies. It does not exempt a sandbox named by ref or selector, which
// still matches in matchExecutorRef/the selector; it only decides the winner
// when a selector is broad.
func sortCandidates(in []executors.Executor) {
	sort.SliceStable(in, func(i, j int) bool {
		sbi := isSandboxRow(in[i])
		sbj := isSandboxRow(in[j])
		if sbi != sbj {
			return !sbi
		}
		si := in[i].Labels["kind"] == "session"
		sj := in[j].Labels["kind"] == "session"
		if si != sj {
			return !si
		}
		return in[i].ID < in[j].ID
	})
}
