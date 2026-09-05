package main

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorPool is the slice of *execpool.Pool the controller uses. An
// interface so selection is testable without a listener, a database, or a
// dialling executor — the reason this whole path shipped untested.
type executorPool interface {
	Live() []execpool.LiveExecutor
	ClientFor(executorID string) (tools.ExecutorClient, error)
	// Tickets and Evict serve transient executors: mint a one-shot ticket,
	// and take the executor down when the control connection that owns it
	// closes. On the interface rather than reached by type assertion so the
	// selection tests' fake pool can satisfy them.
	Tickets() *execpool.TicketRegistry
	Evict(executorID string)
}

// chooseExecutor returns the executor the request's selector admits, computed
// by narrowing the parent's effective set. It is the single place the choice
// is made, so selectExecutor (client) and selectExecutorID (provisioning,
// prompt visibility) can never disagree about which executor won.
func (c *Controller) chooseExecutor(req protocol.SpawnRequest, ownerName string) (executors.Executor, error) {
	candidates, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, ownerName)
	if err != nil {
		return executors.Executor{}, err
	}
	if len(candidates) == 0 {
		return executors.Executor{}, c.explainNoMatch(req, sel, parentSet, childLabels)
	}
	return candidates[0], nil
}

// narrowedExecutorCandidates computes chooseExecutor's candidate list without
// picking a winner or explaining a failure — chooseLaunchExecutor narrows
// this further by launch kind before doing either, so a change to admission
// or lineage narrowing can never happen in only one of the two callers.
func (c *Controller) narrowedExecutorCandidates(req protocol.SpawnRequest, ownerName string) (
	candidates []executors.Executor, parentSet []executors.Executor, childLabels map[string]string, sel executors.Selector, err error,
) {
	sel, err = executors.ParseSelector(req.ExecutorSelector)
	if err != nil {
		return nil, nil, nil, executors.Selector{}, fmt.Errorf("invalid executor selector %q: %w", req.ExecutorSelector, err)
	}

	// The child does not exist yet, so evaluate the PARENT's set and narrow
	// with the request's selector — which is exactly what
	// effectiveExecutorSet will compute for the child once it is stored.
	childLabels = c.admissionLabels(req, ownerName)
	parentSet, err = c.effectiveExecutorSetFor(req.ParentChildID, childLabels)
	if err != nil {
		return nil, nil, nil, executors.Selector{}, err
	}

	candidates = executors.Narrow(parentSet, sel)
	candidates = narrowByWorkspaceMode(candidates, req.WorkspaceMode)
	sortCandidates(candidates)
	return candidates, parentSet, childLabels, sel, nil
}

