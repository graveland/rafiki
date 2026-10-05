// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/quota"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/users"
)

// nativeEventSource adapts the nativebus registry to connectapi.EventSource.
type nativeEventSource struct {
	native *nativebus.Registry
}

func (s *nativeEventSource) Subscribe(childID string) (<-chan *rafikiv1.Event, func()) {
	return s.native.Subscribe(childID)
}

func (s *nativeEventSource) SubscribeAll() (<-chan *rafikiv1.Event, func()) {
	return s.native.SubscribeAll()
}

// Subscribe satisfies connectapi.EventSource via the nativeEventSource adapter.
func (c *Controller) nativeEventSource() *nativeEventSource {
	return &nativeEventSource{native: c.native}
}

// ListChildren satisfies connectapi.ChildLister. An empty statuses means no
// filter.
func (c *Controller) ListChildren(statuses []string) []protocol.ChildSummary {
	snaps := c.List(protocol.ListFilter{})
	kept := make([]childstore.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if len(statuses) > 0 && !containsString(statuses, string(s.Status)) {
			continue
		}
		kept = append(kept, s)
	}
	return c.summariesFor(kept)
}

// summariesFor maps snapshots onto the protocol summaries every list-shaped
// verb serves: ONE batched cost rollup for the non-script kinds (see costsFor
// — N serial round trips was the cockpit's seed-path stall), per-subtree
// pricing for script children (see scriptSpend), and the snapshotToSummary
// field mapping (provider+model join, nil PID for an exited child,
// Unix-millis stamps, catalog-sourced context window) that is easy to get
// subtly wrong by hand. Both the operator list and the child-scoped subtree
// list go through it, so a child caller's ListChildren answers with the same
// per-row facts an operator's does, minus the rows outside its subtree.
func (c *Controller) summariesFor(kept []childstore.Snapshot) []protocol.ChildSummary {
	// ONE bounded context for the whole list's cost work, and ONE rollup for
	// the whole list. Pricing each child with its own SubtreeCost call meant N
	// serial round trips, each with its own timeout, on the cockpit's seed
	// path -- and the seed is re-run whenever an unknown child produces
	// traffic, which is exactly the busy-fleet case where N is large. The
	// single ctx matters as much as the single rollup: the rollup and the
	// per-script subtree queries run SEQUENTIALLY, so two separate timeout
	// windows would give one list a 2×costRollupTimeout latency bound.
	// Script children are withheld from the rollup: their CostUSD is their
	// SUBTREE's spend (scriptSpend), never a rollup of their own rows.
	ctx, cancel := context.WithTimeout(context.Background(), costRollupTimeout)
	defer cancel()

	costSnaps := make([]childstore.Snapshot, 0, len(kept))
	for _, s := range kept {
		if s.Kind != protocol.KindScript {
			costSnaps = append(costSnaps, s)
		}
	}
	costs := c.costsFor(ctx, costSnaps)

	// The script children price one subtree query each, under the same batch
	// bound as the rollup — the SAME ctx, not a second window: both are
	// display numbers that must not hold up the list they decorate.
	out := make([]protocol.ChildSummary, 0, len(kept))
	for _, s := range kept {
		sum := snapshotToSummary(s, c.ContextWindow)
		if s.Kind == protocol.KindScript {
			sum.CostUSD = c.scriptSpend(ctx, s.ChildID)
		} else if cost, ok := costs[s.ChildID]; ok {
			sum.CostUSD = &cost
		}
		out = append(out, sum)
	}
	return out
}

// GetChild satisfies connectapi.ChildLister.
func (c *Controller) GetChild(childID string) (protocol.ChildSummary, bool) {
	snap, ok := c.Get(childID)
	if !ok {
		return protocol.ChildSummary{}, false
	}
	sum := snapshotToSummary(snap, c.ContextWindow)
	if snap.Kind != protocol.KindScript {
		ctx, cancel := context.WithTimeout(context.Background(), costRollupTimeout)
		defer cancel()
		if cost, ok := c.costsFor(ctx, []childstore.Snapshot{snap})[childID]; ok {
			sum.CostUSD = &cost
		}
		return sum, true
	}
	// A script child prices by subtree, the same rule the list path applies.
	ctx, cancel := context.WithTimeout(context.Background(), costRollupTimeout)
	defer cancel()
	sum.CostUSD = c.scriptSpend(ctx, childID)
	return sum, true
}

