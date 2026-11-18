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
	"go.graveland.dev/rafiki/pkg/sandbox"
)

// executorSetLookupTimeout (a variable only so a test can shorten it) bounds every executor-store read the set
// maintenance paths make. An install or uninstall has already succeeded when
// they run, so a stuck store must cost a skipped update, never a hung call.
var executorSetLookupTimeout = installLookupTimeout

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

// executorRemovedUnknownRestNotice is the removal notice for a child whose
// remaining set could not be computed. It omits the Remaining line rather than
// state a set it does not know.
func executorRemovedUnknownRestNotice(removed string) string {
	return fmt.Sprintf("EXECUTOR REMOVED: %s was removed and is no longer in your executor set. Run executor_list to see what remains.", removed)
}

// executorDisabledNotice tells an `all` agent an executor it could name is
// switched off. No set changes: re-enabling restores it.
func executorDisabledNotice(name string) string {
	return fmt.Sprintf("EXECUTOR DISABLED: %s was disabled by your owner. Calls naming it fail until it is enabled again.", name)
}

// executorEnabledNotice tells an `all` agent a disabled executor is back.
func executorEnabledNotice(name string) string {
	return fmt.Sprintf("EXECUTOR ENABLED: %s was enabled again and is available to you (your executor set is all). Name it with executor: %q.", name, name)
}

// executorSetNotifier is the state the set-maintenance paths share. mu guards
// only the maps below and is never held across store or inbox I/O. announced
// remembers which executors this daemon has already told `all` agents about, so
// a reconnect is not a new executor. ordering serialises the notices about one
// executor (announce, removal, enable, disable) so a "removed" can never
// overtake the "available" that preceded it. persists tracks the background
// row writes.
type executorSetNotifier struct {
	mu        sync.Mutex
	announced map[string]bool
	ordering  map[string]*executorOrder
	persists  sync.WaitGroup
}

// executorOrder is one executor's ordering lock, reference counted so the map
// does not grow with every executor id ever seen.
type executorOrder struct {
	mu   sync.Mutex
	refs int
}

// lockExecutor serialises notices about executorID and returns the unlock.
func (n *executorSetNotifier) lockExecutor(executorID string) func() {
	n.mu.Lock()
	if n.ordering == nil {
		n.ordering = map[string]*executorOrder{}
	}
	o := n.ordering[executorID]
	if o == nil {
		o = &executorOrder{}
		n.ordering[executorID] = o
	}
	o.refs++
	n.mu.Unlock()

	o.mu.Lock()
	return func() {
		o.mu.Unlock()
		n.mu.Lock()
		o.refs--
		if o.refs == 0 {
			delete(n.ordering, executorID)
		}
		n.mu.Unlock()
	}
}

// markAnnounced records executorID as announced and reports whether it was new.
func (n *executorSetNotifier) markAnnounced(executorID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.announced == nil {
		n.announced = map[string]bool{}
	}
	if n.announced[executorID] {
		return false
	}
	n.announced[executorID] = true
	return true
}

func (n *executorSetNotifier) forgetAnnounced(executorID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.announced, executorID)
}

func (n *executorSetNotifier) wasAnnounced(executorID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.announced[executorID]
}

// binderDisplayName is the name an operator and the model see for executorID:
// the one the binder's DisplayName gives, so a notice names what the tools do.
func (c *Controller) binderDisplayName(executorID string) string {
	return (&controllerBinder{c: c}).DisplayName(executorID)
}

func (c *Controller) displayNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.binderDisplayName(id))
	}
	return out
}

// setOwnerOf is the owner whose executors a stored set can name. A native
// subagent row carries no owner of its own, so it takes its parent child's; a
// native row whose parent is gone has no attributable owner.
func (c *Controller) setOwnerOf(snap childstore.Snapshot) (string, bool) {
	if !snap.Native {
		return snap.OwnerUserID, true
	}
	parentID, ok := childstore.ParentLabel(snap.Labels)
	if !ok {
		return "", false
	}
	parent, ok := c.st.Get(parentID)
	if !ok {
		return "", false
	}
	return parent.OwnerUserID, true
}

// setHoldersOf returns every non-closed child whose stored set names
// ownerUserID's executors, exited and native subagent rows included: a
// resumable child comes back with the set on its row, so a removed id must not
// stay in it. A closed child is deleted from the store and never listed.
func (c *Controller) setHoldersOf(ownerUserID string) []childstore.Snapshot {
	if c.st == nil {
		return nil
	}
	var out []childstore.Snapshot
	for _, snap := range c.st.List() {
		if owner, ok := c.setOwnerOf(snap); ok && owner == ownerUserID {
			out = append(out, snap)
		}
	}
	return out
}

