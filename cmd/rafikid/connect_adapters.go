// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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
	}

	ctx, cancel := context.WithTimeout(context.Background(), costRollupTimeout)
	defer cancel()
	rows, err := c.coster.CostsByConversation(ctx, sel)
	if err != nil {
		return nil
	}

	byConv := make(map[string]insights.ConversationCost, len(rows))
	byRef := make(map[string]insights.ConversationCost, len(rows))
	for _, r := range rows {
		byConv[r.ConversationID] = r
		if r.ExternalRef != "" {
			byRef[r.ExternalRef] = r
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
		out[s.ChildID] = total
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
