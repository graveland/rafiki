// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/repos"
	"go.graveland.dev/rafiki/pkg/skills"
)

// connectRepos adapts *Controller to connectapi.RepoManager. The
// owner comes from the request CONTEXT via spawnOwner — never a request
// field — the same rule connectPyModules follows, so registration is
// owner-scoped with no way to see or write another owner's sources.
//
// No per-request credential check appears here for the same reason
// connectPyModules carries none: the route's policy interceptor
// (connect_policy.go) already refuses child credentials on these userOnly
// verbs, and the anonymous unix-socket caller writes the shared unattributed
// bucket, the same owner its own children resolve to.
//
// sched is the per-repo timer set; it may be nil (a daemon with no executor
// pool schedules nothing), and every method on it tolerates that.
type connectRepos struct {
	c     *Controller
	sched *repoScheduler
}

// AddRepo registers (or repoints) the source, writes its refresh schedule, and
// then fires the FIRST refresh synchronously as part of registration: `rafiki
// repo add` reports the discovered inventory — or a clear failure —
// immediately, not an empty list the caller has to separately refresh to
// populate.
//
// The registration is rolled back to exactly what it was before the call when
// the first refresh fails, or when the skills half was refused because another
// source already holds one of the repo's namespaces: a source whose first
// refresh never succeeded must not linger as a registration with a known-bad
// clone, and a registration that cannot own its namespaces would be a repo
// that can never store its skills. Any OTHER skill error is reported in the
// response with the registration kept — the scripts half works, and the
// operator can fix the skill. No prior row → the registration is deleted
// outright; a prior row → it is Put back with its previous url/ref/interval. A
// rollback failure is logged and never masks the real error.
func (m connectRepos) AddRepo(ctx context.Context, name, url, ref string, refreshInterval time.Duration) (connectapi.RepoRow, connectapi.RepoRefresh, error) {
	owner := spawnOwner(ctx).UserID
	prior, err := priorRepo(ctx, m.c.repoStore, owner, name)
	if err != nil {
		return connectapi.RepoRow{}, connectapi.RepoRefresh{}, err
	}
	rec, err := m.c.repoStore.Put(ctx, owner, name, url, ref)
	if err != nil {
		return connectapi.RepoRow{}, connectapi.RepoRefresh{}, err // repos.ErrNotFound is impossible here
	}
	// The schedule is part of the registration, and the store is the one place
	// a bad interval is refused: write it before the refresh, so a registration
	// whose interval cannot be stored does not survive the call either.
	if refreshInterval > 0 {
		if err := m.c.repoStore.SetRefreshInterval(ctx, owner, name, refreshInterval); err != nil {
			m.rollbackRepo(context.WithoutCancel(ctx), owner, name, prior, err)
			return connectapi.RepoRow{}, connectapi.RepoRefresh{}, err
		}
		rec.RefreshInterval = refreshInterval
	}
	row := connectapi.RepoRow{
		Name: rec.Name, URL: rec.URL, Ref: rec.Ref, Commit: rec.Commit,
		RefreshInterval: rec.RefreshInterval, ResolvedAt: rec.ResolvedAt,
	}
	if m.c.repoPusher == nil {
		return row, connectapi.RepoRefresh{}, nil
	}
	res, rerr := m.c.repoPusher.refresh(ctx, owner, rec.Name, rec.URL, rec.Ref)
	if rerr != nil {
		// Rollback on a context that outlives the request: the most likely
		// way a first refresh fails is the CALLER disconnecting (Ctrl-C on
		// a long clone), which cancels ctx — a rollback on that dead ctx
		// would fail on every store call and leave the bad row (or the
		// known-good repoint overwritten) in place.
		m.rollbackRepo(context.WithoutCancel(ctx), owner, name, prior, rerr)
		return connectapi.RepoRow{}, connectapi.RepoRefresh{}, rerr
	}
	if res.NamespaceCollision() {
		// The collision's own text names the namespace and its holder, which is
		// what the operator needs to act on; the fallback only exists so a
		// flagged-but-textless result cannot panic here.
		text := "the repo's skills collide with another source's namespace"
		if len(res.SkillErrors) > 0 {
			text = res.SkillErrors[0]
		}
		collision := errors.New(text)
		m.rollbackRepo(context.WithoutCancel(ctx), owner, name, prior, collision)
		return connectapi.RepoRow{}, connectapi.RepoRefresh{}, collision
	}
	row.Skills = res.Skills
	m.sched.arm(owner, name)
	return row, toConnectRefresh(res), nil
}

