package main

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorPool is the slice of *execpool.Pool the controller uses. An
// interface so selection is testable without a listener, a database, or a
// dialling executor — the reason this whole path shipped untested.
type executorPool interface {
	Live() []execpool.LiveExecutor
	ClientFor(executorID string) (execpool.ExecutorClient, error)
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
	if c.execPool == nil {
		return nil, errors.New("no executor listener is configured (set RAFIKI_EXECUTOR_LISTEN)")
	}
	chain, err := c.lineageChain(childID)
	if err != nil {
		return nil, err
	}

	// Start from every live executor that ADMITS the child. The executor-side
	// selector is evaluated once, against the child's own labels, because an
	// agent-side selector alone is permissive by default: it says what the
	// agent wants, not what the executor will take.
	childLabels := map[string]string{}
	if snap, ok := c.st.Get(childID); ok {
		childLabels = snap.Labels
	}
	var set []executors.Executor
	for _, le := range c.execPool.Live() {
		if !le.Enabled {
			continue
		}
		admits, err := executors.ParseSelector(le.Admits)
		if err != nil {
			slog.Warn("execpool: executor has an unparseable admission selector; excluding it",
				"executorId", le.ID, "admits", le.Admits, "error", err)
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

// lineageChain returns the stored executor selectors from the root down to
// childID inclusive.
func (c *Controller) lineageChain(childID string) ([]string, error) {
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

// selectExecutor resolves which executor a child runs on.
//
// When no pool is configured, returns nil, nil — the default of running
// everything in-process. When a pool is configured, evaluates the child's
// selector against the parent's effective set (intersection).
func (c *Controller) selectExecutor(req protocol.SpawnRequest) (execpool.ExecutorClient, error) {
	if c.execPool == nil || req.ExecutorSelector == "" {
		return nil, nil
	}
	sel, err := executors.ParseSelector(req.ExecutorSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid executor selector %q: %w", req.ExecutorSelector, err)
	}
	// The child does not exist yet, so evaluate the PARENT's set and narrow
	// with the request's selector — which is exactly what
	// effectiveExecutorSet will compute for the child once it is stored.
	parentSet, err := c.effectiveExecutorSet(req.ParentChildID)
	if err != nil {
		return nil, err
	}
	candidates := executors.Narrow(parentSet, sel)
	if len(candidates) == 0 {
		return nil, c.explainNoMatch(req, sel, parentSet)
	}
	chosen := candidates[0]
	cl, err := c.execPool.ClientFor(chosen.ID)
	if err != nil {
		return nil, fmt.Errorf("executor %s selected but not reachable: %w", chosen.ID, err)
	}
	return cl, nil
}

// explainNoMatch builds a diagnostic refusal naming every exclusion reason
// per live executor, so a model or operator can distinguish a typo from a
// missing label from an executor that refused the child.
func (c *Controller) explainNoMatch(req protocol.SpawnRequest, sel executors.Selector, parentSet []executors.Executor) error {
	var b strings.Builder
	reqText := req.ExecutorSelector
	if reqText == "" {
		reqText = "(none)"
	}
	fmt.Fprintf(&b, "spawn refused: no executor satisfies `%s`.\n", reqText)
	fmt.Fprintf(&b, "  %d live executor(s),", len(parentSet))
	if req.ParentChildID != "" {
		fmt.Fprintf(&b, " %d in your parent's set:", len(parentSet))
	} else {
		fmt.Fprintf(&b, " %d passing admission:", len(parentSet))
	}
	fmt.Fprintln(&b)

	for _, ex := range parentSet {
		reason := sel.Explain(ex.Labels)
		if reason != "" {
			fmt.Fprintf(&b, "    %-12s excluded by your selector:   %s\n", ex.ID, reason)
		} else {
			// It matched the child's selector but was already filtered out —
			// hence, the parent's set is the constraint.
			fmt.Fprintf(&b, "    %-12s excluded by your PARENT's set\n", ex.ID)
		}
	}

	// Also report live executors that were excluded by ADMISSION (not in parentSet).
	// This is best-effort and helps diagnose executor-side refusals.
	if c.execPool != nil {
		seen := make(map[string]bool)
		for _, ex := range parentSet {
			seen[ex.ID] = true
		}
		hasAdmissionRefusals := false
		for _, le := range c.execPool.Live() {
			if seen[le.ID] || !le.Enabled {
				continue
			}
			admits, err := executors.ParseSelector(le.Admits)
			if err != nil {
				continue
			}
			if !admits.Matches(nil) { // simplified check
				hasAdmissionRefusals = true
				fmt.Fprintf(&b, "    %-12s excluded by ITS admission selector `%s`\n",
					le.ID, le.Admits)
			}
		}
		_ = hasAdmissionRefusals
	}

	return errors.New(b.String())
}
