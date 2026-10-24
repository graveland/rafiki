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
		sum := snapshotToSummary(s, c.ContextWindow, c.displayStateFor)
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
	sum := snapshotToSummary(snap, c.ContextWindow, c.displayStateFor)
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
//
// displayState, when non-nil, fills the operator-facing display tuple; callers
// pass Controller.displayStateFor. A nil func leaves the three fields empty.
func snapshotToSummary(snap childstore.Snapshot, contextWindow func(model string) (contextLen, maxCompletion int, ok bool), displayState func(childID string) (protocol.DisplayState, protocol.WaitingOn, string)) protocol.ChildSummary {
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
		LastActivity: snap.LastActivity,
		ExitCode:     snap.ExitCode,
		ExitSignal:   snap.ExitSignal,
		// The once-resolved routing spec, mirrored from the session. Model is
		// the BASE id — the spec lives only here and in the rafiki/routing
		// label.
		Routing: snap.Routing,
	}
	if displayState != nil {
		cs.DisplayState, cs.WaitingOn, cs.DisplayMsg = displayState(snap.ChildID)
	}
	if len(snap.Labels) > 0 {
		cs.Labels = snap.Labels
	}
	cs.ParentChildID, _ = childstore.ParentLabel(snap.Labels)
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
	snaps := c.orderedDescendants(childID)
	ids := make([]string, len(snaps))
	for i, snap := range snaps {
		ids[i] = snap.ChildID
	}
	return ids
}

// ClosableDescendantIDs satisfies connectapi.ClosableDescendantLister: the
// descendants of childID that are exited and have no live descendant beneath
// them, deepest first. An exited row above a live one is left out because
// closing it would strand the live child. Native thread children are excluded
// as in DescendantIDs.
func (c *Controller) ClosableDescendantIDs(childID string) []string {
	snaps := c.orderedDescendants(childID)
	held := make(map[string]bool, len(snaps))
	for _, snap := range snaps {
		if snap.Status == protocol.StatusExited {
			continue
		}
		for id := snap.ChildID; id != childID && !held[id]; {
			held[id] = true
			parent, ok := c.st.ParentOf(id)
			if !ok {
				break
			}
			id = parent
		}
	}
	var ids []string
	for _, snap := range snaps {
		if !held[snap.ChildID] {
			ids = append(ids, snap.ChildID)
		}
	}
	return ids
}

