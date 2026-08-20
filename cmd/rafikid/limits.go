package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

const (
	// defaultSpawnDepth is what a child gets when the spawner names nothing:
	// enough to delegate once, not enough to build a tree by accident.
	defaultSpawnDepth = 1
	// defaultMaxChildren bounds live descendants across a subtree.
	defaultMaxChildren = 4
	// defaultAbsoluteDepthCeiling is RAFIKI_MAX_DEPTH's fallback.
	defaultAbsoluteDepthCeiling = 3
)

// resolveAbsoluteDepthCeiling reads RAFIKI_MAX_DEPTH.
//
// An explicit "0" is honoured — it disables spawning entirely, which is a
// legitimate deployment posture — so this cannot use the usual "zero means
// default" shortcut. Only an absent or unparseable value falls back.
func resolveAbsoluteDepthCeiling() int {
	s := paths.Get(paths.MaxDepth)
	if s == "" {
		return defaultAbsoluteDepthCeiling
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return defaultAbsoluteDepthCeiling
	}
	return n
}

// limitError builds the refusal every check returns. Typed as
// ErrInvalidArgs so a client sees a request problem, not a daemon fault, and
// worded so the model that triggered it can act: what was asked, what the
// limit is, and which limit it was.
func limitError(format string, args ...any) error {
	return &control.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: fmt.Sprintf(format, args...),
	}
}

// checkSpawnLimits is the single admission gate for phase 05.
//
// EVERY value it consults comes from the store or the environment. req is read
// only for what is being ASKED for, never for what is permitted — that is the
// whole distinction between a boundary and UX. Since fundi children run
// in-process (pkg/inproc), "tool-side" and "daemon-side" share an address
// space, so the rule is not about where this runs but about what it reads.
//
// It runs before anything is registered, which is what lets phase 04 assign a
// task after admission with no compensating write on refusal.
func (c *Controller) checkSpawnLimits(req protocol.SpawnRequest) error {
	if err := c.checkDepth(req); err != nil {
		return err
	}
	if err := c.checkConcurrency(req); err != nil {
		return err
	}
	return c.checkBudget(req)
}

// checkDepth enforces two independent rules:
//
//  1. the SPAWNER's stored grant — may it spawn at all;
//  2. RAFIKI_MAX_DEPTH — an absolute ceiling on where the child would land.
//
// Rule 2 is not rule 1 with different arithmetic. A coordinator's grant is
// local and intuitive ("my workers may also spawn reviewers"); the ceiling is
// a property of the tree the coordinator should not have to think about.
func (c *Controller) checkDepth(req protocol.SpawnRequest) error {
	ceiling := resolveAbsoluteDepthCeiling()

	// Where would the child land?
	childDepth := 0
	if req.ParentChildID != "" {
		parent, ok := c.st.Get(req.ParentChildID)
		if !ok {
			return limitError("parentChildId: no such child: %s", req.ParentChildID)
		}
		// Rule 1: the spawner's own STORED grant. req.MaxDepth is what is
		// being asked FOR THE CHILD and is irrelevant here — a request
		// claiming depth 99 does not raise the asker's own allowance.
		if parent.MaxDepth <= 0 {
			return limitError(
				"agent %s was granted depth 0 and may not spawn. Ask whoever spawned you for a higher --max-depth if this work genuinely needs a subagent",
				req.ParentChildID)
		}
		abs := c.st.AbsoluteDepth(req.ParentChildID)
		if abs < 0 {
			return limitError("parentChildId: no such child: %s", req.ParentChildID)
		}
		childDepth = abs + 1
	}

	// Rule 2: the absolute ceiling, checked against the child's real position
	// in the tree — not against any number the parent supplied.
	if childDepth > ceiling {
		return limitError(
			"spawn refused: the new agent would sit at depth %d, past the RAFIKI_MAX_DEPTH ceiling of %d. This bound is absolute and is not affected by the depth its parent granted",
			childDepth, ceiling)
	}
	return nil
}

// grantedDepth resolves what the NEW child gets. It is bounded by the ceiling
// too, so a stored grant can never describe a subtree the ceiling forbids —
// otherwise the refusal would arrive one generation late and be confusing.
func grantedDepth(req protocol.SpawnRequest, childDepth, ceiling int) int {
	want := defaultSpawnDepth
	if req.MaxDepth != nil {
		want = *req.MaxDepth
	}
	if want < 0 {
		want = 0
	}
	if room := ceiling - childDepth; want > room {
		want = max(room, 0)
	}
	return want
}