// priorRepo returns the owner's existing registration for name, if any —
// the rollback anchor for a failed first refresh. The store has no Get; List
// is the only read it exposes (the same shape recordFor uses). A List error
// fails the add BEFORE any write: without the anchor a rollback could not
// tell a new source from a repointed one, and might delete a pre-existing
// registration it only meant to restore.
func priorRepo(ctx context.Context, store repos.Store, ownerUserID, name string) (*repos.Record, error) {
	recs, err := store.List(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.Name == name {
			rec := r
			return &rec, nil
		}
	}
	return nil, nil
}

// rollbackRepo undoes a failed registration write, restoring the registration
// to what it was before the call: no prior row → delete the registration; a
// prior row → Put its previous url/ref back and restore its schedule (this
// repo table is a pointer to repoint, not an append-only history — no other
// table is touched, and the skills half wrote nothing on the paths that roll
// back). A rollback failure is only logged: the caller already gets the real
// error, and a half-rolled-back registration is one `repo remove` away, not a
// reason to mask the real failure.
//
// The schedule is disarmed ONLY when the rollback removed a registration this
// call had created. A failed RE-add of an already-scheduled repo restores a row
// that is still registered, and its timer is still the one the earlier add
// armed: disarming it here would leave the repo with an interval in the store
// and no timer until the daemon restarts.
func (m connectRepos) rollbackRepo(ctx context.Context, owner, name string, prior *repos.Record, cause error) {
	var err error
	if prior == nil {
		err = m.c.repoStore.Delete(ctx, owner, name)
		// Nothing was registered before the call, so nothing may stay armed.
		m.sched.disarm(owner, name)
	} else {
		_, err = m.c.repoStore.Put(ctx, owner, name, prior.URL, prior.Ref)
		if err == nil {
			err = m.c.repoStore.SetRefreshInterval(ctx, owner, name, prior.RefreshInterval)
		}
		// The registration — and so its timer — predates this call: leave it
		// exactly as it was armed.
	}
	if err != nil {
		slog.Error("repo add: rollback after a failed registration failed",
			"name", name, "owner", owner, "error", err, "cause", cause)
	}
}

