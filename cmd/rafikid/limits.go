package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

const (
	// defaultSpawnDepth is what a child gets when the spawner names nothing:
	// enough to delegate once, not enough to build a tree by accident.
	defaultSpawnDepth = 1
	// defaultMaxChildren bounds live descendants across a subtree.
	defaultMaxChildren = 10
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
	return &connectapi.ControllerError{
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
			// The Get above proved the child exists, so this is the
			// truncated-chain sentinel, not "no such child": the stored parent
			// chain does not reach a root within the walk bound. The child's
			// true position — and so the new child's landing depth — is
			// unknown, and unknown here must refuse, not guess (see
			// checkBudget's fails-closed note for the same reasoning).
			return limitError("parentChildId: cannot determine the depth of %s: its parent chain does not reach a root within the walk limit", req.ParentChildID)
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
	// An indeterminate landing depth (childDepthFor's -1) grants nothing. The
	// child may sit anywhere, including past the ceiling, so the only safe
	// grant is "may not spawn" — the same 0-means-cannot convention
	// grantedChildren uses. Letting the negative ride through the room
	// arithmetic below would fail OPEN: it reads as extra room, and a spawn
	// off a corrupt record mints a child with depth it was never entitled to.
	if childDepth < 0 {
		return 0
	}
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
		// This is the one TRANSIENT refusal in the gate: the cap frees up the
		// moment a descendant settles, so the caller can retry — unlike the
		// depth and zero-cap refusals, which are permanent properties of the
		// parent's grant. Classifying it at_capacity (resource_exhausted)
		// rather than invalid_args is what tells a client to back off and
		// retry instead of rewriting the request.
		return &connectapi.ControllerError{
			Code: protocol.ErrAtCapacity,
			Message: fmt.Sprintf(
				"spawn refused: %d live agent(s) already running beneath %s, at its cap of %d. Wait for one to finish, or stop one with agent_kill",
				live, req.ParentChildID, limit),
		}
	}
	return nil
}

