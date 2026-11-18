// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// executorAddedNotice tells an agent its executor set grew. With two or more
// members every workspace tool call must name one.
func executorAddedNotice(added string, members []string) string {
	return fmt.Sprintf("EXECUTOR ADDED: %s is now in your executor set (%s). With more than one executor, every workspace tool call must pass executor: \"<name>\".",
		added, strings.Join(members, ", "))
}

// executorAvailableNotice tells an agent whose set is all that its owner has
// a new executor.
func executorAvailableNotice(name string) string {
	return fmt.Sprintf("EXECUTOR ADDED: %s is now available to you (your executor set is all). Name it with executor: %q.", name, name)
}

// executorRemovedNotice tells an agent an executor it could address is gone.
func executorRemovedNotice(removed string, remaining []string) string {
	rest := "none"
	if len(remaining) > 0 {
		rest = strings.Join(remaining, ", ")
	}
	return fmt.Sprintf("EXECUTOR REMOVED: %s was removed and is no longer in your executor set. Remaining: %s.", removed, rest)
}

// executorSetNotifier is the state the set-maintenance paths share. mu
// serialises every read-modify-write of a child's stored set (install, uninstall
// and delete can overlap), and announced remembers which executors this daemon
// has already told `all` agents about, so a reconnect is not a new executor.
type executorSetNotifier struct {
	mu        sync.Mutex
	announced map[string]bool
}

// binderDisplayName is the name an operator and the model see for executorID:
// the one the binder's DisplayName gives, so a notice names what the tools do.
func (c *Controller) binderDisplayName(executorID string) string {
	return (&controllerBinder{c: c}).DisplayName(executorID)
}

// liveChildrenOf returns the snapshots of every child of ownerUserID that has
// not exited. An exited child cannot read a notice and its set is history.
func (c *Controller) liveChildrenOf(ownerUserID string) []childstore.Snapshot {
	if c.st == nil {
		return nil
	}
	var out []childstore.Snapshot
	for _, snap := range c.st.List() {
		if snap.Status == protocol.StatusExited || snap.OwnerUserID != ownerUserID {
			continue
		}
		out = append(out, snap)
	}
	return out
}

// materializeLegacySelector is the ids a legacy child's label selector reaches:
// the owner's executors whose labels match it. A sandbox is never reached by
// selection, and skipID (the executor being removed, whose row may still be
// visible) never counts.
func (c *Controller) materializeLegacySelector(snap childstore.Snapshot, skipID string) []string {
	if c.execStore == nil {
		return nil
	}
	sel, err := executors.ParseSelector(snap.ExecutorSelector)
	if err != nil {
		slog.Warn("executor set: a child's stored selector does not parse; its set starts empty",
			"child", snap.ChildID, "selector", snap.ExecutorSelector, "error", err)
		return nil
	}
	rows, err := c.execStore.List(context.Background())
	if err != nil {
		slog.Warn("executor set: could not list executors to materialize a selector",
			"child", snap.ChildID, "error", err)
		return nil
	}
	ids := []string{}
	for _, e := range rows {
		if e.OwnerUserID != snap.OwnerUserID || e.ID == skipID || isSandboxRow(e) {
			continue
		}
		if sel.Matches(e.Labels) {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// storeExecutorSet persists a child's new set, in memory and in its row. A
// failed row write is logged and does not fail the install or uninstall that
// caused it. It reports whether the in-memory set changed, which is what decides
// whether the child is told.
func (c *Controller) storeExecutorSet(childID string, set []string) bool {
	if err := c.st.SetExecutorSet(childID, set); err != nil {
		slog.Warn("executor set: could not update a child's set", "child", childID, "error", err)
		return false
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("executor set: could not persist a child's set", "child", childID, "error", err)
	}
	return true
}

func (c *Controller) displayNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.binderDisplayName(id))
	}
	return out
}

