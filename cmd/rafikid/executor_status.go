// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"slices"
	"sort"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executorinstall"
	"go.graveland.dev/rafiki/pkg/executors"
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
//   - A child sees exactly the owner's rows its own executor set admits: every
//     row the owner owns when the set is "all" (the human arm's row set), the
//     rows with those ids when it lists ids, and the rows a legacy selector
//     matches when the stored set is nil (a malformed selector shows nothing).
//     Connect refuses a child its owner's whole fleet (ListExecutors is
//     userOnly), so a child's listing is bounded by the same set that bounds
//     where it may run.
//
// Every arm also requires the owner match every other verb on this daemon
// uses, so no set or selector can show another owner's machine.
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
	recipes := c.liveInstallRows(ctx)
	displays := c.installDisplaysFor(ctx, recipes)
	var visible func(executors.Executor) bool
	if caller.ChildID != "" {
		visible = c.childExecutorFilter(caller.ChildID)
	}
	launch := c.liveLaunchKinds()

	out := make([]protocol.ExecutorStatus, 0, len(execs))
	for _, e := range execs {
		if e.OwnerUserID != caller.OwnerUserID {
			continue
		}
		if visible != nil && !visible(e) {
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

// childExecutorFilter is the child half of ExecutorStatuses' scope: it admits
// the rows the child's executor set admits. A child with no row, or one whose
// legacy selector does not parse, sees nothing; the owner match is the
// caller's.
func (c *Controller) childExecutorFilter(childID string) func(executors.Executor) bool {
	none := func(executors.Executor) bool { return false }
	if c == nil || c.st == nil {
		return none
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return none
	}
	all, ids, selector := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector)
	if all {
		return func(executors.Executor) bool { return true }
	}
	if selector != "" {
		sel, err := executors.ParseSelector(selector)
		if err != nil {
			return none
		}
		return func(e executors.Executor) bool { return sel.Matches(e.Labels) }
	}
	return func(e executors.Executor) bool { return slices.Contains(ids, e.ID) }
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
