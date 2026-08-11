package main

import (
	"fmt"
	"strconv"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
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

// checkBudget is implemented in Task 4.
func (c *Controller) checkBudget(protocol.SpawnRequest) error { return nil }

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