// scriptSpend resolves one script child's CostUSD: the SUBTREE's spend. A
// script child drives no turns of its own — its fundi and claude descendants
// do — and subtreeSpend folds the script child's own conversations in with
// the descendants', so the answer is the subtree's whole spend. An error
// means the field stays unset: nil is "not reported", and a reported zero
// would read as "the subtree has spent nothing", which nothing here measured.
// The error itself is logged at Debug — an unpriced row is a display
// degradation, not an RPC failure the caller could act on.
func (c *Controller) scriptSpend(ctx context.Context, childID string) *float64 {
	spend, err := c.subtreeSpend(ctx, childID)
	if err != nil {
		slog.Debug("connect: script child subtree spend unavailable", "child", childID, "error", err)
		return nil
	}
	return &spend
}

// snapshotToSummary converts a childstore.Snapshot to the wire ChildSummary
// shape. PID is omitted (nil) when the child has exited. Model is formatted as
// "provider/model" when both fields are present.
//
// contextWindow, when non-nil, is consulted for the resolved Model to fill
// ContextWindow/MaxCompletionTokens — a func rather than a *routing.ModelCatalog
// so callers (and tests) need not depend on pkg/routing; callers pass
// Controller.ContextWindow bound to the real implementation. A nil func or a
// false ok leaves both fields at their zero value (omitted on the wire).
func snapshotToSummary(snap childstore.Snapshot, contextWindow func(model string) (contextLen, maxCompletion int, ok bool)) protocol.ChildSummary {
	model := snap.Model
	if snap.Provider != "" && snap.Model != "" {
		model = snap.Provider + "/" + snap.Model
	}
	var pid *int
	if snap.Status != protocol.StatusExited {
		pid = &snap.PID
	}
	cs := protocol.ChildSummary{
		ChildID:      snap.ChildID,
		PID:          pid,
		Cwd:          snap.Cwd,
		Name:         snap.Name,
		Kind:         snap.Kind,
		Model:        model,
		SessionID:    snap.SessionID,
		SessionFile:  snap.SessionFile,
		Status:       string(snap.Status),
		StartedAt:    snap.StartedAt,
		LastActivity: snap.LastActivity.UnixMilli(),
		ExitCode:     snap.ExitCode,
		ExitSignal:   snap.ExitSignal,
		// The once-resolved routing spec, mirrored from the session. Model is
		// the BASE id — the spec lives only here and in the rafiki/routing
		// label.
		Routing: snap.Routing,
	}
	if len(snap.Labels) > 0 {
		cs.Labels = snap.Labels
	}
	if len(snap.SlashCommands) > 0 {
		cs.SlashCommands = snap.SlashCommands
	}
	if contextWindow != nil && model != "" {
		if cl, mc, ok := contextWindow(model); ok {
			cs.ContextWindow = cl
			cs.MaxCompletionTokens = mc
		}
	}
	if snap.MaxCost > 0 {
		maxCost := snap.MaxCost
		cs.MaxCost = &maxCost
	}
	if snap.Result != "" {
		cs.Result = snap.Result
	}
	return cs
}

// costRollupTimeout bounds the whole batch, not one child. It is a display
// number: a slow rollup must not hold up the list it decorates.
const costRollupTimeout = 3 * time.Second