// noticeRecipient reports whether a notice should reach snap: only a live
// child with a process of its own. A native subagent shares its parent's
// process, and the parent child receives the notice.
func noticeRecipient(snap childstore.Snapshot) bool {
	return snap.Status != protocol.StatusExited && !snap.Native
}

// legacyMaterializer resolves legacy selector children to the executor ids
// their selector reaches, listing the owner's executors at most once and under
// a bounded deadline.
type legacyMaterializer struct {
	c      *Controller
	loaded bool
	rows   []executors.Executor
	err    error
}

// ids is the owner's non-sandbox executors snap's stored selector matches. ok is
// false when they cannot be determined (no store, an unparsable selector, a
// failed or timed-out listing); the caller then leaves the child untouched,
// because narrowing a legacy child on a guess destroys reach it had.
func (m *legacyMaterializer) ids(snap childstore.Snapshot) (ids []string, ok bool) {
	if m.c.execStore == nil {
		return nil, false
	}
	sel, err := executors.ParseSelector(snap.ExecutorSelector)
	if err != nil {
		slog.Warn("executor set: a child's stored selector does not parse; its set is left as it was",
			"child", snap.ChildID, "selector", snap.ExecutorSelector, "error", err)
		return nil, false
	}
	if !m.loaded {
		ctx, cancel := context.WithTimeout(context.Background(), executorSetLookupTimeout)
		m.rows, m.err = m.c.execStore.List(ctx)
		cancel()
		m.loaded = true
	}
	if m.err != nil {
		slog.Warn("executor set: could not list executors to materialize a selector; the child's set is left as it was",
			"child", snap.ChildID, "error", m.err)
		return nil, false
	}
	owner, ok := m.c.setOwnerOf(snap)
	if !ok {
		return nil, false
	}
	ids = []string{}
	for _, e := range m.rows {
		if e.OwnerUserID != owner || isSandboxRow(e) {
			continue
		}
		if sel.Matches(e.Labels) {
			ids = append(ids, e.ID)
		}
	}
	return ids, true
}

// persistExecutorSets writes the named children's rows in the background, one
// goroutine per call. The in-memory set is already updated, so a slow or failed
// write costs a stale stored row (healed by the next write of that row), never a
// hung or failed install, uninstall or on-connect hook.
func (c *Controller) persistExecutorSets(childIDs []string) {
	if len(childIDs) == 0 || c.children == nil {
		return
	}
	c.execSets.persists.Add(1)
	go func() {
		defer c.execSets.persists.Done()
		for _, id := range childIDs {
			if err := c.writeRecord(id); err != nil {
				slog.Warn("executor set: could not persist a child's set", "child", id, "error", err)
			}
		}
	}()
}

// addToExecutorSet puts executorID into childID's set and tells the child, once,
// when the set actually grew. A child whose set is all gets nothing here: the
// connect hook announces to it, once, for every executor however it arrived. A
// legacy selector child whose reach cannot be materialized is left untouched.
func (c *Controller) addToExecutorSet(childID, executorID string) {
	if c.st == nil {
		return
	}
	snap, ok := c.st.Get(childID)
	if !ok || snap.Status == protocol.StatusExited {
		return
	}
	all, ids, selector := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector)
	if all {
		return
	}
	var legacy []string
	if selector != "" {
		m := &legacyMaterializer{c: c}
		legacy, ok = m.ids(snap)
		if !ok {
			return
		}
		ids = legacy
	}
	added := !slices.Contains(ids, executorID)

	err := c.st.Update(childID, func(s *childstore.Session) {
		// The stored set is re-read under the row's own lock, so two overlapping
		// installs by one child both land.
		switch {
		case s.ExecutorSet == nil:
			s.ExecutorSet = withMember(legacy, executorID)
		case len(s.ExecutorSet) == 1 && s.ExecutorSet[0] == childstore.ExecutorSetAll:
			added = false
		case slices.Contains(s.ExecutorSet, executorID):
			added = false
		default:
			s.ExecutorSet = append(slices.Clone(s.ExecutorSet), executorID)
		}
	})
	if err != nil {
		slog.Warn("executor set: could not update a child's set", "child", childID, "error", err)
		return
	}
	if selector != "" || added {
		c.persistExecutorSets([]string{childID})
	}
	if !added {
		return
	}
	now, _ := c.st.Get(childID)
	c.sendSteer(childID, executorAddedNotice(c.binderDisplayName(executorID), c.displayNames(now.ExecutorSet)))
}

