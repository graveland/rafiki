// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/quota"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/users"
)

// nativeEventSource adapts the nativebus registry to connectapi.EventSource,
// avoiding a method-name collision with Controller.Subscribe (which already
// exists with a different signature for the frame-based control protocol).
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
// The Controller's own Subscribe method serves the frame-based protocol;
// this is a distinct signature for the Connect-based StreamEvents path.
func (c *Controller) nativeEventSource() *nativeEventSource {
	return &nativeEventSource{native: c.native}
}

// ListChildren satisfies connectapi.ChildLister. An empty statuses means no
// filter.
//
// The Snapshot -> ChildSummary mapping is control.SnapshotToSummary, not a
// local reimplementation: it joins provider and model, nils the PID for an
// exited child, converts time.Time to Unix millis, and sources the context
// window through the catalog func. Every one of those is easy to get subtly
// wrong by hand.
func (c *Controller) ListChildren(statuses []string) []protocol.ChildSummary {
	snaps := c.List(protocol.ListFilter{})
	kept := make([]childstore.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		if len(statuses) > 0 && !containsString(statuses, string(s.Status)) {
			continue
		}
		kept = append(kept, s)
	}
	// ONE rollup for the whole list. Pricing each child with its own
	// SubtreeCost call meant N serial round trips, each with its own timeout,
	// on the cockpit's seed path -- and the seed is re-run whenever an unknown
	// child produces traffic, which is exactly the busy-fleet case where N is
	// large.
	costs := c.costsFor(kept)

	out := make([]protocol.ChildSummary, 0, len(kept))
	for _, s := range kept {
		sum := control.SnapshotToSummary(s, c.ContextWindow)
		if cost, ok := costs[s.ChildID]; ok {
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
	sum := control.SnapshotToSummary(snap, c.ContextWindow)
	if cost, ok := c.costsFor([]childstore.Snapshot{snap})[childID]; ok {
		sum.CostUSD = &cost
	}
	return sum, true
}

// Costs satisfies control.Controller for the framed protocol's ctrl_list and
// ctrl_get -- the same batched rollup ListChildren/GetChild already use for
// the Connect plane, so cost_usd now populates on both surfaces from one
// implementation.
func (c *Controller) Costs(snaps []childstore.Snapshot) map[string]float64 {
	return c.costsFor(snaps)
}

// costRollupTimeout bounds the whole batch, not one child. It is a display
// number: a slow rollup must not hold up the list it decorates.
const costRollupTimeout = 3 * time.Second

// costsFor prices every child in one round trip, keyed by child id.
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
func (c *Controller) costsFor(snaps []childstore.Snapshot) map[string]float64 {
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

	ctx, cancel := context.WithTimeout(context.Background(), costRollupTimeout)
	defer cancel()
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
	if err := requireUserCredential(ctx); err != nil {
		return "", err
	}
	req := protocol.SpawnRequest{
		Cwd:              p.Cwd,
		Name:             p.Name,
		Model:            p.Model,
		Kind:             p.Kind,
		Labels:           p.Labels,
		ParentChildID:    p.ParentChildID,
		ExecutorSelector: p.ExecutorSelector,
		ExecutorRef:      p.ExecutorRef,
		MaxDepth:         p.MaxDepth,
		MaxCost:          p.MaxCost,
		MaxChildren:      p.MaxChildren,
	}
	// The owner is read from the CONTEXT, never the request: it is matched by
	// executor admission selectors, so a client that could name it could claim
	// to be any owner. This mirrors dispatcher.spawn reading conn.Identity().
	//
	// server.UserTokenAuth.Middleware is what put it there — the Connect
	// routes mount inside the proxy face's middleware stack
	// (server.Handler.Mount, wired in proxy.go), so a remote caller's
	// credential has already been resolved to a user by the time a handler
	// runs. The zero value is correct and expected on the unix socket, which
	// authenticates nobody because the socket itself is the credential.
	//
	// This was hardcoded to users.Identity{}, which was invisible while the
	// only reachable mount was that socket.
	res, err := l.c.Spawn(ctx, req, spawnOwner(ctx))
	if err != nil {
		return "", err
	}
	return res.ChildID, nil
}

func (l connectLifecycle) Kill(ctx context.Context, childID string, shutdownMs, killMs int64) (connectapi.KillOutcome, error) {
	res, err := l.c.Kill(ctx, childID, shutdownMs, killMs)
	if err != nil {
		return connectapi.KillOutcome{}, err
	}
	return connectapi.KillOutcome{
		ExitCode:   res.ExitCode,
		Signal:     res.Signal,
		DurationMs: res.DurationMs,
		Escalated:  res.Escalated,
	}, nil
}

func (l connectLifecycle) SetBudget(ctx context.Context, childID string, maxCost float64) error {
	return l.c.SetChildBudgetAsOperator(ctx, childID, maxCost)
}

// connectModels adapts *Controller to connectapi.ModelLister. A distinct type
// for the same reason connectLifecycle is one: Controller.ListModels already
// exists with a different signature (it answers the framed ctrl_list_models),
// and renaming it would touch every existing caller for no gain.
type connectModels struct{ c *Controller }

func (m connectModels) ListModels(ctx context.Context, provider, kind string) ([]connectapi.ModelRow, error) {
	return m.c.ListModelRows(ctx, provider, kind)
}

// connectExecutors adapts *Controller to connectapi.ExecutorLister.
type connectExecutors struct{ c *Controller }

func (e connectExecutors) ListExecutors(ctx context.Context, kind string) ([]connectapi.ExecutorRow, error) {
	// The row set is scoped BY the caller's identity — "the executors this
	// owner could spawn onto" — so a child-attributed caller would be reading
	// its owner's fleet, the same borrowed identity Spawn refuses.
	if err := requireUserCredential(ctx); err != nil {
		return nil, err
	}
	return e.c.ListExecutorRows(ctx, kind, spawnOwner(ctx).Username)
}

// connectSkills adapts the daemon's skills.Store to connectapi.SkillManager.
// The translation exists so pkg/connectapi — which cmd/rafiki links — never
// imports pkg/skills' store half. version is the daemon's build version, used
// to stamp shadowed_core_version on core overrides.
type connectSkills struct {
	st      skills.Store
	version string
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
	return skillRowFrom(out), nil
}

func (c connectSkills) DeleteSkill(ctx context.Context, ns, name string) error {
	return translateSkillErr(c.st.Delete(ctx, ns, name))
}

func (c connectSkills) SetSkillEnabled(ctx context.Context, ns, name string, enabled bool) error {
	return translateSkillErr(c.st.SetEnabled(ctx, ns, name, enabled))
}

func (l connectLifecycle) Close(_ context.Context, childID string) error {
	return l.c.Close(childID)
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

// requireUserCredential is S1's Connect-plane gate: a verb that acts or
// answers AS the caller's identity requires that identity to come from a real
// user credential. A child-attributed identity carries the owner's UserID —
// that is the attribution path /v1/messages bills turns through — so a
// non-empty-UserID check cannot stand in for provenance here either.
//
// IdentityFromContext returning nil is NOT refused: on the unix socket the
// socket itself is the credential and the caller is anonymous (spawns land
// unowned, executor rows list unscoped), exactly as before provenance
// existed. Every presented credential that is not a user credential —
// child-attributed, or one resolving to the zero identity — is refused with a
// named permission error, because an RPC caller should see why.
func requireUserCredential(ctx context.Context) error {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.IsUserCredential() {
		return nil
	}
	if id.Via == server.ProvenanceChildAttributed {
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("agent-control verbs require a user credential; this identity is child-attributed"))
	}
	return connect.NewError(connect.CodePermissionDenied,
		errors.New("agent-control verbs require a user credential; the presented credential does not resolve to one"))
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

// errNotAUserCredential is scopeFor's refusal for any identity that is not a
// real user credential -- the only provenance the design's Scope mechanism
// accepts. See requireUserCredential for the identical reasoning applied to
// the agent-control verbs.
var errNotAUserCredential = errors.New("conversation queries require a user credential")

// scopeFor is the ONE place the Connect plane turns an authenticated
// identity into an insights.Scope. See design doc §3: IsUserCredential, not
// a non-empty UserID (a child-attributed identity carries its owner's
// UserID and must never inherit that owner's scope); IsAdmin is a column
// read that only Authenticate ever sets.
func scopeFor(ctx context.Context) (insights.Scope, error) {
	id := server.IdentityFromContext(ctx)
	if id == nil || !id.IsUserCredential() {
		return insights.Scope{}, connect.NewError(connect.CodePermissionDenied, errNotAUserCredential)
	}
	if id.IsAdmin {
		return insights.ScopeAll(), nil
	}
	return insights.ScopeOwner(id.UserID), nil
}

// connectConversations adapts *Controller to connectapi.ConversationInsights.
type connectConversations struct{ c *Controller }

func (a connectConversations) Search(ctx context.Context, f connectapi.ConversationSearchFilter) ([]connectapi.ConversationSummaryRow, error) {
	scope, err := scopeFor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := a.c.ConversationSearch(ctx, scope, insights.SearchFilter{
		Since: unixToTimePtr(f.SinceUnix), Until: unixToTimePtr(f.UntilUnix),
		Owner: f.Owner, Persona: f.Persona, Source: f.Source, Model: f.Model,
		Status: f.Status, Path: insights.Path(f.Path), MinTokens: f.MinTokens,
		Text: f.Text, Limit: f.Limit,
	})
	if err != nil {
		return nil, controllerConnectError(err)
	}
	out := make([]connectapi.ConversationSummaryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, connectapi.ConversationSummaryRow{
			ID: r.ID, Name: r.Name, Owner: r.Owner, Persona: r.Persona, Source: r.Source,
			Model: r.Model, Status: r.Status, DrivenBy: r.DrivenBy, CreatedAtUnix: r.CreatedAt.Unix(),
			Turns: r.Turns, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CacheReadTokens: r.CacheReadTokens, CacheHitRatio: r.CacheHitRatio, TotalCostUSD: r.TotalCostUSD,
			FirstMessage: r.FirstMessage,
		})
	}
	return out, nil
}

func (a connectConversations) Export(ctx context.Context, conversationID string) (connectapi.TranscriptRow, bool, error) {
	scope, err := scopeFor(ctx)
	if err != nil {
		return connectapi.TranscriptRow{}, false, err
	}
	tr, err := a.c.ConversationExport(ctx, scope, conversationID)
	if conversationReadNotFound(err) {
		return connectapi.TranscriptRow{}, false, nil
	}
	if err != nil {
		return connectapi.TranscriptRow{}, false, controllerConnectError(err)
	}
	turns := make([]connectapi.TranscriptTurnRow, 0, len(tr.Turns))
	for _, t := range tr.Turns {
		turns = append(turns, connectapi.TranscriptTurnRow{
			Ordinal: t.Ordinal, Role: t.Role, Content: t.Content, Skills: t.Skills,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheReadTokens: t.CacheReadTokens,
			LatencyMS: t.LatencyMS, Model: t.Model, PrefixHash: t.PrefixHash,
		})
	}
	return connectapi.TranscriptRow{
		ConversationID: tr.ConversationID, Owner: tr.Owner, Persona: tr.Persona,
		Source: tr.Source, DrivenBy: tr.DrivenBy, Turns: turns, AvailableSkills: tr.AvailableSkills,
	}, true, nil
}

// conversationReadNotFound reports whether err is the Controller's not-found
// answer for a conversation read. The Controller translates
// insights.ErrNotFound into a *control.ControllerError carrying only a
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
	var ce *control.ControllerError
	if errors.As(err, &ce) {
		return ce.Code == protocol.ErrNotFound
	}
	return false
}

// controllerConnectError maps the Controller's *control.ControllerError onto
// a connect error so its curated Message reaches the peer under its protocol
// code. The ControllerError contract (pkg/control/dispatch.go) is that the
// codebase wrote the message -- the same promise mapErr honors on the framed
// plane -- so forwarding it is safe. Any error that is NOT a ControllerError
// is returned unchanged; the handler (connectapi.queryError) redacts it, so
// a pgx failure cannot name the database through this surface either. The
// translation lives HERE rather than in pkg/connectapi because that package
// must never reach pkg/control, which imports pkg/insights directly.
func controllerConnectError(err error) error {
	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		return err
	}
	return connect.NewError(controllerConnectCode(ce.Code), errors.New(ce.Message))
}

// controllerConnectCode translates the protocol codes a ControllerError can
// carry onto connect codes. ErrNoAgentDB is a daemon configuration gap, not
// a transient failure -- the request is fine and the operator must set
// RAFIKI_DB -- so FailedPrecondition, never Unavailable: a retry cannot
// heal it. Unrecognized codes stay Internal; their messages are still
// curated, so the text forwards unchanged.
func controllerConnectCode(code string) connect.Code {
	switch code {
	case protocol.ErrNotFound:
		return connect.CodeNotFound
	case protocol.ErrNoAgentDB:
		return connect.CodeFailedPrecondition
	default:
		return connect.CodeInternal
	}
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