// costsFor prices every non-script child in one round trip, keyed by child
// id. Callers pass only non-script snapshots: a script child's CostUSD is its
// subtree's spend (scriptSpend), not a rollup of its own conversations. The
// context is the CALLER's — summariesFor shares one bounded window across its
// rollup and its per-script subtree queries, rather than stacking two.
//
// The two correlation routes are NOT interchangeable and getting them the
// wrong way round is silent. A conversation is found by UUID for a fundi child
// (childstore.Snapshot.SessionID) and by external_ref for a proxy child, where
// the daemon sets X-Rafiki-Session to the CHILD id. subtreeSelector is the
// established mapping and says the same thing; handing SessionID to both
// routes makes every proxy child match neither, roll up 0, and report a
// non-nil zero -- which then overwrites a cost the rail had correctly
// accumulated from turn_end.
//
// Absent from the map means NOT KNOWN (no cost source, or the rollup failed)
// and leaves CostUSD nil. Present-and-zero means the query ran and found no
// turns, which is a different fact and is allowed to be reported.
func (c *Controller) costsFor(ctx context.Context, snaps []childstore.Snapshot) map[string]float64 {
	if c.coster == nil || len(snaps) == 0 {
		return nil
	}
	var sel insights.SubtreeSelector
	for _, s := range snaps {
		if s.SessionID != "" {
			sel.ConversationIDs = append(sel.ConversationIDs, s.SessionID)
		}
		sel.ExternalRefs = append(sel.ExternalRefs, s.ChildID)
		sel.ExternalRefPrefixes = append(sel.ExternalRefPrefixes, s.ChildID+threadRefSep)
	}

	rows, err := c.coster.CostsByConversation(ctx, sel)
	if err != nil {
		return nil
	}

	byConv := make(map[string]insights.ConversationCost, len(rows))
	byRef := make(map[string]insights.ConversationCost, len(rows))
	// orphanBranches sums, per parent child id, the branch conversations that
	// belong to no child of their own. Those are Claude Code's WebFetch and
	// WebSearch helpers: they fork a branch per call and get no synthetic child
	// (they declare no client tools, so they are not agents), and the spend is
	// a tool call the PARENT made. Rolling it into the parent's own cost is
	// both where it belongs and the only way it stays in TOTAL, which is summed
	// from child rows.
	//
	// Claimed-ness is checked against the whole childstore, never against
	// snaps: snaps may be status-filtered, and a real subagent filtered out of
	// the list must not have its cost slide onto its parent.
	orphanBranches := make(map[string]float64)
	for _, r := range rows {
		byConv[r.ConversationID] = r
		if r.ExternalRef == "" {
			continue
		}
		byRef[r.ExternalRef] = r
		parent, _, isBranch := strings.Cut(r.ExternalRef, threadRefSep)
		if !isBranch {
			continue
		}
		if _, claimed := c.st.Get(r.ExternalRef); !claimed {
			orphanBranches[parent] += r.Cost
		}
	}

	out := make(map[string]float64, len(snaps))
	for _, s := range snaps {
		total := 0.0
		var counted string
		if s.SessionID != "" {
			if r, ok := byConv[s.SessionID]; ok {
				total += r.Cost
				counted = r.ConversationID
			}
		}
		// Dedupe on the conversation row: a child reachable by BOTH routes
		// resolves to one row and must be counted once.
		if r, ok := byRef[s.ChildID]; ok && r.ConversationID != counted {
			total += r.Cost
		}
		out[s.ChildID] = total + orphanBranches[s.ChildID]
	}
	return out
}

// DescendantDepth satisfies eventlog.Lineage.
func (c *Controller) DescendantDepth(ancestorID, candidateID string) int {
	return c.st.DescendantDepth(ancestorID, candidateID)
}

// DescendantIDs satisfies connectapi.DescendantLister: the ids beneath childID,
// deepest first, so a cascade ends each child before the parent that spawned
// it. Ties break on id so the order is stable. Native thread children are left
// out: they have no process of their own and already end and close with their
// parent (exitNativeChildrenOf, closeNativeChildrenOf).
func (c *Controller) DescendantIDs(childID string) []string {
	type entry struct {
		id    string
		depth int
	}
	var entries []entry
	for _, snap := range c.st.Descendants(childID) {
		if snap.Native {
			continue
		}
		entries = append(entries, entry{snap.ChildID, c.st.DescendantDepth(childID, snap.ChildID)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].depth != entries[j].depth {
			return entries[i].depth > entries[j].depth
		}
		return entries[i].id < entries[j].id
	})
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.id
	}
	return ids
}

// Labels satisfies eventlog.Lineage.
func (c *Controller) Labels(childID string) (map[string]string, bool) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return nil, false
	}
	return snap.Labels, true
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// connectLifecycle adapts *Controller to connectapi.ChildLifecycle, which
// cannot be satisfied by *Controller directly: Controller.Spawn and
// Controller.Kill already exist with different signatures, and renaming them
// would touch every existing caller for no gain.
type connectLifecycle struct{ c *Controller }

func (l connectLifecycle) Spawn(ctx context.Context, p connectapi.SpawnParams) (string, error) {
	// The policy interceptor (connect_policy.go) admits a per-child credential
	// here since wave 1 — Spawn is childScoped — and the handler has already
	// forced p.ParentChildID to the caller's own child id (see
	// connectapi.Spawn). That forced parent IS the child-spawn admission: the
	// depth, children and budget checks read the PARENT's grant through it,
	// and spawnOwner stamps the owner from the credential, which for a child
	// credential is its owner — the same owner fundi's controllerSpawner
	// stamps from the stored row, resolved here from the same credential the
	// mount authenticated.
	res, err := l.c.Spawn(ctx, buildProtocolSpawnRequest(p), spawnOwner(ctx))
	if err != nil {
		return "", err
	}
	return res.ChildID, nil
}