// subtreeCoster is the one thing the budget checks and the spend rollups need
// from insights.
type subtreeCoster interface {
	// UsageByConversation returns the selection's token sums per (conversation,
	// model), unpriced, in one round trip. Subtree spend reads it rather than a
	// pre-priced total so a closed member's rows can be memoised and the union
	// priced once.
	UsageByConversation(ctx context.Context, sel insights.SubtreeSelector) ([]insights.ConversationUsage, error)
	// PriceUsage prices those rows through the SAME pricer this coster prices
	// its SQL rollups with, so "what does this subtree cost" cannot acquire a
	// second definition of list prices.
	PriceUsage(rows []insights.ConversationUsage) (total float64, unpriced []string)
	// CostsByConversation prices the selection per conversation in one round
	// trip. A LIST wants each child's own number, and asking for a priced
	// subtree once per child is N round trips.
	CostsByConversation(ctx context.Context, sel insights.SubtreeSelector) ([]insights.ConversationCost, error)
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
// Three routes, because a subtree routinely mixes execution models: a fundi
// child's conversation id is on its lineage row (session_id / conversation_id),
// while a claude or pi child's row correlates by external_ref, which the daemon
// sets to the childID (controller.go's X-Rafiki-Session header). The third is
// the branch prefix "<childID>:", which catches a claude child's thread branches
// that have no synthetic child of their own — Claude Code's WebFetch and WebSearch
// helpers fork one per call and are deliberately not agents, but their spend
// is the parent's. Following only one under-reports, and under-reporting a
// budget is the expensive direction.
//
// A real subagent's branch is named by BOTH its own ExternalRef and its
// parent's prefix; the query ORs the three lists in one pass, so a row
// matching twice is still one row.
//
// ONE source: the database lineage. Every child — native subagents included —
// has a row, so unioning the live in-memory set would be a second definition of
// what a subtree contains, and a disagreement in the CHEAP direction is an
// unenforced budget. The live store answers only live-state questions.
//
// A lineage error is RETURNED, never swallowed, so each caller decides what
// an unreadable lineage means for it: admission and budget mutation fail
// CLOSED (a budget that cannot be checked is not enforced), the
// child-credential scope path answers an error rather than an empty-and-
// allowed scope, and a display path (a heartbeat's cost clause) degrades by
// omitting the number. Returning an empty selector instead would fail OPEN on
// every budget path — closing a child could LOWER an ancestor's spend. A
// missing lineage source is an error for the same reason, never an empty scope.
func (c *Controller) subtreeSelector(ctx context.Context, rootChildID string) (insights.SubtreeSelector, error) {
	members, err := c.lineageMembers(ctx, rootChildID)
	if err != nil {
		return insights.SubtreeSelector{}, err
	}
	var sel insights.SubtreeSelector
	for _, m := range members {
		addLineageMember(&sel, m)
	}
	sel.ConversationIDs = dedupeStrings(sel.ConversationIDs)
	sel.ExternalRefs = dedupeStrings(sel.ExternalRefs)
	sel.ExternalRefPrefixes = dedupeStrings(sel.ExternalRefPrefixes)
	return sel, nil
}

// addLineageMember names one member by all three correlation routes: its
// conversation id(s) by UUID, its child id by external_ref, and the "<childID>:"
// branch prefix its thread branches carry.
func addLineageMember(sel *insights.SubtreeSelector, m childstore.LineageMember) {
	for _, conv := range []string{m.ConversationID, m.SessionID} {
		if conv != "" {
			sel.ConversationIDs = append(sel.ConversationIDs, conv)
		}
	}
	sel.ExternalRefs = append(sel.ExternalRefs, m.ChildID)
	sel.ExternalRefPrefixes = append(sel.ExternalRefPrefixes, m.ChildID+threadRefSep)
}

// lineageRows reads childID's line straight from the database: the child's own
// row and every row beneath it, live or closed, labels included.
//
// A missing lineage source is an ERROR, never an empty line: a daemon that
// cannot read its lineage cannot price a subtree or scope a child credential,
// and answering "nothing is in it" fails OPEN on every budget path. The one
// entry point is childstore.LineageSource.Lineage — there is no root-fallback
// dialect, because the database knows an ancestor's root from the ancestor's
// own row.
func (c *Controller) lineageRows(ctx context.Context, childID string) ([]childstore.LineageRow, error) {
	if c.lineage == nil {
		return nil, fmt.Errorf("subtree lineage for %s: no lineage source", childID)
	}
	rows, err := c.lineage.Lineage(ctx, childID)
	if err != nil {
		return nil, fmt.Errorf("subtree lineage for %s: %w", childID, err)
	}
	return rows, nil
}

// lineageMembers is lineageRows projected onto the database-free member shape.
func (c *Controller) lineageMembers(ctx context.Context, childID string) ([]childstore.LineageMember, error) {
	rows, err := c.lineageRows(ctx, childID)
	if err != nil {
		return nil, err
	}
	return c.membersWithin(rows, childID), nil
}

// membersWithin selects childID's own line from a row set a lineage read
// returned, adding childID's own row from the LIVE store when the row set has
// none. That is the only place the live store answers a lineage question, and it
// is deliberately narrow: a child that is running but whose own row is not in
// the read still prices the conversations it is writing, and the direction that
// must never be wrong is the one that counts the money. It never contributes a
// second member — the subtree itself comes from the database.
func (c *Controller) membersWithin(rows []childstore.LineageRow, childID string) []childstore.LineageMember {
	members := childstore.DescendantsOf(rows, childID)
	for _, m := range members {
		if m.ChildID == childID {
			return members
		}
	}
	if snap, ok := c.st.Get(childID); ok {
		return append([]childstore.LineageMember{
			{ChildID: snap.ChildID, SessionID: snap.SessionID},
		}, members...)
	}
	return members
}

// dedupeStrings removes duplicates from in, preserving first-seen order. It is
// the union step subtreeSelector needs: a member names a row through more than
// one route, and a selector that named a row twice would pass it to the query
// twice.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// subtreeSpend prices rootChildID's whole subtree: every lineage member's usage
// rows, each conversation counted once, priced through the coster's own pricer.
//
// It propagates its own "no cost source" error, the lineage error and the usage
// error, so an unreadable subtree stays a fail-closed refusal at every budget
// caller rather than an empty (cheap) one.
func (c *Controller) subtreeSpend(ctx context.Context, rootChildID string) (float64, error) {
	if c.coster == nil {
		return 0, errors.New("no cost source configured")
	}
	members, err := c.lineageMembers(ctx, rootChildID)
	if err != nil {
		return 0, err
	}
	usage, err := c.usageForMembers(ctx, members)
	if err != nil {
		return 0, err
	}
	total, unpriced := c.coster.PriceUsage(usage)
	if len(unpriced) > 0 {
		// WARN, not Debug: a model with no list price records its usage as $0,
		// and the dollars this returns are what checkBudget, sweepBudgets and
		// budgetHalted compare a child's lifetime maxCost against. The number that
		// degrades is the GUARDRAIL's, not a display figure, and silently is how a
		// child runs past its budget unnoticed.
		//
		// Deliberately not fatal: the turns ran and were captured, so refusing here
		// would wedge a healthy fleet over a missing catalog entry. PriceUsage names
		// each unpriced model ONCE, so one uncatalogued model prints one line
		// however many conversations spent on it.
		slog.Warn("subtree spend: an unpriced model's usage is counted as $0, so this budget cannot see its cost",
			"root", rootChildID, "models", unpriced)
	}
	return total, nil
}

// childDepthFor is where a child of parentID would land. Kept separate from
// checkDepth so the stored grant and the admission check cannot disagree
// about the arithmetic.
//
// It returns -1 when the landing depth cannot be determined: parentID is
// unknown, or its parent chain cannot be walked to a root within the walk
// bound. Callers must never feed that -1 into grant arithmetic as a real
// depth — grantedDepth turns it into a grant of 0 ("cannot spawn"), never
// "assume top level".
func childDepthFor(st *childstore.Store, parentID string) int {
	if parentID == "" {
		return 0
	}
	abs := st.AbsoluteDepth(parentID)
	if abs < 0 {
		return -1
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
// executor set.
//
// Kinds split by how they host. A fundi child honours the set directly:
// resolveExecutor -> chooseExecutor admits it against the set the child
// inherits from its parent. A claude child honours the set exactly when this
// daemon is executor-routed (claudeExecutorRouted): its daraja, and the claude
// process the daraja hosts, launch on the pool's machine via
// chooseLaunchExecutor, which runs the same admission pipeline against that
// inherited set, so no live executor outside the parent's set can host it.
// Without a pool connection claudeRunner falls back to a local subprocess on
// the daemon's own host, which IS the widening this guard refuses: a parent
// confined to an executor set could otherwise spawn a child running outside
// every executor it is restricted to simply by naming a kind, and agent_spawn
// exposes `kind` to the model, so the escape is one prompt injection away.
//
// A parent is confined when its set is not "all" (childstore.ResolveSetSpec;
// a nil stored set is a legacy row and resolves through its selector). A
// descendant may narrow, never widen. This reads only the parent row the
// daemon stamped and daemon-local pool state: nothing from the caller, nothing
// from the model, so Spawn, Resume and agent_spawn are all covered by the one
// check, with no route left to enumerate.
func checkKindNarrowing(st *childstore.Store, req protocol.SpawnRequest, executorPoolConnected, scriptExecutorRouted bool) error {
	// An omitted kind is fundi (spawnKindLabel), so normalize it the same way
	// resolveSpawnPlan and spawnKindLabel do before testing against the kinds
	// that honour an executor set.
	kind := req.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	if kind == protocol.KindFundi {
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
	if all, _, _ := childstore.ResolveSetSpec(parent.ExecutorSet, parent.ExecutorSelector); all {
		return nil // parent addresses every executor of its owner: nothing to widen
	}

	// claude honours the set whenever the daemon is executor-routed: the
	// launch executor is picked by chooseLaunchExecutor against the inherited
	// set, so the spawn never widens it. Only the local-subprocess fallback forks on
	// the daemon's own host.
	if kind == protocol.KindClaude && executorPoolConnected {
		return nil
	}

	// script honours the set when a live executor actually advertises the
	// script launch kind (scriptExecutorRouted — the same predicate the
	// runner routes on): the child's daraja launches on the pool's machine
	// through chooseLaunchExecutor, the same admission pipeline. When no
	// executor declares the kind, the runner falls back to a local fork —
	// which is exactly the widening this guard refuses for a confined
	// parent, so the fallback must be refused rather than taken.
	if kind == protocol.KindScript && scriptExecutorRouted {
		return nil
	}

	if kind == protocol.KindClaude {
		return &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "spawn refused: the parent is confined to an executor set, and kind claude has " +
				"no executor pool connection — its local-subprocess fallback would fork on the " +
				"daemon's own host with the daemon's filesystem. Configure an executor pool " +
				"(RAFIKI_EXECUTORS_ENABLED) or run this spawn top-level.",
		}
	}
	if kind == protocol.KindScript {
		if executorPoolConnected {
			return &connectapi.ControllerError{
				Code: protocol.ErrInvalidArgs,
				Message: "spawn refused: the parent is confined to an executor set, and no live executor " +
					"advertises the script launch kind (start one with --launch script) — the local " +
					"fallback would fork on the daemon's own host with the daemon's filesystem. " +
					"Run this spawn top-level, or host a script-capable executor.",
			}
		}
		return &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "spawn refused: the parent is confined to an executor set, and kind script has " +
				"no executor pool connection — its local-subprocess fallback would fork on the " +
				"daemon's own host with the daemon's filesystem. Configure an executor pool " +
				"(RAFIKI_EXECUTORS_ENABLED) with --launch script, or run this spawn top-level.",
		}
	}
	return &connectapi.ControllerError{
		Code: protocol.ErrInvalidArgs,
		Message: "spawn refused: the parent is confined to an executor set, and kind " + kind +
			" ignores executors entirely — it would fork on the daemon's own host with the " +
			"daemon's filesystem. Only kind \"fundi\" (and kind \"claude\" with an executor " +
			"pool configured) can honour an executor set.",
	}
}

// localForkRefused reports the confinement that forbids hosting a child through
// a LOCAL fork on the daemon's own host (the script and claude fallbacks), or
// nil when the fork is allowed.
//
// checkKindNarrowing refuses this at Spawn for a FRESH child by reading the
// PARENT's stored grant; this makes the SAME decision at the point of the fork,
// so any caller that reaches a runner without that guard — a resume, startup
// recovery, or a future runner caller — still cannot fork a confined child on
// the daemon host. The two must agree: a parent the guard refuses is exactly a
// child this refuses.
//
// It reads the child's set from the one place it lives at that moment:
//
//   - the child's OWN stored set when its row exists (a resume/recovery); and
//   - freshSet, the set resolveSpawnExecutorSet fixed on the request, when it
//     does not (a fresh spawn: the row is written only after the runner is
//     built). A fresh child's set is a subset of its parent's but need not be
//     "all" when the parent's is, so the parent's set proves nothing about it.
//     A fresh child whose set was never resolved (nil) fails CLOSED: it cannot
//     be shown to be unconfined, and a fresh spawn always carries one; and
//   - sandbox ownership, read exactly as controllerBinder.ChooseFor reads it
//     (ownedSandbox): the child's own spawn-block sandbox, or a subtree one an
//     ancestor holds. A sandboxed child's stored set normally covers this too;
//     the lookup is belt-and-braces for a row whose set was never persisted.
//
// A child is confined when its set is not "all"; a nil stored set is a legacy
// row and resolves through its selector.
func (c *Controller) localForkRefused(ctx context.Context, kind, childID, parentChildID, ownerUserID string, freshSet []string) error {
	if ctx == nil {
		// A hand-built Controller in a test may have no base context; the lookup
		// below must never run on a nil parent (context.WithTimeout panics).
		ctx = context.Background()
	}
	if snap, ok := c.st.Get(childID); ok {
		if all, _, _ := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector); !all {
			return localForkConfinedErr(kind, "an executor set")
		}
	} else {
		if freshSet == nil {
			return fmt.Errorf("cannot confirm the executor set of new child %s: none was resolved", childID)
		}
		if all, _, _ := childstore.ResolveSetSpec(freshSet, ""); !all {
			return localForkConfinedErr(kind, "an executor set")
		}
	}
	if c.sandboxStore != nil {
		ctx, cancel := context.WithTimeout(ctx, sandboxBindTimeout)
		defer cancel()
		row, owned, err := c.ownedSandbox(ctx, childID, parentChildID, ownerUserID)
		if err != nil {
			return err
		}
		if owned {
			return localForkConfinedErr(kind, "sandbox "+sandboxDisplayName(row))
		}
	}
	return nil
}

// localForkConfinedErr is the actionable refusal localForkRefused returns. It
// names the kind and the confinement, and points at the two fixes (host the
// kind on an executor, or spawn top-level).
func localForkConfinedErr(kind, confinement string) error {
	return &connectapi.ControllerError{
		Code: protocol.ErrInvalidArgs,
		Message: fmt.Sprintf(
			"refused: this %s child is confined to the executor plane (%s), and no live executor can host it — "+
				"a local process on the daemon's own host would escape that confinement. "+
				"Host a %s-capable executor, or run this spawn top-level.",
			kind, confinement, kind),
	}
}