// chooseLaunchExecutor is chooseExecutor's sibling for a kind that must be
// LAUNCHED (currently only "claude") rather than reached as an already-running
// executor's tool surface. It runs the SAME admission/lineage/workspace-mode
// pipeline chooseExecutor does, then further requires the executor to
// advertise launchKind among the DescribeResponse.LaunchKinds it self-reported
// at connect time. Self-reporting is safe here for the same reason it is safe
// for pkg/connectapi/daraja_handlers.go's DarajaLaunch RPC (and for
// LiveExecutor.Proxies more generally): it only ever NARROWS what the
// executor will accept, so a lying executor can only be chosen from a set its
// admission selector already admitted — never a way to widen it.
func (c *Controller) chooseLaunchExecutor(req protocol.SpawnRequest, ownerName, launchKind string) (executors.Executor, error) {
	candidates, parentSet, childLabels, sel, err := c.narrowedExecutorCandidates(req, ownerName)
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
	if len(kept) == 0 {
		return executors.Executor{}, c.explainNoLaunchMatch(req, sel, parentSet, childLabels, launchKind)
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
func (c *Controller) explainNoLaunchMatch(req protocol.SpawnRequest, childSel executors.Selector, parentSet []executors.Executor, childLabels map[string]string, launchKind string) error {
	launchable := launchKindSet(c.execPool.Live(), launchKind)
	var sb strings.Builder
	fmt.Fprintf(&sb, "spawn refused: no executor can launch a %q child for selector %q.\n", launchKind, req.ExecutorSelector)
	fmt.Fprintf(&sb, "  %d live executor(s), %d in your parent's set:\n", len(c.execPool.Live()), len(parentSet))
	for _, le := range c.execPool.Live() {
		e := le.Executor
		var reason string
		switch {
		case !e.Enabled:
			reason = "disabled"
		case !launchable[e.ID]:
			reason = fmt.Sprintf("does not support launching %q children", launchKind)
		default:
			if admitsSel, aerr := executors.ParseSelector(e.Admits); aerr != nil {
				reason = fmt.Sprintf("unparseable admission selector %q", e.Admits)
			} else if !admitsSel.Matches(childLabels) {
				reason = fmt.Sprintf("excluded by ITS admission selector %q", e.Admits)
			} else if explain := childSel.Explain(e.Labels); explain != "" {
				reason = fmt.Sprintf("excluded by your selector:   %s", explain)
			} else {
				reason = "excluded by your selector or your parent's set"
			}
		}
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
func (c *Controller) admissionLabels(req protocol.SpawnRequest, ownerName string) map[string]string {
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
	if ownerName != "" {
		labels["owner"] = ownerName
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
		if workspaceModeOrPinned(e.WorkspaceMode) == want {
			kept = append(kept, e)
		}
	}
	return kept
}

// effectiveExecutorSet is every live executor childID may use.
//
// Computed by walking UP the lineage: a top-level agent's set is every live
// executor that admits it; a child's set is its PARENT'S set intersected with
// its own stored selector. Never computed by trying to prove a child's
// selector implies its parent's — that is decidable for equality matches and a
// logic puzzle the moment notin appears, and getting it wrong fails OPEN,
// which is the wrong direction for a confidentiality boundary.
//
// Intersection is also what makes agent-written annotations safe: an
// annotation can help a child FIND an executor it was already permitted to
// use, but cannot manufacture permission.
//
// No caching. The store holds tens of children and the pool holds a handful of
// executors; a cache here would be a second source of truth for an
// authority decision.
func (c *Controller) effectiveExecutorSet(childID string) ([]executors.Executor, error) {
	childLabels := map[string]string{}
	if snap, ok := c.st.Get(childID); ok {
		childLabels = snap.Labels
	}
	return c.effectiveExecutorSetFor(childID, childLabels)
}

// effectiveExecutorSetFor is effectiveExecutorSet with the admission labels
// supplied by the caller instead of read from the childstore — the case
// chooseExecutor needs for a top-level spawn, whose childstore entry does not
// exist yet (see admissionLabels).
func (c *Controller) effectiveExecutorSetFor(childID string, childLabels map[string]string) ([]executors.Executor, error) {
	if c.execPool == nil {
		return nil, errors.New("no executor pool is configured (requires RAFIKI_DB; also requires RAFIKI_EXECUTORS_ENABLED=1 when RAFIKI_CONTROL_LISTEN is set)")
	}

	// Start from every live executor that ADMITS the child. The executor-side
	// selector is evaluated once, against the child's own labels, because an
	// agent-side selector alone is permissive by default: it says what the
	// agent wants, not what the executor will take.
	var set []executors.Executor
	for _, le := range c.execPool.Live() {
		if !le.Executor.Enabled {
			continue
		}
		admits, err := executors.ParseSelector(le.Executor.Admits)
		if err != nil {
			// A malformed admission selector must EXCLUDE, never admit. An
			// operator typo that silently opens a machine to every child is
			// the failure this ordering exists to prevent.
			continue
		}
		if !admits.Matches(childLabels) {
			continue
		}
		set = append(set, le.Executor)
	}

	// Then narrow by every selector from the root down to and including this
	// child. Root-first order is not cosmetic: it means an ancestor's
	// constraint can never be widened by a descendant's.
	chain, err := c.lineageChain(childID)
	if err != nil {
		return nil, err
	}
	for _, ancestorSelector := range chain {
		if ancestorSelector == "" {
			continue
		}
		sel, err := executors.ParseSelector(ancestorSelector)
		if err != nil {
			return nil, fmt.Errorf("stored executor selector %q is unparseable: %w", ancestorSelector, err)
		}
		set = executors.Narrow(set, sel)
	}
	return set, nil
}

// inheritExecutorGrant fills in an executor grant the request left silent,
// from the SPAWNER's stored record.
//
// An omitted selector used to mean "no executor", which made it a confinement
// escape rather than a default: resolveExecutor returned (nil, nil), so the
// child's tools ran IN THE DAEMON PROCESS on the daemon host — outside every
// executor its parent was restricted to. Worse, the escape compounded: the
// child stored "" as its own selector, so lineageChain skipped it and its
// entire subtree inherited the way out.
//
// Inheritance is the right default rather than a refusal. It is what
// agent_spawn's tool description already promises the model ("omit to use the
// same machines you can"), it keeps every existing top-level agent working,
// and it composes: the child's stored selector is now non-empty, so ITS
// children inherit in turn and the subtree cannot leak out a generation later.
//
// Read from the store, never from the request. That is the same rule the
// limits checks follow, and for the same reason: a request is written by the
// caller, and a caller that could name its own parent's grant could widen it.
//
// The two fields are independent. A parent confined to ephemeral workspaces
// whose child inherits the selector but silently defaults to "pinned" has been
// handed a wider grant than its parent's through the back door; and an
// inherited mode no live executor's ROW offers excludes every candidate in
// narrowByWorkspaceMode, refusing the spawn, which is the safe direction.
//
// A top-level spawn (no parent) has nothing to inherit and keeps today's
// behaviour: no selector, tools in-process.
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
	if req.ExecutorSelector == "" {
		req.ExecutorSelector = parent.ExecutorSelector
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

// explainNoMatch builds a refusal message naming the excluding predicate per
// candidate, so the reason is legible to the caller.
//
// Four exclusion reasons are distinguished:
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
func (c *Controller) explainNoMatch(req protocol.SpawnRequest, childSel executors.Selector, parentSet []executors.Executor, childLabels map[string]string) error {
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
		reason := ""
		if !e.Enabled {
			reason = "disabled"
		} else if admitsSel, err := executors.ParseSelector(e.Admits); err != nil {
			reason = fmt.Sprintf("unparseable admission selector %q", e.Admits)
		} else {
			if !admitsSel.Matches(childLabels) {
				reason = fmt.Sprintf("excluded by ITS admission selector %q: this child is %v", e.Admits, childLabels)
			} else if req.WorkspaceMode != "" && workspaceModeOrPinned(e.WorkspaceMode) != req.WorkspaceMode {
				reason = fmt.Sprintf("offers workspace_mode=%s; you asked for %s",
					workspaceModeOrPinned(e.WorkspaceMode), req.WorkspaceMode)
			} else {
				inParentSet := false
				for _, p := range parentSet {
					if p.ID == e.ID {
						inParentSet = true
						break
					}
				}
				if !inParentSet {
					reason = "excluded by your PARENT's set"
				} else {
					explain := childSel.Explain(e.Labels)
					if explain != "" {
						reason = fmt.Sprintf("excluded by your selector:   %s", explain)
					} else {
						reason = "excluded by your selector"
					}
				}
			}
		}
		fmt.Fprintf(&sb, "    %-12s  %s\n", shortID(e.ID), reason)
	}
	return fmt.Errorf("%s", sb.String())
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
func sortCandidates(in []executors.Executor) {
	sort.SliceStable(in, func(i, j int) bool {
		si := in[i].Labels["kind"] == "session"
		sj := in[j].Labels["kind"] == "session"
		if si != sj {
			return !si
		}
		return in[i].ID < in[j].ID
	})
}
