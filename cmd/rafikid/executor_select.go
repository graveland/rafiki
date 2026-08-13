package main

import (
	"errors"
	"fmt"
	"slices"
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
}

// selectExecutor picks an executor from the live pool based on the request's
// label selector. The refusal path is as important as the success path: a
// spawn whose grant no live executor satisfies fails IMMEDIATELY, naming what
// was required, what was live, and which predicate excluded each candidate.
//
// Selection is two-sided: the child's selector narrows the PARENT's effective
// executor set — never Live() directly. A child can never reach an executor
// its parent could not.
func (c *Controller) selectExecutor(req protocol.SpawnRequest) (tools.ExecutorClient, error) {
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
		return nil, fmt.Errorf("executor %s selected but not reachable: %w", chosen.ID[:12], err)
	}
	return cl, nil
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
// Three exclusion reasons are distinguished:
//   - The child's own selector excludes the executor (the parent's set would
//     have allowed it).
//   - The parent's set excludes the executor (the child never got to evaluate
//     it).
//   - The executor's admission selector refused the child.
func (c *Controller) explainNoMatch(req protocol.SpawnRequest, childSel executors.Selector, parentSet []executors.Executor) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "spawn refused: no executor satisfies %q.\n", req.ExecutorSelector)
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
			childLabels := map[string]string{}
			if snap, ok := c.st.Get(req.ParentChildID); ok {
				childLabels = snap.Labels
			}
			if !admitsSel.Matches(childLabels) {
				reason = fmt.Sprintf("excluded by ITS admission selector %q: this child is %v", e.Admits, childLabels)
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
