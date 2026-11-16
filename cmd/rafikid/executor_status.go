// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sort"

	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorStatusBinding implements tools.ExecutorLister for ONE caller.
//
// The same binding rule as executorInstallBinding and controllerSandboxes: the
// caller — a child id and its owner, or just an owner for a human — is closed
// over at construction and appears in no method signature, so a tool argument
// can never name a different owner or a different child's lineage.
func newExecutorStatusBinding(c *Controller, caller installCaller) tools.ExecutorLister {
	return &executorStatusBinding{c: c, caller: caller}
}

type executorStatusBinding struct {
	c      *Controller
	caller installCaller
}

func (b *executorStatusBinding) List(ctx context.Context) ([]protocol.ExecutorStatus, error) {
	return b.c.ExecutorStatuses(ctx, b.caller)
}

// ExecutorStatuses lists the executors ONE caller may inspect, with the pool's
// live health view. WHO SEES WHAT is decided here, not by the tool:
//
//   - A human caller (ChildID == "") sees the executors its owner owns — for a
//     non-admin credential exactly the rows `rafiki executor list` shows it,
//     since executorAuthority scopes that surface by the same durable id. An
//     ADMIN's `rafiki executor list` additionally shows the unowned rows; this
//     does not, because the listing is the read half of the install pair and
//     install authority is owner-scoped on both faces (installCaller carries no
//     admin bit). Widening here would show a machine the caller could not
//     install onto or remove.
//   - A child sees what its own provisioning can already affect: the executors
//     its LINEAGE installed (installedByLineage — the rule UninstallExecutor
//     enforces, so a machine it may remove is a machine it may see), plus the
//     executor its own child row is BOUND to, which is the machine it is
//     running on and can already run workspace tools on. A sibling's, a
//     descendant's or a stranger's machine is invisible, which keeps this
//     narrower than the owner-wide listing Connect refuses a child (its
//     ListExecutors is userOnly because a child reading its OWNER's fleet is the
//     borrowed identity Spawn refuses).
//
// Both rules also require the owner match every other verb on this daemon uses,
// so a recipe row pointing at a foreign executor cannot widen the listing.
//
// The result is sorted by name then id: a listing a model re-reads must not
// reshuffle under it because the store returned rows in another order.
func (c *Controller) ExecutorStatuses(
	ctx context.Context, caller installCaller,
) ([]protocol.ExecutorStatus, error) {
	if err := c.requireExecutorStore(); err != nil {
		return nil, err
	}
	execs, err := c.ExecutorList(protocol.ExecutorListRequest{})
	if err != nil {
		return nil, err
	}
	// One read answers both halves of the child scope: which executors this
	// lineage installed, and — after decoration — which launcher and ssh
	// destination each installed row came from.
	recipes := c.liveInstallRows(ctx)
	displays := c.installDisplaysFor(ctx, recipes)
	bound := c.boundExecutorID(caller.ChildID)
	launch := c.liveLaunchKinds()

	out := make([]protocol.ExecutorStatus, 0, len(execs))
	for _, e := range execs {
		if e.OwnerUserID != caller.OwnerUserID {
			continue
		}
		if caller.ChildID != "" && !c.executorVisibleToChild(caller.ChildID, e.ID, bound, recipes) {
			continue
		}
		st := protocol.ExecutorStatus{
			ID:            e.ID,
			Name:          e.Labels["machine"],
			Connected:     e.Connected,
			Enabled:       e.Enabled,
			ConnectedAt:   e.ConnectedAt,
			Isolation:     e.Isolation,
			WorkspaceMode: e.WorkspaceMode,
			Roots:         e.Roots,
			LaunchKinds:   launch[e.ID],
			Labels:        e.Labels,
			Annotations:   e.Annotations,
		}
		if !e.LastSeenAt.IsZero() {
			seen := e.LastSeenAt
			st.LastSeen = &seen
		}
		if d, ok := displays[e.ID]; ok {
			st.InstalledVia, st.InstalledTarget = d.Via, d.Target
			st.InstallMode, st.LastLaunch = d.Mode, d.LastLaunch
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// executorVisibleToChild is the child half of ExecutorStatuses' scope: the
// machine the child is bound to, or one an executor its own lineage installed.
func (c *Controller) executorVisibleToChild(
	childID, executorID, boundExecutorID string, recipes []executorinstall.Row,
) bool {
	if boundExecutorID != "" && executorID == boundExecutorID {
		return true
	}
	for _, r := range recipes {
		if r.ExecutorID == executorID && c.installedByLineage(childID, r.InstalledByChild) {
			return true
		}
	}
	return false
}

// boundExecutorID is the executor this child's workspace lives on, recorded by
// controllerBinder.NoteBinding. Empty when the child has no row (a caller whose
// credential names none) or has never been bound to a machine, which is the
// in-process case.
func (c *Controller) boundExecutorID(childID string) string {
	if c == nil || c.st == nil || childID == "" {
		return ""
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return ""
	}
	return snap.Labels[executorBindingLabel]
}

// liveLaunchKinds maps each live executor's id to the child kinds it
// self-reported it can launch. A listing that omitted this would tell a reader
// nothing about WHERE a claude child can go, which is the question a down
// machine raises next. A pool-less daemon answers an empty map: nothing is live,
// so nothing is launching.
func (c *Controller) liveLaunchKinds() map[string][]string {
	out := map[string][]string{}
	if c == nil || c.execPool == nil {
		return out
	}
	for _, le := range c.execPool.Live() {
		if kinds := le.Describe.GetLaunchKinds(); len(kinds) > 0 {
			out[le.Executor.ID] = kinds
		}
	}
	return out
}

// liveInstallRows reads every LIVE install recipe; nil when the installer is not
// wired or the read fails. A listing must not fail because provenance is
// unavailable — the same rule installDisplays follows.
func (c *Controller) liveInstallRows(ctx context.Context) []executorinstall.Row {
	if c == nil || c.install == nil || c.install.store == nil {
		return nil
	}
	rows, err := c.install.store.ListLiveAll(ctx)
	if err != nil {
		slog.Warn("could not read executor installs; listing executors without their launcher",
			"error", err)
		return nil
	}
	return rows
}