// checkConcurrency caps simultaneously LIVE descendants across the spawner's
// subtree.
//
// Separate from cost on purpose: a runaway recursion of cheap spawns exhausts
// process tables, file descriptors and memory long before it exhausts a dollar
// budget, and the two failures want different numbers.
func (c *Controller) checkConcurrency(req protocol.SpawnRequest) error {
	if req.ParentChildID == "" {
		return nil // a top-level spawn has no subtree to bound
	}
	parent, ok := c.st.Get(req.ParentChildID)
	if !ok {
		return limitError("parentChildId: no such child: %s", req.ParentChildID)
	}
	limit := parent.MaxChildren
	if limit <= 0 {
		return limitError(
			"agent %s has a live-agent cap of %d and may not spawn",
			req.ParentChildID, limit)
	}
	live := c.st.LiveDescendantCount(req.ParentChildID)
	if live >= limit {
		return limitError(
			"spawn refused: %d live agent(s) already running beneath %s, at its cap of %d. Wait for one to finish, or stop one with agent_kill",
			live, req.ParentChildID, limit)
	}
	return nil
}

// subtreeCoster is the one thing the budget checks need from insights.
type subtreeCoster interface {
	SubtreeCost(ctx context.Context, sel insights.SubtreeSelector) (float64, error)
}

// checkBudget enforces the cost ceiling at admission.
//
// Cost is the one limit that DECREMENTS: dollars a grandchild spends are gone
// from the subtree, so the budget is consulted against actual spend rather
// than against a static grant. Spend is summed across the subtree via phase
// 01's lineage labels — which is why that phase stamps rafiki/root even though
// rafiki/parent alone would describe the tree.
//
// Fails CLOSED for a budgeted parent: if the spend cannot be read, the answer
// to "are we under budget" is unknown, and treating unknown as yes is how a
// database hiccup becomes an unbounded bill. An UNBUDGETED parent has nothing
// to compare against, so the same failure is irrelevant to it and must not
// block it — the daemon must keep working without a database.
func (c *Controller) checkBudget(req protocol.SpawnRequest) error {
	// A negative grant is malformed, and it is malformed independently of the
	// parent — hence before the early returns below, both of which would
	// otherwise let it through on exactly the paths where no other limit
	// bounds the child.
	//
	// This is the one limit where clamping to 0 fails OPEN: grantedCost stores
	// 0 as UNLIMITED, so --max-cost=-1 would mint an unbudgeted child, and
	// then checkBudget returns nil for every one of ITS spawns in turn. Depth
	// and children clamp negatives to 0 too, but there 0 means "cannot spawn",
	// so they fail closed and need no equivalent guard.
	if req.MaxCost != nil && *req.MaxCost < 0 {
		return limitError(
			"spawn refused: max-cost cannot be negative (asked for $%.2f). Omit it to grant an unlimited budget, or name a positive amount",
			*req.MaxCost)
	}
	if req.ParentChildID == "" {
		return nil
	}
	parent, ok := c.st.Get(req.ParentChildID)
	if !ok {
		return limitError("parentChildId: no such child: %s", req.ParentChildID)
	}
	if parent.MaxCost <= 0 {
		return nil // unset means unlimited
	}

	ctx, cancel := context.WithTimeout(context.Background(), budgetQueryTimeout)
	defer cancel()
	spent, err := c.subtreeSpend(ctx, req.ParentChildID)
	if err != nil {
		return limitError(
			"spawn refused: agent %s has a $%.2f budget and its spend could not be read (%v). A budget that cannot be checked is not enforced, so this fails closed",
			req.ParentChildID, parent.MaxCost, err)
	}

	remaining := parent.MaxCost - spent
	if remaining <= 0 {
		return limitError(
			"spawn refused: budget exhausted — this agent tree has spent $%.2f of its $%.2f budget. Raise it with a higher --max-cost, or finish with what is already running",
			spent, parent.MaxCost)
	}
	if req.MaxCost != nil && *req.MaxCost > remaining {
		return limitError(
			"spawn refused: you asked to grant $%.2f but only $%.2f of the $%.2f budget remains (already spent: $%.2f). Grant at most the remainder",
			*req.MaxCost, remaining, parent.MaxCost, spent)
	}
	return nil
}

// budgetQueryTimeout bounds the spend rollup. A slow query must not wedge a
// spawn indefinitely; it becomes a fail-closed refusal instead, which is
// recoverable by retrying.
const budgetQueryTimeout = 5 * time.Second