// buildProtocolSpawnRequest maps the Connect-plane params onto the framed
// protocol request Controller.Spawn applies. Extracted so the field mapping is
// testable without a Controller behind it.
//
// EnvOverride is never set here: it has no Connect-plane field (see
// connectapi.SpawnParams's doc comment) and stays false, its framed default.
func buildProtocolSpawnRequest(p connectapi.SpawnParams) protocol.SpawnRequest {
	return protocol.SpawnRequest{
		Cwd:              p.Cwd,
		Name:             p.Name,
		Model:            p.Model,
		Kind:             p.Kind,
		Preset:           p.Preset,
		Prefill:          p.Prefill,
		Script:           p.Script,
		Labels:           p.Labels,
		ParentChildID:    p.ParentChildID,
		ExecutorSelector: p.ExecutorSelector,
		SkipDerivedIndex: p.SkipDerivedIndex,
		ExecutorRef:      p.ExecutorRef,
		MaxDepth:         p.MaxDepth,
		MaxCost:          p.MaxCost,
		MaxChildren:      p.MaxChildren,

		ConfigDir:          p.ConfigDir,
		AppendSystemPrompt: p.AppendSystemPrompt,
		Thinking:           p.Thinking,
		NoSession:          p.NoSession,
		ResumeSession:      p.ResumeSession,
		ForkSession:        p.ForkSession,
		Extensions:         p.Extensions,
		NoExtensions:       p.NoExtensions,
		Verbose:            p.Verbose,
		ExtraArgs:          p.ExtraArgs,
		SkillsDirs:         p.SkillsDirs,
		MCPConfig:          p.MCPConfig,
		Env:                p.Env,
		RecordRequests:     p.RecordRequests,
		PassthroughAuth:    p.PassthroughAuth,
	}
}

func (l connectLifecycle) Kill(ctx context.Context, childID string, shutdownTimeout, killTimeout time.Duration) (connectapi.KillOutcome, error) {
	res, err := l.c.Kill(ctx, childID, shutdownTimeout, killTimeout)
	if err != nil {
		return connectapi.KillOutcome{}, err
	}
	return connectapi.KillOutcome{
		ExitCode:  res.ExitCode,
		Signal:    res.Signal,
		Duration:  res.Duration,
		Escalated: res.Escalated,
	}, nil
}

// SetBudget is operator authority for a user credential (or the socket's
// nil identity) and agent_set_budget's rule for a per-child credential:
// SetChildBudget checks direct parentage and the caller's remaining grant,
// so a child cannot lift its way past its own budget fence.
func (l connectLifecycle) SetBudget(ctx context.Context, childID string, maxCost float64) error {
	if id := server.IdentityFromContext(ctx); id != nil && id.Via == server.ProvenanceChildToken {
		return l.c.SetChildBudget(ctx, id.ChildID, childID, maxCost)
	}
	return l.c.SetChildBudgetAsOperator(ctx, childID, maxCost)
}

// SetRouting is operator authority for a user credential (or the socket's nil
// identity) and subtree authority for a per-child credential: SetChildRouting
// bounds the target to the caller's subtree and refuses only= from a child, so
// a child cannot pin providers past the daemon's bans.
func (l connectLifecycle) SetRouting(ctx context.Context, childID, delta string) (string, error) {
	if id := server.IdentityFromContext(ctx); id != nil && id.Via == server.ProvenanceChildToken {
		return l.c.SetChildRouting(ctx, id.ChildID, childID, delta)
	}
	return l.c.SetChildRoutingAsOperator(ctx, childID, delta)
}

// connectModels adapts *Controller to connectapi.ModelLister. A distinct type
// for the same reason connectLifecycle is one: Controller.ListModels already
// exists with a different signature, and renaming it would touch every
// existing caller for no gain.
type connectModels struct{ c *Controller }

func (m connectModels) ListModels(ctx context.Context, provider, kind string) ([]connectapi.ModelRow, error) {
	return m.c.ListModelRows(ctx, provider, kind)
}

// connectExecutors adapts *Controller to connectapi.ExecutorLister.
type connectExecutors struct{ c *Controller }

func (e connectExecutors) ListExecutors(ctx context.Context, kind string) ([]connectapi.ExecutorRow, error) {
	// The row set is scoped BY the caller's identity — "the executors this
	// owner could spawn onto" — so a child-attributed caller would be reading
	// its owner's fleet, the same borrowed identity Spawn refuses. The
	// refusal itself lives on the route's policy interceptor (userOnly), not
	// here. Both halves of the identity ride along: Username is the "owner"
	// label Admits matches, UserID is what the ownership rule compares —
	// the exact pair a real top-level spawn would carry.
	o := spawnOwner(ctx)
	return e.c.ListExecutorRows(ctx, kind, o.Username, o.UserID)
}