func withMember(ids []string, id string) []string {
	out := slices.Clone(ids)
	if out == nil {
		out = []string{}
	}
	if !slices.Contains(out, id) {
		out = append(out, id)
	}
	return out
}

// dropMember removes executorID from childID's stored set under the row's lock.
// A nil (legacy) set becomes materialized, which the caller computed. It reports
// the set afterwards and whether the id was in it.
func (c *Controller) dropMember(childID, executorID string, materialized []string) (rest []string, held bool) {
	err := c.st.Update(childID, func(s *childstore.Session) {
		cur := s.ExecutorSet
		if cur == nil {
			cur = materialized
			held = true
		} else {
			held = slices.Contains(cur, executorID)
		}
		if !held {
			return
		}
		rest = make([]string, 0, len(cur))
		for _, id := range cur {
			if id != executorID {
				rest = append(rest, id)
			}
		}
		s.ExecutorSet = slices.Clone(rest)
	})
	if err != nil {
		return nil, false
	}
	return rest, held
}

// removeFromExecutorSets takes a removed executor out of the stored set of every
// non-closed child of its owner (exited and native subagent rows included, since
// a resumed child reads the stored set) and notifies the live ones. The row is
// already gone, so the caller passes its name and labels: a legacy selector
// child is recognised by the labels the row carried.
//
// A sandbox row is reached by ownership, never by selection or by `all`, so only
// a child that holds it by id changes and is told.
func (c *Controller) removeFromExecutorSets(executorID, ownerUserID, name string, labels map[string]string) {
	unlock := c.execSets.lockExecutor(executorID)
	defer unlock()
	c.execSets.forgetAnnounced(executorID)
	if c.st == nil {
		return
	}
	isSandbox := isSandboxRow(executors.Executor{Labels: labels})

	var persist []string
	m := &legacyMaterializer{c: c}
	for _, snap := range c.setHoldersOf(ownerUserID) {
		live := noticeRecipient(snap)
		all, ids, selector := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector)
		switch {
		case all:
			if live && !isSandbox {
				c.sendSteer(snap.ChildID, executorRemovedNotice(name, []string{childstore.ExecutorSetAll}))
			}
		case selector != "":
			sel, err := executors.ParseSelector(selector)
			if isSandbox || err != nil || !sel.Matches(labels) {
				continue
			}
			rest, ok := m.ids(snap)
			if !ok {
				if live {
					c.sendSteer(snap.ChildID, executorRemovedUnknownRestNotice(name))
				}
				continue
			}
			rest, _ = c.dropMember(snap.ChildID, executorID, rest)
			if !snap.Native {
				persist = append(persist, snap.ChildID)
			}
			if live {
				c.sendSteer(snap.ChildID, executorRemovedNotice(name, c.displayNames(rest)))
			}
		case slices.Contains(ids, executorID):
			rest, held := c.dropMember(snap.ChildID, executorID, nil)
			if !held {
				continue
			}
			// A native subagent row has no durable record to update.
			if !snap.Native {
				persist = append(persist, snap.ChildID)
			}
			if live {
				c.sendSteer(snap.ChildID, executorRemovedNotice(name, c.displayNames(rest)))
			}
		}
	}
	c.persistExecutorSets(persist)
}

// seedAnnouncedExecutors marks every executor that already has a row as
// announced, so a daemon restart does not tell `all` agents about executors
// they have always had. A listing failure or timeout logs and leaves the set
// empty: the cost is one spurious announcement per executor after a degraded
// startup.
func (c *Controller) seedAnnouncedExecutors(ctx context.Context) {
	var rows []executors.Executor
	var err error
	if c.execStore != nil {
		lctx, cancel := context.WithTimeout(ctx, 2*executorSetLookupTimeout)
		rows, err = c.execStore.List(lctx)
		cancel()
	}
	c.execSets.mu.Lock()
	defer c.execSets.mu.Unlock()
	c.execSets.announced = map[string]bool{}
	if err != nil {
		slog.Warn("executor set: could not seed the announced executors; "+
			"each executor may be announced once after this start", "error", err)
		return
	}
	for _, e := range rows {
		c.execSets.announced[e.ID] = true
	}
}

// executorConnectSetter is the pool's on-connect registration seam.
type executorConnectSetter interface {
	SetOnConnect(fn func(executorID string))
}