// ListRepos answers the caller's registrations with the state their last
// refresh left behind: the resolved commit and when, the opt-in interval, how
// many skills the source owns, and how many scripts its cached discovery
// reported. The skill count is one read for the whole list (not one per row),
// and the script count is a cache read — zero until this daemon has refreshed
// the source.
func (m connectRepos) ListRepos(ctx context.Context) ([]connectapi.RepoRow, error) {
	owner := spawnOwner(ctx).UserID
	recs, err := m.c.repoStore.List(ctx, owner)
	if err != nil {
		return nil, err
	}
	skillCounts, err := m.skillCounts(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]connectapi.RepoRow, 0, len(recs))
	for _, r := range recs {
		row := connectapi.RepoRow{
			Name: r.Name, URL: r.URL, Ref: r.Ref,
			Commit:          r.Commit,
			RefreshInterval: r.RefreshInterval,
			ResolvedAt:      r.ResolvedAt,
			Skills:          skillCounts[repoSource(r.Name)],
		}
		if m.c.repoPusher != nil {
			if inv, ok := m.c.repoPusher.inventoryFor(owner, r.Name); ok {
				row.Scripts = len(inv.Scripts)
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// skillCounts counts the owner's skill rows per source marker, in one read.
// The store's List is not owner-scoped, so the owner filter is here — and it
// is an exact match, so the unattributed bucket ("") sees the global layer's
// rows and no user's.
func (m connectRepos) skillCounts(ctx context.Context, ownerUserID string) (map[string]int, error) {
	if m.c.skillStore == nil {
		return nil, nil
	}
	recs, err := m.c.skillStore.List(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for _, r := range recs {
		if r.OwnerUserID == ownerUserID {
			out[r.Source]++
		}
	}
	return out, nil
}

// RefreshRepo re-runs the fan-out with the source's STORED url/ref —
// the refresh request carries only the name, and what gets cloned is the
// registration, never the caller's memory of it.
func (m connectRepos) RefreshRepo(ctx context.Context, name string) (connectapi.RepoRefresh, error) {
	owner := spawnOwner(ctx).UserID
	if m.c.repoPusher == nil {
		return connectapi.RepoRefresh{}, errors.New("no executor pool: nothing to refresh a repo on")
	}
	rec, err := m.c.repoPusher.recordFor(ctx, owner, name)
	if err != nil {
		return connectapi.RepoRefresh{}, err
	}
	res, err := m.c.repoPusher.refresh(ctx, owner, rec.Name, rec.URL, rec.Ref)
	if err != nil {
		return connectapi.RepoRefresh{}, err
	}
	return toConnectRefresh(res), nil
}

// SetRepo changes a registration's ref and/or its refresh interval, leaving
// whichever half the caller did not name exactly as it was.
//
// It does NOT refresh: a repoint takes effect on the next refresh — the
// operator's `repo refresh`, or the schedule's next fire — and the stored
// commit keeps describing the ref the row is leaving until then. Re-arming
// after the change is what makes a newly scheduled source fire (immediately,
// when it has never been resolved).
func (m connectRepos) SetRepo(ctx context.Context, req connectapi.RepoSetRequest) (connectapi.RepoRow, error) {
	owner := spawnOwner(ctx).UserID
	prior, err := priorRepo(ctx, m.c.repoStore, owner, req.Name)
	if err != nil {
		return connectapi.RepoRow{}, err
	}
	if prior == nil {
		return connectapi.RepoRow{}, repos.ErrNotFound
	}
	rec := *prior
	if req.Ref != nil && *req.Ref != rec.Ref {
		rec, err = m.c.repoStore.Put(ctx, owner, req.Name, rec.URL, *req.Ref)
		if err != nil {
			return connectapi.RepoRow{}, err
		}
	}
	if req.RefreshInterval != nil {
		if err := m.c.repoStore.SetRefreshInterval(ctx, owner, req.Name, *req.RefreshInterval); err != nil {
			return connectapi.RepoRow{}, err
		}
		rec.RefreshInterval = *req.RefreshInterval
	}
	m.sched.arm(owner, req.Name)
	row := connectapi.RepoRow{
		Name: rec.Name, URL: rec.URL, Ref: rec.Ref, Commit: rec.Commit,
		RefreshInterval: rec.RefreshInterval, ResolvedAt: rec.ResolvedAt,
	}
	if m.c.repoPusher != nil {
		if inv, ok := m.c.repoPusher.inventoryFor(owner, rec.Name); ok {
			row.Scripts = len(inv.Scripts)
		}
	}
	counts, err := m.skillCounts(ctx, owner)
	if err != nil {
		return connectapi.RepoRow{}, err
	}
	row.Skills = counts[repoSource(rec.Name)]
	return row, nil
}

// RemoveRepo deletes the source's skills, then the registration, then the
// pusher's cached inventory for the name, then pushes the corpus on.
//
// The skills go FIRST: the repo:<name> source marker is the only handle on
// them, and once the registration is gone nothing would name them again — they
// would keep shadowing every lower layer until an operator found them by hand.
// Nothing is sent to executors about the checkout itself: the cache
// directory's next refresh of anything re-derives what exists; there is no
// prune model for repos.
func (m connectRepos) RemoveRepo(ctx context.Context, name string) error {
	owner := spawnOwner(ctx).UserID
	changed := false
	if m.c.skillStore != nil {
		var err error
		changed, err = m.c.skillStore.PutSet(ctx, skills.SetKey{OwnerUserID: owner, Source: repoSource(name)}, nil)
		if err != nil {
			return err
		}
	}
	if err := m.c.repoStore.Delete(ctx, owner, name); err != nil {
		return err
	}
	m.sched.disarm(owner, name)
	if m.c.repoPusher != nil {
		m.c.repoPusher.evict(owner, name)
	}
	// Only a removal that actually dropped rows changes any executor's
	// corpus; an empty set is already what every executor holds.
	if changed && m.c.skillPusher != nil {
		m.c.skillPusher.pushAll(ctx)
	}
	return nil
}

// toConnectRefresh lifts the pusher's result into the connectapi face's own
// struct — the two wire formats stay strangers, the same way the control
// plane's RepoScript message duplicates the executor one instead of importing
// it.
func toConnectRefresh(res RefreshResult) connectapi.RepoRefresh {
	return connectapi.RepoRefresh{
		Commit:      res.Commit,
		Unchanged:   res.Unchanged,
		Scripts:     toConnectRepoScripts(res.Scripts),
		Packages:    toConnectRepoPackages(res.Packages),
		VenvReady:   res.VenvReady,
		VenvError:   res.VenvError,
		Skills:      res.Skills,
		SkillErrors: res.SkillErrors,
	}
}

// toConnectRepoScripts/toConnectRepoPackages lift the executor protocol's
// discovery messages into the connectapi face's own structs.
func toConnectRepoScripts(srcs []*executorpb.RepoScript) []connectapi.RepoScript {
	out := make([]connectapi.RepoScript, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, connectapi.RepoScript{Name: s.GetName(), Description: s.GetDescription()})
	}
	return out
}

func toConnectRepoPackages(pkgs []*executorpb.RepoPackage) []connectapi.RepoPackage {
	out := make([]connectapi.RepoPackage, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, connectapi.RepoPackage{Name: p.GetName(), Description: p.GetDescription()})
	}
	return out
}

// compile-time pin: the adapter really satisfies the manager interface the
// Server's setter expects.
var _ connectapi.RepoManager = connectRepos{}