// connectSkills adapts the daemon's skills.Store to connectapi.SkillManager.
// The translation exists so pkg/connectapi — which cmd/rafiki links — never
// imports pkg/skills' store half. version is the daemon's build version, used
// to stamp shadowed_core_version on core overrides.
type connectSkills struct {
	st      skills.Store
	version string

	// push refreshes the executor-facing skill corpus after a successful
	// write (Upsert, SetEnabled): push-on-write rather than a timer. Nil is
	// legal — tests and any wiring constructed before the pusher existed —
	// and means writes still succeed, they just don't fan out.
	push func(ctx context.Context)
}

func skillRowFrom(r skills.Record) connectapi.SkillRow {
	return connectapi.SkillRow{
		Namespace:           r.Namespace,
		Name:                r.Name,
		Description:         r.Description,
		Body:                r.Body,
		Source:              r.Source,
		ShadowedCoreVersion: r.ShadowedCoreVersion,
		Enabled:             r.Enabled,
		UpdatedAt:           r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// translateSkillErr maps the store's sentinels onto connectapi's, so a missing
// skill stays an ANSWER (NotFound) and a name held by an enabled row stays one
// too (AlreadyExists) rather than both becoming internal errors.
func translateSkillErr(err error) error {
	if errors.Is(err, skills.ErrNotFound) {
		return connectapi.ErrSkillNotFound
	}
	if errors.Is(err, skills.ErrSourceConflict) {
		return connectapi.ErrSkillSourceConflict
	}
	return err
}

func (c connectSkills) ListSkills(ctx context.Context, includeDisabled bool) ([]connectapi.SkillRow, error) {
	recs, err := c.st.List(ctx, !includeDisabled)
	if err != nil {
		return nil, err
	}
	out := make([]connectapi.SkillRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, skillRowFrom(r))
	}
	return out, nil
}

func (c connectSkills) GetSkill(ctx context.Context, ns, name string) (connectapi.SkillRow, error) {
	r, err := c.st.Get(ctx, ns, name)
	if err != nil {
		return connectapi.SkillRow{}, translateSkillErr(err)
	}
	return skillRowFrom(r), nil
}

func (c connectSkills) UpsertSkill(ctx context.Context, row connectapi.SkillRow) (connectapi.SkillRow, error) {
	// Stamping shadowed_core_version is the daemon's job, never the client's:
	// the stamp records which DAEMON build's core skill this row displaced, and
	// warnStaleOverrides compares it against the running daemon — a client's
	// claim about either is a self-reported fact. It is stamped only when the
	// upsert replaces a core row (the row Get finds at the name), so an
	// ordinary skill never carries a phantom shadow version; a re-written
	// override carries no stamp at all, which reads as fresh rather than stale.
	// A Get that errors is not evidence — the upsert proceeds un-stamped.
	if c.version != "" {
		if cur, err := c.st.Get(ctx, row.Namespace, row.Name); err == nil && cur.Source == skills.CoreSource {
			row.ShadowedCoreVersion = c.version
		}
	}
	out, err := c.st.Upsert(ctx, skills.Record{
		Namespace:           row.Namespace,
		Name:                row.Name,
		Description:         row.Description,
		Body:                row.Body,
		Source:              row.Source,
		ShadowedCoreVersion: row.ShadowedCoreVersion,
		Enabled:             row.Enabled,
	})
	if err != nil {
		return connectapi.SkillRow{}, translateSkillErr(err)
	}
	if c.push != nil {
		c.push(ctx)
	}
	return skillRowFrom(out), nil
}

func (c connectSkills) DeleteSkill(ctx context.Context, ns, name string) error {
	return translateSkillErr(c.st.Delete(ctx, ns, name))
}

func (c connectSkills) SetSkillEnabled(ctx context.Context, ns, name string, enabled bool) error {
	if err := c.st.SetEnabled(ctx, ns, name, enabled); err != nil {
		return translateSkillErr(err)
	}
	if c.push != nil {
		c.push(ctx)
	}
	return nil
}

func (l connectLifecycle) Close(_ context.Context, childID string) error {
	return l.c.Close(childID)
}

// DescendantIDs satisfies connectapi.DescendantLister. Kill/Close's
// include_descendants asserts that optional interface off the ChildLifecycle
// value at runtime, and the value wired is this adapter, not *Controller —
// the method must exist HERE, whatever the Controller implements, or every
// include_descendants request is refused unimplemented.
func (l connectLifecycle) DescendantIDs(childID string) []string {
	return l.c.DescendantIDs(childID)
}

// spawnOwner maps the proxy face's authenticated identity onto the daemon's.
// Two types for one concept, because pkg/server predates pkg/users and the
// face's Identity is a pointer whose nil means "no user" — the case every
// unix-socket call takes.
func spawnOwner(ctx context.Context) users.Identity {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return users.Identity{}
	}
	return users.Identity{UserID: id.UserID, Username: id.Username}
}

