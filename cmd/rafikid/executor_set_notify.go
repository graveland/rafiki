// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
)

// executorSetLookupTimeout (a variable only so a test can shorten it) bounds every executor-store read the set
// maintenance paths make. An install or uninstall has already succeeded when
// they run, so a stuck store must cost a skipped update, never a hung call.
var executorSetLookupTimeout = installLookupTimeout

// executorOrderTimeout (a variable so a test can shorten it) bounds how long a
// notice path waits for another operation on the same executor. A wait that
// runs out costs a skipped notice (or, for a removal, an unordered one), never a
// hung install, uninstall, enable or disable.
var executorOrderTimeout = 10 * time.Second

// executorAdminTimeout (a variable so a test can shorten it) bounds one whole
// administrative executor operation: reference resolution, the row write and the
// notices that follow.
var executorAdminTimeout = 30 * time.Second

// executorSetPersistTimeout bounds one targeted set write.
var executorSetPersistTimeout = 5 * time.Second

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
// a reconnect is not a new executor. ordering serialises everything that
// decides or announces one executor's state (announce, removal, enable, disable,
// rollback), so a "removed" can never overtake the "available" that preceded it.
// persistLocks serialises the stored-set write of one child, which makes the
// last write the one carrying the newest in-memory set. persists tracks the
// background writes.
type executorSetNotifier struct {
	mu           sync.Mutex
	announced    map[string]bool
	ordering     keyedMutex
	persistLocks keyedMutex
	persists     sync.WaitGroup
}

// keyedMutex is a mutex per key whose acquisition ends with its context. An
// entry exists only while someone holds or awaits it, so the map does not grow
// with every key ever seen.
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedEntry
}

type keyedEntry struct {
	token chan struct{}
	refs  int
}

// lock takes key's mutex or gives up when ctx ends. The returned unlock is
// always safe to call; ok reports whether the mutex is held.
func (k *keyedMutex) lock(ctx context.Context, key string) (unlock func(), ok bool) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = map[string]*keyedEntry{}
	}
	e := k.entries[key]
	if e == nil {
		e = &keyedEntry{token: make(chan struct{}, 1)}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	release := func() {
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
	select {
	case e.token <- struct{}{}:
		return func() {
			<-e.token
			release()
		}, true
	case <-ctx.Done():
		release()
		return func() {}, false
	}
}

// lockExecutor serialises operations on executorID's announced state and
// notices. When the wait ends first the returned unlock is a no-op and ok is
// false; the caller decides whether to proceed unordered or skip.
func (c *Controller) lockExecutor(executorID string) (unlock func(), ok bool) {
	return c.lockExecutorWithin(context.Background(), executorID)
}

// lockExecutorWithin is lockExecutor bounded by parent as well as by
// executorOrderTimeout.
func (c *Controller) lockExecutorWithin(parent context.Context, executorID string) (unlock func(), ok bool) {
	ctx, cancel := context.WithTimeout(parent, executorOrderTimeout)
	defer cancel()
	unlock, ok = c.execSets.ordering.lock(ctx, executorID)
	if !ok {
		slog.Warn("executor set: another operation on this executor did not finish in time",
			"executor", shortID(executorID), "timeout", executorOrderTimeout)
	}
	return unlock, ok
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

// persistExecutorSets writes the named children's stored sets in the
// background, one goroutine per call. The in-memory set is already updated, so a
// slow or failed write costs a stale stored set (healed by the next set write),
// never a hung or failed install, uninstall or on-connect hook.
//
// Only the executor_set column is written, never the row: a snapshot taken
// here and written after a Close or an exit would otherwise clear closed_at or
// overwrite a newer status. The set is read from memory under the child's own
// write lock, so of two racing updates the last write always carries the newest
// set.
func (c *Controller) persistExecutorSets(childIDs []string) {
	if len(childIDs) == 0 || c.setWriter == nil {
		return
	}
	c.execSets.persists.Add(1)
	go func() {
		defer c.execSets.persists.Done()
		for _, id := range childIDs {
			if err := c.persistExecutorSet(id); err != nil {
				slog.Warn("executor set: could not persist a child's set", "child", id, "error", err)
			}
		}
	}()
}

func (c *Controller) persistExecutorSet(childID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), executorSetPersistTimeout)
	defer cancel()
	unlock, ok := c.execSets.persistLocks.lock(ctx, childID)
	if !ok {
		return ctx.Err()
	}
	defer unlock()
	snap, ok := c.st.Get(childID)
	if !ok {
		return nil
	}
	return c.setWriter.SetExecutorSet(ctx, childID, snap.ExecutorSet)
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
// the set afterwards, also when the id was already gone, and whether the id was
// in it.
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
			// A concurrent removal already took it out (or converted a legacy
			// child): the stored set is the true remainder.
			rest = slices.Clone(cur)
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
//
// Set maintenance outranks notice order: when the ordering wait runs out the
// removal proceeds unordered rather than leave a removed id in a stored set.
func (c *Controller) removeFromExecutorSets(executorID, ownerUserID, name string, labels map[string]string) {
	unlock, _ := c.lockExecutor(executorID)
	defer unlock()
	c.removeFromExecutorSetsLocked(executorID, ownerUserID, name, labels)
}

// removeFromExecutorSetsLocked is removeFromExecutorSets for a caller already
// holding the executor's ordering lock.
func (c *Controller) removeFromExecutorSetsLocked(executorID, ownerUserID, name string, labels map[string]string) {
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
	unlock, ok := c.lockExecutor(executorID)
	if !ok {
		// Not marked announced, so the next connect tries again.
		return
	}
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

// noticeExecutorEnabledLocked tells every live `all` agent of the executor's
// owner that the owner switched it off or on. The caller holds the executor's
// ordering lock, which is what keeps two transitions from duplicating or
// reordering their notices. No set changes: admission already excludes a
// disabled executor, and enabling restores it.
func (c *Controller) noticeExecutorEnabledLocked(e executors.Executor, enabled bool) {
	if c.st == nil || isSandboxRow(e) {
		return
	}
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

// rollbackExecutorRow deletes the executor row of an install that did not reach
// a connection, and tells `all` agents it is gone if a racing connect already
// announced it. Reading the row, the announced check and the delete happen under
// the executor's ordering lock, the same one announce holds: either the announce
// ran first (and the removal follows it) or it runs after the delete, finds no
// row and announces nothing. ctx bounds the whole rollback.
func (c *Controller) rollbackExecutorRow(ctx context.Context, executorID string) {
	unlock, ok := c.execSets.ordering.lock(ctx, executorID)
	if !ok {
		slog.Warn("executor set: rollback did not get the executor's ordering lock in time; deleting the row unordered",
			"executor", shortID(executorID))
	}
	defer unlock()
	row, haveRow := c.executorRowByID(ctx, executorID)
	c.deleteExecutorRow(executorID)
	if haveRow && c.execSets.wasAnnounced(executorID) {
		c.removeFromExecutorSetsLocked(row.ID, row.OwnerUserID, installNameOf(row), row.Labels)
		return
	}
	c.execSets.forgetAnnounced(executorID)
}
