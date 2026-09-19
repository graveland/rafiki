// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"go.graveland.dev/rafiki/pkg/connectapi"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// connectGitSources adapts *Controller to connectapi.GitSourceManager. The
// owner comes from the request CONTEXT via spawnOwner — never a request
// field — the same rule connectPyModules follows, so registration is
// owner-scoped with no way to see or write another owner's sources.
//
// No credential gate is applied, deliberately, for the same reason
// connectPyModules carries none: an anonymous unix-socket caller writes the
// shared unattributed bucket, which is the same owner its own children
// resolve to, and a child-attributed caller is a legitimate pymodule-surface
// writer through its owner.
type connectGitSources struct{ c *Controller }

// AddGitSource registers (or repoints) the source, then fires the FIRST
// refresh synchronously as part of registration: `rafiki python repo add`
// reports the discovered inventory — or a clear failure — immediately, not
// an empty list the caller has to separately refresh to populate. A failed
// first refresh fails the call (the row stays registered; a retry is
// `python repo refresh`), because an operator who just typed a URL should
// hear about a bad clone right away, not on the first run that needs it.
func (m connectGitSources) AddGitSource(ctx context.Context, name, url, ref string) (connectapi.GitSourceRow, error) {
	owner := spawnOwner(ctx).UserID
	rec, err := m.c.gitpymoduleStore.Put(ctx, owner, name, url, ref)
	if err != nil {
		return connectapi.GitSourceRow{}, err // gitpymodules.ErrNotFound is impossible here
	}
	if m.c.gitpymodulePusher != nil {
		if _, err := m.c.gitpymodulePusher.refresh(ctx, owner, rec.Name, rec.URL, rec.Ref); err != nil {
			return connectapi.GitSourceRow{}, err
		}
	}
	return connectapi.GitSourceRow{Name: rec.Name, URL: rec.URL, Ref: rec.Ref}, nil
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

// RemoveGitSource deletes the registration outright. The pusher's cached
// inventory for the name is simply never read again — no eviction step is
// needed, and no executor is told (the cache directory's next refresh of
// anything re-derives what exists; there is no prune model for git sources).
func (m connectGitSources) RemoveGitSource(ctx context.Context, name string) error {
	return m.c.gitpymoduleStore.Delete(ctx, spawnOwner(ctx).UserID, name)
}

// compile-time pin: the adapter really satisfies the manager interface the
// Server's setter expects.
var _ connectapi.GitSourceManager = connectGitSources{}