// connectQuota adapts *quota.Store to connectapi.QuotaReader, resolving the
// caller's own user id from ctx rather than taking one as an argument — the
// same reasoning as spawnOwner: a caller-supplied id would let anyone read
// anyone else's usage.
type connectQuota struct{ store *quota.Store }

func (q connectQuota) RateLimitStatus(ctx context.Context) (connectapi.RateLimitStatus, bool, error) {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.UserID == "" {
		return connectapi.RateLimitStatus{}, false, nil
	}
	st, ok, err := q.store.Get(ctx, id.UserID)
	if err != nil || !ok {
		return connectapi.RateLimitStatus{}, ok, err
	}
	return connectapi.RateLimitStatus{
		OrganizationID: st.OrganizationID,
		FiveH: connectapi.RateLimitWindow{
			Utilization: st.FiveH.Utilization, ResetAt: st.FiveH.ResetAt, Status: st.FiveH.Status,
		},
		SevenD: connectapi.RateLimitWindow{
			Utilization: st.SevenD.Utilization, ResetAt: st.SevenD.ResetAt, Status: st.SevenD.Status,
		},
		OverallStatus: st.OverallStatus,
		UpdatedAt:     st.UpdatedAt,
	}, true, nil
}

// errNotAUserCredential is scopeFor's refusal for any NON-NIL identity that is
// not a real user credential -- the only provenance the design's Scope
// mechanism accepts, besides the nil identity itself (unix-socket local
// trust, see scopeFor). The agent-control verbs get the identical reasoning
// from the route's policy interceptor (cmd/rafikid/connect_policy.go), which
// refuses them before these handlers run.
var errNotAUserCredential = errors.New("conversation queries require a user credential")

// scopeFor is the ONE place the Connect plane turns an authenticated
// identity into an insights.Scope. See design doc §3: IsUserCredential, not
// a non-empty UserID (a child-attributed identity carries its owner's
// UserID and must never inherit that owner's scope); IsAdmin is a column
// read that only Authenticate ever sets.
//
// A nil identity is the unix socket's local trust, not a missing credential:
// tokenAuth.Middleware refuses a request with no Authorization header before
// it ever reaches a Connect handler on the proxy face (TestScopeForNilIsUnreachableOffTheUDS
// proves this), so nil can only arrive here via the UDS mount's optional
// identity interceptor. It resolves ScopeAll, the same inference the framed
// plane already makes for an unauthenticated local caller -- without this a
// token-less local profile loses `rafiki conversations` entirely.
func scopeFor(ctx context.Context) (insights.Scope, error) {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return insights.ScopeAll(), nil
	}
	if !id.IsUserCredential() {
		return insights.Scope{}, connect.NewError(connect.CodePermissionDenied, errNotAUserCredential)
	}
	if id.IsAdmin {
		return insights.ScopeAll(), nil
	}
	return insights.ScopeOwner(id.UserID), nil
}

// childConversationScope is scopeFor for the conversation reads a per-child
// credential may make (ConversationSearch/Export/Query): that caller reads
// its own subtree through insights.ScopeSubtree — the MCP face's
// newMCPChildConversationReader boundary — never its owner's corpus. Every
// other identity resolves through scopeFor unchanged, so the per-boot child
// shapes stay refused here as they are at the gate.
func childConversationScope(ctx context.Context, c *Controller) (insights.Scope, error) {
	if id := server.IdentityFromContext(ctx); id != nil && id.Via == server.ProvenanceChildToken {
		sel, err := c.subtreeSelector(ctx, id.ChildID)
		if err != nil {
			// Fail CLOSED, never empty-and-allowed: a lineage that cannot be read
			// must not shrink the boundary to nothing (that would look like "no
			// rows", i.e. no authority) NOR widen it to the owner's corpus. An
			// error the verb answers is the only shape that is neither.
			return insights.Scope{}, err
		}
		return insights.ScopeSubtree(sel), nil
	}
	return scopeFor(ctx)
}

