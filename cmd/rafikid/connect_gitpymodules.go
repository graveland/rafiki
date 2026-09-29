// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"

	"go.graveland.dev/rafiki/pkg/connectapi"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
)

// connectGitSources adapts *Controller to connectapi.GitSourceManager. The
// owner comes from the request CONTEXT via spawnOwner — never a request
// field — the same rule connectPyModules follows, so registration is
// owner-scoped with no way to see or write another owner's sources.
//
// No per-request credential check appears here for the same reason
// connectPyModules carries none: the route's policy interceptor
// (connect_policy.go) already refuses child credentials on these userOnly
// verbs, and the anonymous unix-socket caller writes the shared unattributed
// bucket, the same owner its own children resolve to.
type connectGitSources struct{ c *Controller }

// AddGitSource registers (or repoints) the source, then fires the FIRST
// refresh synchronously as part of registration: `rafiki python repo add`
// reports the discovered inventory — or a clear failure — immediately, not
// an empty list the caller has to separately refresh to populate. A failed
// first refresh fails the call AND leaves the registration as it was before
// the call: a source whose first refresh never succeeded must not linger as
// a registration with a known-bad clone (a retry is another add, not
// `python repo remove` first). No prior row → the registration is deleted
// outright; a prior row → it is Put back with its previous url/ref. A
// rollback failure is logged and never masks the refresh error.
func (m connectGitSources) AddGitSource(ctx context.Context, name, url, ref string) (connectapi.GitSourceRow, error) {
	owner := spawnOwner(ctx).UserID
	prior, err := priorGitSource(ctx, m.c.gitpymoduleStore, owner, name)
	if err != nil {
		return connectapi.GitSourceRow{}, err
	}
	rec, err := m.c.gitpymoduleStore.Put(ctx, owner, name, url, ref)
	if err != nil {
		return connectapi.GitSourceRow{}, err // gitpymodules.ErrNotFound is impossible here
	}
	if m.c.gitpymodulePusher != nil {
		if _, rerr := m.c.gitpymodulePusher.refresh(ctx, owner, rec.Name, rec.URL, rec.Ref); rerr != nil {
			m.rollbackGitSource(ctx, owner, name, prior, rerr)
			return connectapi.GitSourceRow{}, rerr
		}
	}
	return connectapi.GitSourceRow{Name: rec.Name, URL: rec.URL, Ref: rec.Ref}, nil
}

// priorGitSource returns the owner's existing registration for name, if any —
// the rollback anchor for a failed first refresh. The store has no Get; List
// is the only read it exposes (the same shape recordFor uses). A List error
// fails the add BEFORE any write: without the anchor a rollback could not
// tell a new source from a repointed one, and might delete a pre-existing
// registration it only meant to restore.
func priorGitSource(ctx context.Context, store gitpymodules.Store, ownerUserID, name string) (*gitpymodules.GitSourceRecord, error) {
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

// rollbackGitSource undoes a failed first refresh's write, restoring the
// registration to what it was before the call: no prior row → delete the
// registration; a prior row → Put its previous url/ref back (this git-source
// table is a pointer to repoint, not an append-only history — no other table
// is touched). A rollback failure is only logged: the caller already gets
// the refresh error, and a half-rolled-back registration is one
// `python repo remove` away, not a reason to mask the real failure.
func (m connectGitSources) rollbackGitSource(ctx context.Context, owner, name string, prior *gitpymodules.GitSourceRecord, refreshErr error) {
	var err error
	if prior == nil {
		err = m.c.gitpymoduleStore.Delete(ctx, owner, name)
	} else {
		_, err = m.c.gitpymoduleStore.Put(ctx, owner, name, prior.URL, prior.Ref)
	}
	if err != nil {
		slog.Error("git source add: rollback after a failed first refresh failed",
			"name", name, "owner", owner, "error", err)
	}
}

func (m connectGitSources) ListGitSources(ctx context.Context) ([]connectapi.GitSourceRow, error) {
	recs, err := m.c.gitpymoduleStore.List(ctx, spawnOwner(ctx).UserID)
	if err != nil {
		return nil, err
	}
	out := make([]connectapi.GitSourceRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, connectapi.GitSourceRow{Name: r.Name, URL: r.URL, Ref: r.Ref})
	}
	return out, nil
}

// RefreshGitSource re-runs the fan-out with the source's STORED url/ref —
// the refresh request carries only the name, and what gets cloned is the
// registration, never the caller's memory of it.
func (m connectGitSources) RefreshGitSource(ctx context.Context, name string) ([]connectapi.GitSourceScript, []connectapi.GitSourcePackage, bool, string, error) {
	owner := spawnOwner(ctx).UserID
	if m.c.gitpymodulePusher == nil {
		return nil, nil, false, "", errors.New("no executor pool: nothing to refresh a git source on")
	}
	rec, err := m.c.gitpymodulePusher.recordFor(ctx, owner, name)
	if err != nil {
		return nil, nil, false, "", err
	}
	inv, err := m.c.gitpymodulePusher.refresh(ctx, owner, rec.Name, rec.URL, rec.Ref)
	if err != nil {
		return nil, nil, false, "", err
	}
	return toConnectGitScripts(inv.Scripts), toConnectGitPackages(inv.Packages), inv.VenvReady, inv.VenvError, nil
}

// toConnectGitScripts/toConnectGitPackages lift the executor protocol's
// discovery messages into the connectapi face's own structs — the two wire
// formats stay strangers, the same way the control plane's GitSourceScript
// message duplicates the executor one instead of importing it.
func toConnectGitScripts(srcs []*executorpb.GitSourceScript) []connectapi.GitSourceScript {
	out := make([]connectapi.GitSourceScript, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, connectapi.GitSourceScript{Name: s.GetName(), Description: s.GetDescription()})
	}
	return out
}

func toConnectGitPackages(pkgs []*executorpb.GitSourcePackage) []connectapi.GitSourcePackage {
	out := make([]connectapi.GitSourcePackage, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, connectapi.GitSourcePackage{Name: p.GetName(), Description: p.GetDescription()})
	}
	return out
}

// RemoveGitSource deletes the registration outright, then evicts the
// pusher's cached inventory for the name — the cache readers never consult
// the store, so a kept entry would keep rendering on every "everything"
// surface (skill body, MCP pymodule_list, `python list`) until restart.
// No executor is told: the cache directory's next refresh of anything
// re-derives what exists; there is no prune model for git sources.
func (m connectGitSources) RemoveGitSource(ctx context.Context, name string) error {
	owner := spawnOwner(ctx).UserID
	if err := m.c.gitpymoduleStore.Delete(ctx, owner, name); err != nil {
		return err
	}
	if m.c.gitpymodulePusher != nil {
		m.c.gitpymodulePusher.evict(owner, name)
	}
	return nil
}

// compile-time pin: the adapter really satisfies the manager interface the
// Server's setter expects.
var _ connectapi.GitSourceManager = connectGitSources{}