// orderedDescendants lists childID's non-native descendants deepest first,
// ties broken on id.
func (c *Controller) orderedDescendants(childID string) []childstore.Snapshot {
	type entry struct {
		snap  childstore.Snapshot
		depth int
	}
	var entries []entry
	for _, snap := range c.st.Descendants(childID) {
		if snap.Native {
			continue
		}
		entries = append(entries, entry{snap, c.st.DescendantDepth(childID, snap.ChildID)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].depth != entries[j].depth {
			return entries[i].depth > entries[j].depth
		}
		return entries[i].snap.ChildID < entries[j].snap.ChildID
	})
	out := make([]childstore.Snapshot, len(entries))
	for i, e := range entries {
		out[i] = e.snap
	}
	return out
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
		Sandbox:          p.Sandbox,
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

// Rename is operator authority: the policy gate admits only a user credential
// or the socket's nil identity.
func (l connectLifecycle) Rename(ctx context.Context, childID, name string) error {
	return l.c.Rename(ctx, childID, name)
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
// imports pkg/skills' store half.
type connectSkills struct {
	st skills.Store

	// push refreshes the executor-facing skill corpus after a successful
	// write (delete, enable/disable): push-on-write rather than a timer. Nil is
	// legal — tests and any wiring constructed before the pusher existed — and
	// means writes still succeed, they just don't fan out.
	push func(ctx context.Context)
}

func skillRowFrom(r skills.Record) connectapi.SkillRow {
	var files []connectapi.SkillFile
	for _, f := range r.Files {
		files = append(files, connectapi.SkillFile{
			Path: f.Path,
			Mode: f.Mode,
			Size: int64(len(f.Content)),
		})
	}
	return connectapi.SkillRow{
		ID:          r.ID,
		Namespace:   r.Namespace,
		Name:        r.Name,
		Description: r.Description,
		Body:        r.Body,
		Source:      r.Source,
		OwnerUserID: r.OwnerUserID,
		Layer:       skillLayerName(skills.LayerOf(r)),
		Enabled:     r.Enabled,
		Files:       files,
		UpdatedAt:   r.UpdatedAt,
	}
}

// translateSkillErr maps the store's sentinels onto connectapi's, so a missing
// skill stays an ANSWER (NotFound) rather than becoming an internal error.
func translateSkillErr(err error) error {
	if errors.Is(err, skills.ErrNotFound) {
		return connectapi.ErrSkillNotFound
	}
	return err
}

// skillLayerName is the wire spelling of a layer: "core", "global" or "user".
// The domain's Layer is an int, deliberately not shared with the wire, so the
// mapping lives in exactly one place.
func skillLayerName(l skills.Layer) string {
	switch l {
	case skills.LayerCore:
		return "core"
	case skills.LayerUser:
		return "user"
	default:
		return "global"
	}
}

// visibleSkills keeps the rows this caller may see: the core and global layers
// are shared, and a user row is visible only to its owner. Another owner's row
// is not addressable and not listed — its content is not this caller's
// business, and neither is its existence.
func visibleSkills(ctx context.Context, rows []skills.Record) []skills.Record {
	owner := spawnOwner(ctx).UserID
	out := make([]skills.Record, 0, len(rows))
	for _, r := range rows {
		switch skills.LayerOf(r) {
		case skills.LayerCore, skills.LayerGlobal:
			out = append(out, r)
		default:
			if owner != "" && r.OwnerUserID == owner {
				out = append(out, r)
			}
		}
	}
	return out
}

// skillRowKey identifies a row across the store's List and Resolve outputs.
// The id is the real key; the composite is a fallback for a record that has
// none (a store fake), so a shadowed computation can never collapse two
// different rows into one.
func skillRowKey(r skills.Record) string {
	if r.ID != "" {
		return r.ID
	}
	return r.Namespace + "\x00" + r.Name + "\x00" + r.Source + "\x00" + r.OwnerUserID
}

// highestLayerSkill picks the strongest visible row for ns:name: the caller's
// own row, then the global row, then the core row. It is the target of
// enable/disable and rm — those verbs act on a row, and the row a bare name
// means is the one the caller would see in effect.
func highestLayerSkill(rows []skills.Record, ns, name string) (skills.Record, bool) {
	var (
		best      skills.Record
		bestLayer skills.Layer
		found     bool
	)
	for _, r := range rows {
		if r.Namespace != ns || r.Name != name {
			continue
		}
		if l := skills.LayerOf(r); !found || l > bestLayer {
			best, bestLayer, found = r, l, true
		}
	}
	return best, found
}

// resolvedSkill finds the row a name resolves to — the one a child would
// actually load, namespace shadowing included.
func resolvedSkill(rows []skills.Record, owner, ns, name string) (skills.Record, bool) {
	for _, r := range skills.Resolve(rows, owner) {
		if r.Namespace == ns && r.Name == name {
			return r, true
		}
	}
	return skills.Record{}, false
}

// mayChangeSharedSkill reports whether this caller may write a shared row (the
// core and global layers, whose OwnerUserID is empty): the unix socket's nil
// identity is local trust, and a real user credential must be an admin. A
// child credential is never a user credential, so it can never satisfy this
// even if the users row it was minted from is an admin.
func mayChangeSharedSkill(ctx context.Context) bool {
	id := server.IdentityFromContext(ctx)
	return id == nil || (id.IsUserCredential() && id.IsAdmin)
}

// sharedSkillRefusal is the PermissionDenied every non-admin user gets for a
// shared row. It names the skill and both ways to be allowed, because "you may
// not" without a route forward is an unactionable error.
func sharedSkillRefusal(ns, name string) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
		"shared skill %s:%s can only be changed by an admin or over the local socket", ns, name))
}

// skillTarget resolves the row an enable/disable or rm acts on and applies the
// shared-row permission rule. NotFound rather than Forbidden when the row does
// not exist at all: an absent name is not a permission problem, and answering
// Forbidden would leak which names exist.
func (c connectSkills) skillTarget(ctx context.Context, ns, name string) (skills.Record, error) {
	rows, err := c.st.List(ctx, false)
	if err != nil {
		return skills.Record{}, err
	}
	target, ok := highestLayerSkill(visibleSkills(ctx, rows), ns, name)
	if !ok {
		return skills.Record{}, connectapi.ErrSkillNotFound
	}
	// Core and global are the shared layers; only a user row has an owner to
	// answer for it. LayerOf is consulted rather than the owner field so a core
	// row is shared even if it somehow carries an owner.
	if skills.LayerOf(target) != skills.LayerUser && !mayChangeSharedSkill(ctx) {
		return skills.Record{}, sharedSkillRefusal(ns, name)
	}
	return target, nil
}

// inEffectSkills returns the keys of the visible rows that actually resolve for
// this owner. A row is shadowed when it is enabled and absent from this set.
// One computation for both reads: `skills list` and `skills show` must never
// disagree about whether the same row is in effect.
func inEffectSkills(rows []skills.Record, owner string) map[string]bool {
	inEffect := make(map[string]bool, len(rows))
	for _, r := range skills.Resolve(rows, owner) {
		inEffect[skillRowKey(r)] = true
	}
	return inEffect
}

func (c connectSkills) ListSkills(ctx context.Context, includeDisabled bool) ([]connectapi.SkillRow, error) {
	// Every row, disabled included: shadowing is decided by the presence of a
	// row, so a disabled higher-layer override must be in hand even when the
	// caller asked for enabled rows only. Dropping it here would report the
	// row it hides as in effect.
	all, err := c.st.List(ctx, false)
	if err != nil {
		return nil, err
	}
	rows := visibleSkills(ctx, all)
	inEffect := inEffectSkills(rows, spawnOwner(ctx).UserID)
	out := make([]connectapi.SkillRow, 0, len(rows))
	for _, r := range rows {
		if !includeDisabled && !r.Enabled {
			continue
		}
		row := skillRowFrom(r)
		row.Shadowed = r.Enabled && !inEffect[skillRowKey(r)]
		out = append(out, row)
	}
	return out, nil
}