// connectConversations adapts *Controller to connectapi.ConversationInsights.
type connectConversations struct{ c *Controller }

func (a connectConversations) Search(ctx context.Context, f connectapi.ConversationSearchFilter) ([]connectapi.ConversationSummaryRow, error) {
	scope, err := childConversationScope(ctx, a.c)
	if err != nil {
		return nil, err
	}
	rows, err := a.c.ConversationSearch(ctx, scope, insights.SearchFilter{
		Since: unixToTimePtr(f.SinceUnix), Until: unixToTimePtr(f.UntilUnix),
		Owner: f.Owner, Persona: f.Persona, Source: f.Source, Model: f.Model,
		Status: f.Status, Path: insights.Path(f.Path), MinTokens: f.MinTokens,
		Text: f.Text, Limit: f.Limit, Closed: f.Closed,
	})
	if err != nil {
		logIfUncoded("connect: conversation search failed", err)
		return nil, connectapi.ConnectErr(err)
	}
	out := make([]connectapi.ConversationSummaryRow, 0, len(rows))
	for _, r := range rows {
		row := connectapi.ConversationSummaryRow{
			ID: r.ID, Name: r.Name, Owner: r.Owner, Persona: r.Persona, Source: r.Source,
			Model: r.Model, Status: r.Status, DrivenBy: r.DrivenBy, CreatedAtUnix: r.CreatedAt.Unix(),
			Turns: r.Turns, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens, CacheHitRatio: r.CacheHitRatio, TotalCostUSD: r.TotalCostUSD,
			FirstMessage: r.FirstMessage,
		}
		// ClosedAt is the ONLY signal a row was closed; ClosedAtUnix stays nil
		// (proto optional unset) for an open conversation, never 0, so "closed at
		// the epoch" and "still open" cannot collapse into each other.
		if r.ClosedAt != nil {
			unix := r.ClosedAt.Unix()
			row.ClosedAtUnix = &unix
		}
		out = append(out, row)
	}
	return out, nil
}

func (a connectConversations) Export(ctx context.Context, conversationID string) (connectapi.TranscriptRow, bool, error) {
	scope, err := childConversationScope(ctx, a.c)
	if err != nil {
		return connectapi.TranscriptRow{}, false, err
	}
	tr, err := a.c.ConversationExport(ctx, scope, conversationID)
	if conversationReadNotFound(err) {
		return connectapi.TranscriptRow{}, false, nil
	}
	if err != nil {
		logIfUncoded("connect: conversation export failed", err)
		return connectapi.TranscriptRow{}, false, connectapi.ConnectErr(err)
	}
	turns := make([]connectapi.TranscriptTurnRow, 0, len(tr.Turns))
	for _, t := range tr.Turns {
		turns = append(turns, connectapi.TranscriptTurnRow{
			Ordinal: t.Ordinal, Role: t.Role, Content: t.Content, Skills: t.Skills,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheReadTokens: t.CacheReadTokens,
			LatencyMS: t.LatencyMS, Model: t.Model, PrefixHash: t.PrefixHash,
			ServedProvider: t.ServedProvider,
		})
	}
	return connectapi.TranscriptRow{
		ConversationID: tr.ConversationID, Owner: tr.Owner, Persona: tr.Persona,
		Source: tr.Source, DrivenBy: tr.DrivenBy, Turns: turns, AvailableSkills: tr.AvailableSkills,
	}, true, nil
}