// subtreeSelector collects every conversation belonging to rootChildID's
// subtree, INCLUDING rootChildID's own.
//
// Two routes, because a subtree routinely mixes execution models: a fundi
// child's conversation id is on its snapshot (SessionID), while a claude or
// pi child's row correlates by external_ref, which the daemon sets to the
// childID (controller.go's X-Rafiki-Session header). Following only one
// under-reports, and under-reporting a budget is the expensive direction.
func (c *Controller) subtreeSelector(rootChildID string) insights.SubtreeSelector {
	var sel insights.SubtreeSelector
	add := func(snap childstore.Snapshot) {
		if snap.SessionID != "" {
			sel.ConversationIDs = append(sel.ConversationIDs, snap.SessionID)
		}
		sel.ExternalRefs = append(sel.ExternalRefs, snap.ChildID)
	}
	if snap, ok := c.st.Get(rootChildID); ok {
		add(snap)
	}
	for _, snap := range c.st.Descendants(rootChildID) {
		add(snap)
	}
	return sel
}

func (c *Controller) subtreeSpend(ctx context.Context, rootChildID string) (float64, error) {
	if c.coster == nil {
		return 0, errors.New("no cost source configured")
	}
	return c.coster.SubtreeCost(ctx, c.subtreeSelector(rootChildID))
}

// childDepthFor is where a child of parentID would land. Kept separate from
// checkDepth so the stored grant and the admission check cannot disagree
// about the arithmetic.
func childDepthFor(st *childstore.Store, parentID string) int {
	if parentID == "" {
		return 0
	}
	abs := st.AbsoluteDepth(parentID)
	if abs < 0 {
		return 0
	}
	return abs + 1
}

// grantedCost resolves the child's budget. Nil means unlimited, stored as 0 —
// which is why every budget check must test "is this child budgeted at all"
// before comparing, rather than treating 0 as "spend nothing".
//
// The negative clamp is unreachable: checkBudget refuses a negative grant at
// admission, because HERE the clamp would fail open — 0 is unlimited. It is
// kept only so a future caller that reaches this without going through
// admission gets a defined value rather than a negative budget in the store.
func grantedCost(req protocol.SpawnRequest) float64 {
	if req.MaxCost == nil {
		return 0
	}
	if *req.MaxCost < 0 {
		return 0
	}
	return *req.MaxCost
}

func grantedChildren(req protocol.SpawnRequest) int {
	if req.MaxChildren == nil {
		return defaultMaxChildren
	}
	if *req.MaxChildren < 0 {
		return 0
	}
	return *req.MaxChildren
}

// checkKindNarrowing refuses a child whose kind would escape its parent's
// executor grant.
//
// claude and pi are forked on the daemon's own host: agentRunner returns a nil
// Runner for them, so resolveExecutor is never called and ExecutorSelector is
// ignored outright. A confined parent
// could therefore spawn an unconfined sibling simply by naming a kind — and
// agent_spawn exposes `kind` to the model, so the escape is one prompt
// injection away.
//
// This is the same monotonicity effectiveExecutorSet enforces for selectors: a
// descendant may narrow, never widen. It reads only the parent row the daemon
// stamped — nothing from the caller, nothing from the model — so ctrl_spawn,
// ctrl_resume and agent_spawn are all covered by the one check, with no route
// left to enumerate.
func checkKindNarrowing(st *childstore.Store, req protocol.SpawnRequest) error {
	if req.Kind == protocol.KindFundi {
		return nil
	}
	if req.ParentChildID == "" {
		return nil // top-level: nothing to widen
	}
	parent, ok := st.Get(req.ParentChildID)
	if !ok {
		// computeLineageLabels refuses an unknown parent with a better
		// message; not finding one here is not this check's business.
		return nil
	}
	if parent.ExecutorSelector == "" {
		return nil // parent is already unconfined on this host
	}

	// An omitted kind is pi (spawnKindLabel), so say what was actually
	// requested rather than echoing an empty string back at the caller.
	asked := req.Kind
	if asked == "" {
		asked = protocol.KindPi + " (the default when kind is omitted)"
	}
	return &control.ControllerError{
		Code: protocol.ErrInvalidArgs,
		Message: "spawn refused: the parent runs under an executor grant, and kind " + asked +
			" ignores executors entirely — it would fork on the daemon's own host with the " +
			"daemon's filesystem. Only kind \"fundi\" can honour an executor grant.",
	}
}