func (c connectSkills) GetSkill(ctx context.Context, ns, name string) (connectapi.SkillRow, error) {
	all, err := c.st.List(ctx, false)
	if err != nil {
		return connectapi.SkillRow{}, err
	}
	owner := spawnOwner(ctx).UserID
	rows := visibleSkills(ctx, all)
	target, ok := resolvedSkill(rows, owner, ns, name)
	if !ok {
		// The name is not in effect (a namespace a higher layer replaced, or a
		// disabled row). Show the row the caller can still act on rather than
		// pretending it does not exist.
		target, ok = highestLayerSkill(rows, ns, name)
	}
	if !ok {
		return connectapi.SkillRow{}, connectapi.ErrSkillNotFound
	}
	r, err := c.st.Get(ctx, target.ID)
	if err != nil {
		return connectapi.SkillRow{}, translateSkillErr(err)
	}
	row := skillRowFrom(r)
	// A fallback row is precisely the shadowed case, so carry the same flag
	// ListSkills computes rather than leaving show and list to disagree.
	row.Shadowed = r.Enabled && !inEffectSkills(rows, owner)[skillRowKey(r)]
	return row, nil
}

func (c connectSkills) DeleteSkill(ctx context.Context, ns, name string) error {
	target, err := c.skillTarget(ctx, ns, name)
	if err != nil {
		return translateSkillErr(err)
	}
	// The source marker is ownership: only a hand-authored or legacy imported
	// row is the user's to delete. A core row is disabled instead, and a repo
	// row goes when its repo does — deleting it here would silently resurrect
	// on the next refresh.
	switch {
	case target.Source == skills.CoreSource:
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			`%s:%s is a core skill; disable it with "rafiki skills disable %s:%s"`, ns, name, ns, name))
	case strings.HasPrefix(target.Source, "repo:"):
		repo := strings.TrimPrefix(target.Source, "repo:")
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			`%s:%s comes from repo %q; remove the repo with "rafiki repo remove %s"`, ns, name, repo, repo))
	case target.Source != "manual" && !strings.HasPrefix(target.Source, "import:"):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"%s:%s comes from source %q, which rafiki does not let you delete", ns, name, target.Source))
	}
	if err := c.st.Delete(ctx, target.ID); err != nil {
		return translateSkillErr(err)
	}
	if c.push != nil {
		c.push(ctx)
	}
	return nil
}

func (c connectSkills) SetSkillEnabled(ctx context.Context, ns, name string, enabled bool) error {
	target, err := c.skillTarget(ctx, ns, name)
	if err != nil {
		return translateSkillErr(err)
	}
	if err := c.st.SetEnabled(ctx, target.ID, enabled); err != nil {
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

// ClosableDescendantIDs satisfies connectapi.ClosableDescendantLister, asserted
// off the ChildLifecycle at runtime exactly like DescendantIDs above, so the
// method must exist HERE or every exited-descendant close is refused
// unimplemented.
func (l connectLifecycle) ClosableDescendantIDs(childID string) []string {
	return l.c.ClosableDescendantIDs(childID)
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
		Since: f.Since, Until: f.Until,
		Owner: f.Owner, Persona: f.Persona, Source: f.Source, Model: f.Model,
		Status: f.Status, Path: insights.Path(f.Path), MinTokens: f.MinTokens,
		Text: f.Text, Limit: f.Limit,
	})
	if err != nil {
		logIfUncoded("connect: conversation search failed", err)
		return nil, connectapi.ConnectErr(err)
	}
	out := make([]connectapi.ConversationSummaryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, connectapi.ConversationSummaryRow{
			ID: r.ID, Name: r.Name, Owner: r.Owner, Persona: r.Persona, Source: r.Source,
			Model: r.Model, Status: r.Status, DrivenBy: r.DrivenBy, CreatedAt: r.CreatedAt,
			ClosedAt: r.ClosedAt, Turns: r.Turns,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens, CacheHitRatio: r.CacheHitRatio, TotalCostUSD: r.TotalCostUSD,
			FirstMessage: r.FirstMessage,
		})
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
		// A child id that resolves to nothing carries a remedy (retry once the
		// child's first request is captured); a conversation id stays the
		// bare "not found" so scope misses are indistinguishable from absence.
		if strings.HasPrefix(conversationID, "c_") {
			return connectapi.TranscriptRow{}, false, connectapi.ConnectErr(err)
		}
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
			Latency: msPtrDuration(t.LatencyMS), Model: t.Model, PrefixHash: t.PrefixHash,
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
		Since: f.Since, Until: f.Until,
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

// msPtrDuration converts an optional millisecond count onto an optional
// time.Duration, keeping nil (not reported) distinct from a measured zero.
func msPtrDuration(ms *int) *time.Duration {
	if ms == nil {
		return nil
	}
	d := time.Duration(*ms) * time.Millisecond
	return &d
}