// RunQuery adapts Controller.ConversationQuery onto connectapi's catalogue
// mirror types. Scope is derived HERE, from the caller's own identity -- the
// wire carries a query name and a filter, never a scope. An Entry the switch
// does not name is a programming error upstream (insights.Entry's marker
// interface keeps a stray value out at compile time elsewhere); failing loud
// here beats guessing a cell's type.
func (a connectConversations) RunQuery(ctx context.Context, name string, f connectapi.CatalogueFilter) (connectapi.CatalogueResult, error) {
	scope, err := childConversationScope(ctx, a.c)
	if err != nil {
		return connectapi.CatalogueResult{}, err
	}
	res, err := a.c.ConversationQuery(ctx, scope, name, insights.StatsFilter{
		Since: unixToTimePtr(f.SinceUnix), Until: unixToTimePtr(f.UntilUnix),
		Owner: f.Owner, Persona: f.Persona, Source: f.Source, Model: f.Model,
		Path: insights.Path(f.Path),
	})
	if err != nil {
		logIfUncoded("connect: conversation query failed", err)
		return connectapi.CatalogueResult{}, connectapi.ConnectErr(err)
	}
	cols := make([]connectapi.QueryColumnMeta, 0, len(res.Columns))
	for _, c := range res.Columns {
		kind := "string"
		switch c.Kind {
		case insights.ColInt:
			kind = "int"
		case insights.ColFloat:
			kind = "float"
		}
		cols = append(cols, connectapi.QueryColumnMeta{Name: c.Name, Kind: kind, Format: c.Format})
	}
	rows := make([][]connectapi.QueryRowValue, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := make([]connectapi.QueryRowValue, 0, len(r))
		for _, e := range r {
			switch v := e.(type) {
			case insights.IntEntry:
				row = append(row, connectapi.QueryRowValue{Int: int64(v), IsInt: true})
			case insights.FloatEntry:
				row = append(row, connectapi.QueryRowValue{Float: float64(v), IsFloat: true})
			case insights.StringEntry:
				row = append(row, connectapi.QueryRowValue{Str: string(v)})
			default:
				return connectapi.CatalogueResult{}, fmt.Errorf("connect_adapters: unhandled insights.Entry type %T", e)
			}
		}
		rows = append(rows, row)
	}
	return connectapi.CatalogueResult{Columns: cols, Rows: rows}, nil
}

// conversationReadNotFound reports whether err is the Controller's not-found
// answer for a conversation read. The Controller translates
// insights.ErrNotFound into a *connectapi.ControllerError carrying only a
// message string -- ControllerError does not Unwrap its original -- so
// errors.Is against the sentinel never matches through the translation and
// must be paired with the code comparison dispatch's mapErr uses. A bare
// errors.Is check would send every scope miss and every missing id down the
// CodeInternal path, turning the design's "a scope miss reads as not-found"
// rule into "reads as a server error". The sentinel branch stays first so a
// future Controller that passes the sentinel through untouched still
// matches.
func conversationReadNotFound(err error) bool {
	if errors.Is(err, insights.ErrNotFound) {
		return true
	}
	var ce *connectapi.ControllerError
	if errors.As(err, &ce) {
		return ce.Code == protocol.ErrNotFound
	}
	return false
}

// logIfUncoded logs err's cause when it is NOT a *connectapi.ControllerError --
// connectapi.ConnectErr redacts an uncoded error to a fixed "internal error"
// text and does not log, so the cause is lost unless the call site logs it
// first (the same rule pkg/connectapi's own SetBudget and Close follow).
// A ControllerError's message is already curated for the wire and reaches the
// caller unredacted, so logging it here would be noise.
func logIfUncoded(msg string, err error) {
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		slog.Error(msg, "error", err)
	}
}

// connectReview adapts *Controller to connectapi.ConversationReviewer. Scope
// is derived HERE, from the caller's own credential — the wire carries no
// scope, matching every conversation verb above.
type connectReview struct{ c *Controller }

func (a connectReview) Review(ctx context.Context, req connectapi.ReviewRequest) ([]connectapi.ReviewAccept, error) {
	scope, err := scopeFor(ctx)
	if err != nil {
		return nil, err
	}
	return a.c.ConversationReview(ctx, scope, req)
}

// connectFindingsReader adapts *Controller to connectapi.
// ConversationFindingsReader. Same scope rule: derived from the credential,
// never carried on the wire.
type connectFindingsReader struct{ c *Controller }

func (a connectFindingsReader) Findings(ctx context.Context, f connectapi.ReviewFindingsFilter) ([]connectapi.ReviewFinding, error) {
	scope, err := scopeFor(ctx)
	if err != nil {
		return nil, err
	}
	findings, _, err := a.c.ConversationFindings(ctx, scope, f)
	return findings, err
}

func (a connectFindingsReader) RecentAnalyses(ctx context.Context, conversationIDs []string, limit int) ([]connectapi.ReviewAnalysis, error) {
	scope, err := scopeFor(ctx)
	if err != nil {
		return nil, err
	}
	_, analyses, err := a.c.ConversationFindings(ctx, scope, connectapi.ReviewFindingsFilter{ConversationIDs: conversationIDs, Limit: limit})
	return analyses, err
}

// unixToTimePtr converts a wire Unix-seconds value to *time.Time, treating 0
// as unset -- matches pkg/control/dispatch.go's unixToTime (duplicated here
// rather than exported, since pkg/control and cmd/rafikid have no shared
// leaf package for it and it is three lines).
func unixToTimePtr(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}