// wireExecutorConnect seeds the announced executors and registers the on-connect
// hook. The seed comes first: a connection that arrives before it would announce
// an executor every `all` agent already has.
func (c *Controller) wireExecutorConnect(ctx context.Context, pool executorConnectSetter) {
	c.seedAnnouncedExecutors(ctx)
	pool.SetOnConnect(func(id string) { c.onExecutorConnect(ctx, id) })
}

// onExecutorConnect is everything that happens when an executor's connection is
// installed. It runs on its own goroutine, outside Pool.mu, once per
// connection, which is also what resumes the recovered children waiting for THIS
// executor (pending_resume.go): the sweep runs on every connect and leaves
// still-unsatisfied children pended.
func (c *Controller) onExecutorConnect(ctx context.Context, id string) {
	c.sweepPendingResumes()
	// A launch waits on the connection ARRIVING, never on a poll: the
	// installer's one-shot waiters for this executor fire here.
	c.installConnected(id)
	c.announceIfNew(id)
	if c.skillPusher != nil {
		if err := c.skillPusher.pushIfEligible(ctx, id); err != nil {
			slog.Warn("skill sync on connect failed", "executor", id, "error", err)
		}
	}
	if c.pymodulePusher != nil {
		if _, err := c.pymodulePusher.pushIfEligible(ctx, id); err != nil {
			slog.Warn("pymodule sync on connect failed", "executor", id, "error", err)
		}
	}
}

// allSetRecipients are the live children of ownerUserID whose set is all.
func (c *Controller) allSetRecipients(ownerUserID string) []childstore.Snapshot {
	var out []childstore.Snapshot
	for _, snap := range c.setHoldersOf(ownerUserID) {
		if !noticeRecipient(snap) {
			continue
		}
		if all, _, _ := childstore.ResolveSetSpec(snap.ExecutorSet, snap.ExecutorSelector); all {
			out = append(out, snap)
		}
	}
	return out
}

// announceIfNew tells every live `all` agent of the executor's owner, whose
// labels the executor admits, about an executor the first time it connects.
// Sandboxes are never announced: only the child that owns one holds it, by id.
func (c *Controller) announceIfNew(executorID string) {
	unlock := c.execSets.lockExecutor(executorID)
	defer unlock()
	if !c.execSets.markAnnounced(executorID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), executorSetLookupTimeout)
	defer cancel()
	row, ok := c.executorRowByID(ctx, executorID)
	if !ok {
		// A row that cannot be read now may be announced by a later connect.
		c.execSets.forgetAnnounced(executorID)
		return
	}
	if isSandboxRow(row) {
		return
	}
	name := installNameOf(row)
	for _, snap := range c.allSetRecipients(row.OwnerUserID) {
		if executorAdmitsChild(row, snap.Labels, row.OwnerUserID) {
			c.sendSteer(snap.ChildID, executorAvailableNotice(name))
		}
	}
}

// noticeExecutorEnabled tells every live `all` agent of the executor's owner that
// the owner switched it off or on. No set changes: admission already excludes a
// disabled executor, and enabling restores it.
func (c *Controller) noticeExecutorEnabled(e executors.Executor, enabled bool) {
	if c.st == nil || isSandboxRow(e) {
		return
	}
	unlock := c.execSets.lockExecutor(e.ID)
	defer unlock()
	name := installNameOf(e)
	msg := executorDisabledNotice(name)
	if enabled {
		msg = executorEnabledNotice(name)
	}
	for _, snap := range c.allSetRecipients(e.OwnerUserID) {
		c.sendSteer(snap.ChildID, msg)
	}
}

// removeSandboxFromExecutorSets is a removed sandbox's effect on sets: the
// child that holds it by id loses it and is told. A sandbox row's executor row
// may already be gone, so the name comes from the sandbox row.
func (c *Controller) removeSandboxFromExecutorSets(row sandbox.Row) {
	if row.ExecutorID == "" {
		return
	}
	name := row.Name
	if name == "" {
		name = shortID(row.ExecutorID)
	}
	c.removeFromExecutorSets(row.ExecutorID, row.OwnerUserID, name,
		map[string]string{sandbox.RowLabelSandbox: "1"})
}

// forgetRolledBackExecutor drops what the announce path remembers about an
// executor row an install rolled back. When a racing connect already announced
// it, `all` agents were told it was available, so they are told it is gone too.
func (c *Controller) forgetRolledBackExecutor(executorID string, row executors.Executor, haveRow bool) {
	if haveRow && c.execSets.wasAnnounced(executorID) {
		c.removeFromExecutorSets(row.ID, row.OwnerUserID, installNameOf(row), row.Labels)
		return
	}
	c.execSets.forgetAnnounced(executorID)
}