// addToExecutorSet puts executorID into childID's set and tells the child. A
// child whose set is all gets nothing here: the connect hook announces to it,
// once, for every executor however it arrived.
func (c *Controller) addToExecutorSet(childID, executorID string) {
	if c.st == nil {
		return
	}
	c.execSets.mu.Lock()
	defer c.execSets.mu.Unlock()

	snap, ok := c.st.Get(childID)
	if !ok || snap.Status == protocol.StatusExited {
		return
	}
	all, ids, selector := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector)
	if all {
		return
	}
	if selector != "" {
		ids = c.materializeLegacySelector(snap, "")
	}
	if !slices.Contains(ids, executorID) {
		ids = append(slices.Clone(ids), executorID)
	}
	if !c.storeExecutorSet(childID, ids) {
		return
	}
	c.sendSteer(childID, executorAddedNotice(c.binderDisplayName(executorID), c.displayNames(ids)))
}

// removeFromExecutorSets takes a removed executor out of every live child of its
// owner and tells each child that could address it. The row is already gone, so
// the caller passes its name and labels: a legacy selector child is recognised
// by the labels the row carried.
func (c *Controller) removeFromExecutorSets(executorID, ownerUserID, name string, labels map[string]string) {
	c.execSets.mu.Lock()
	defer c.execSets.mu.Unlock()
	delete(c.execSets.announced, executorID)

	for _, snap := range c.liveChildrenOf(ownerUserID) {
		all, ids, selector := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector)
		switch {
		case all:
			c.sendSteer(snap.ChildID, executorRemovedNotice(name, []string{childstore.ExecutorSetAll}))
		case selector != "":
			sel, err := executors.ParseSelector(selector)
			if err != nil || !sel.Matches(labels) {
				continue
			}
			rest := c.materializeLegacySelector(snap, executorID)
			if c.storeExecutorSet(snap.ChildID, rest) {
				c.sendSteer(snap.ChildID, executorRemovedNotice(name, c.displayNames(rest)))
			}
		case slices.Contains(ids, executorID):
			rest := make([]string, 0, len(ids))
			for _, id := range ids {
				if id != executorID {
					rest = append(rest, id)
				}
			}
			if c.storeExecutorSet(snap.ChildID, rest) {
				c.sendSteer(snap.ChildID, executorRemovedNotice(name, c.displayNames(rest)))
			}
		}
	}
}

// seedAnnouncedExecutors marks every executor that already has a row as
// announced, so a daemon restart does not tell `all` agents about executors
// they have always had. A listing failure logs and leaves the set empty: the
// cost is one spurious announcement per executor after a degraded startup.
func (c *Controller) seedAnnouncedExecutors(ctx context.Context) {
	c.execSets.mu.Lock()
	defer c.execSets.mu.Unlock()
	c.execSets.announced = map[string]bool{}
	if c.execStore == nil {
		return
	}
	rows, err := c.execStore.List(ctx)
	if err != nil {
		slog.Warn("executor set: could not seed the announced executors; "+
			"each executor may be announced once after this start", "error", err)
		return
	}
	for _, e := range rows {
		c.execSets.announced[e.ID] = true
	}
}

// announceIfNew tells every live `all` agent of the executor's owner about an
// executor the first time it connects. Sandboxes are never announced: they are
// reached by ownership, not by a set.
func (c *Controller) announceIfNew(executorID string) {
	c.execSets.mu.Lock()
	if c.execSets.announced == nil {
		c.execSets.announced = map[string]bool{}
	}
	if c.execSets.announced[executorID] {
		c.execSets.mu.Unlock()
		return
	}
	c.execSets.announced[executorID] = true
	c.execSets.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), installLookupTimeout)
	defer cancel()
	row, ok := c.executorRowByID(ctx, executorID)
	if !ok || isSandboxRow(row) {
		return
	}
	name := installNameOf(row)
	for _, snap := range c.liveChildrenOf(row.OwnerUserID) {
		if all, _, _ := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector); all {
			c.sendSteer(snap.ChildID, executorAvailableNotice(name))
		}
	}
}
