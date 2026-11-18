// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
)

const notifyOtherOwner = "u_other"

// steerLog records every executor notice the controller pushed, per child, the
// way the event buffer hands them to delivery. before, when set, runs ahead of
// each recorded notice and may block it.
type steerLog struct {
	mu     sync.Mutex
	byID   map[string][]string
	before func(childID, text string)
}

func (l *steerLog) of(childID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.byID[childID]...)
}

// waitForNotices blocks until childID has n notices or the deadline passes, and
// returns what it has. A notice pushed from the on-connect hook lands after the
// install that waited on the same hook returns, so a test reads it by waiting
// on the log, never by sleeping.
func (l *steerLog) waitForNotices(t *testing.T, childID string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := l.of(childID)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// attachSteerLog gives a controller an event buffer whose flush is recorded.
// PushSteer flushes immediately, so a notice is visible on return.
func attachSteerLog(ctrl *Controller) *steerLog {
	l := &steerLog{byID: map[string][]string{}}
	buf := eventbuf.New(eventbuf.Config{}, clock.NewFakeClock(time.Now()))
	buf.SetFlush(func(childID, _ string, orphans []inbox.Inbound) {
		for _, o := range orphans {
			if l.before != nil {
				l.before(childID, o.Text)
			}
			l.mu.Lock()
			l.byID[childID] = append(l.byID[childID], o.Text)
			l.mu.Unlock()
		}
	})
	buf.SetBusy(func(string) bool { return false })
	ctrl.evbuf = buf
	return l
}

// seedSetExecutor adds a plain enrolled executor row owned by owner.
func seedSetExecutor(f *installFixture, id, machine, owner string) {
	f.execs.execs[id] = executors.Executor{
		ID: id, Labels: map[string]string{"machine": machine, "role": "w"},
		OwnerUserID: owner, Enabled: true, Isolation: "none",
		WorkspaceMode: "pinned", Roots: []string{"~"},
	}
}

// seedSetChild inserts a live child with the given stored set through the
// store's own setter, so the set takes the path production writes use.
func seedSetChild(t *testing.T, f *installFixture, id, owner string, status protocol.Status, set []string) {
	t.Helper()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: id, Status: status, StartedAt: time.Now(), OwnerUserID: owner,
		Labels: map[string]string{"owner": installTestOwnerName, childstore.LabelRoot: id},
	})
	if set != nil {
		assert.NewAborting(t).Require().NoError(f.ctrl.st.SetExecutorSet(id, set), "seed set")
	}
}

// seedLegacyChild is a child written before sets existed: no set, a selector.
func seedLegacyChild(t *testing.T, f *installFixture, id, selector string) {
	t.Helper()
	seedSetChild(t, f, id, installTestOwner, protocol.StatusIdle, nil)
	assert.NewAborting(t).Require().NoError(
		f.ctrl.st.Update(id, func(s *childstore.Session) { s.ExecutorSelector = selector }), "legacy selector")
}

// seedNativeThread inserts a native subagent row the way EnsureThreadChild does:
// no owner of its own, a parent label, the parent's set cloned.
func seedNativeThread(t *testing.T, f *installFixture, id, parent string, set []string) {
	t.Helper()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(), Native: true,
		Labels: map[string]string{childstore.LabelParent: parent, labelNativeSubagent: "1"},
	})
	assert.NewAborting(t).Require().NoError(f.ctrl.st.SetExecutorSet(id, set), "seed thread set")
}

func setOf(f *installFixture, id string) []string {
	s, _ := f.ctrl.st.Get(id)
	return s.ExecutorSet
}

// connectHook makes the fixture's tunnel stand in for the pool: the session
// fires the daemon's real on-connect hook, the one main.go registers.
func connectHook(f *installFixture, id func() string) {
	f.state.serve = func(ctx context.Context, _ net.Conn) error {
		f.ctrl.onExecutorConnect(ctx, id())
		<-ctx.Done()
		return ctx.Err()
	}
}

func childInstallCaller(id string) installCaller {
	return installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName, ChildID: id}
}

// setRecordStore is a child store that keeps the executor set of each row it is
// asked to write. err fails every write; block holds every write until it is
// closed (or the write's context ends).
type setRecordStore struct {
	stubChildStore
	mu    sync.Mutex
	sets  map[string][]string
	calls int
	err   error
	block chan struct{}
}

func (s *setRecordStore) Upsert(ctx context.Context, rec childstore.ChildRecord) error {
	s.mu.Lock()
	block, err := s.block, s.err
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err != nil {
		return err
	}
	if s.sets == nil {
		s.sets = map[string][]string{}
	}
	s.sets[rec.ChildID] = slices.Clone(rec.ExecutorSet)
	return nil
}

func (s *setRecordStore) stored(childID string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.sets[childID]
	return set, ok
}

func (s *setRecordStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// listGatedStore is the fixture's executor store with a List that fails or
// blocks on demand and a Get that blocks on demand.
type listGatedStore struct {
	*installExecStore
	mu       sync.Mutex
	listErr  error
	listHold chan struct{}
	getHold  chan struct{}
}

func (s *listGatedStore) gates() (listHold, getHold chan struct{}, listErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listHold, s.getHold, s.listErr
}

func (s *listGatedStore) failList(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listErr = err
}

func (s *listGatedStore) List(ctx context.Context) ([]executors.Executor, error) {
	hold, _, listErr := s.gates()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if listErr != nil {
		return nil, listErr
	}
	return s.installExecStore.List(ctx)
}

func (s *listGatedStore) Get(ctx context.Context, id string) (executors.Executor, error) {
	_, hold, _ := s.gates()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return executors.Executor{}, ctx.Err()
		}
	}
	return s.installExecStore.Get(ctx, id)
}

func gateExecStore(f *installFixture) *listGatedStore {
	g := &listGatedStore{installExecStore: f.execs}
	f.ctrl.execStore = g
	return g
}

// shortLookups shrinks the store-read deadline so a test of a stuck store ends
// in milliseconds. Tests that call it do not run in parallel.
func shortLookups(t *testing.T) {
	t.Helper()
	prev := executorSetLookupTimeout
	executorSetLookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { executorSetLookupTimeout = prev })
}

// within fails the test unless fn returns before d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

func TestInstallAddsToTheInstallersSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	store := &setRecordStore{}
	f.ctrl.children = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")
	f.ctrl.execSets.persists.Wait()

	want := []string{installTestLauncher, "exec-A", "exec-1"}
	c.EqDeep(want, setOf(f, "c_kid"), "the installer's set")
	stored, ok := store.stored("c_kid")
	c.Require().True(ok, "the child's row was written")
	c.EqDeep(want, stored, "the persisted set")
	c.EqDeep([]string{executorAddedNotice("ublk-s2", []string{installTestLauncherN, "alpha", "ublk-s2"})}, log.of("c_kid"), "exactly one notice")
}

// The row's author does not decide who may use it: a reinstall of a row the CLI
// made still joins the reinstaller's set.
func TestReinstallAddsToTheReinstallersSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })
	f.launcher.probeResults = nil

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "reinstall")

	c.Eq("", f.installs.liveRows()[0].InstalledByChild, "the recipe's author is unchanged")
	c.EqDeep([]string{installTestLauncher, "exec-A", "exec-1"}, setOf(f, "c_kid"), "the reinstaller's set")
	c.Len(log.of("c_kid"), 1, "notices")
}

// A reinstall by a child that already holds the executor changes nothing, so it
// is told nothing and nothing is written.
func TestReinstallByAMemberChangesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	store := &setRecordStore{}
	f.ctrl.children = store
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-1"})
	connectHook(f, func() string { return "exec-1" })
	f.launcher.probeResults = nil

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "reinstall")
	f.ctrl.execSets.persists.Wait()

	c.EqDeep([]string{installTestLauncher, "exec-1"}, setOf(f, "c_kid"), "the set is unchanged")
	c.Empty(log.of("c_kid"), "no EXECUTOR ADDED for a member")
	c.Eq(0, store.writes(), "nothing is written")
}

func TestInstallByAnAllChildChangesNothingButAnnounces(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	connectHook(f, func() string { return "exec-1" })

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_all"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")

	c.EqDeep([]string{childstore.ExecutorSetAll}, setOf(f, "c_all"), "an all set stays all")
	// The announcement comes from the on-connect hook, after the install that
	// waited on it has returned.
	c.EqDeep([]string{executorAvailableNotice("ublk-s2")}, log.waitForNotices(t, "c_all", 1), "one announcement, from the connect hook only")
}

func TestLegacyRowMaterializesOnInstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_old", "role=w")
	connectHook(f, func() string { return "exec-1" })
	// The legacy child reaches its launcher through the same selector.
	f.pool.live[0].Executor.Labels["role"] = "w"

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_old"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")

	got := slices.Clone(setOf(f, "c_old"))
	slices.Sort(got)
	c.EqDeep([]string{"exec-1", "exec-A", "exec-B"}, got, "the selector's members plus the install")
	c.Len(log.of("c_old"), 1, "notices")
}

// A listing that fails after the install connected must not turn a legacy child
// into a child that addresses only the new executor.
func TestLegacyInstallWithUnlistableExecutorsLeavesTheSetUntouched(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	gate := gateExecStore(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_old", "role=w")
	f.pool.live[0].Executor.Labels["role"] = "w"
	f.state.serve = func(ctx context.Context, _ net.Conn) error {
		gate.failList(errors.New("store down"))
		f.ctrl.onExecutorConnect(ctx, "exec-1")
		<-ctx.Done()
		return ctx.Err()
	}

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_old"), installReq(installTestLauncherN))
	c.Require().NoError(err, "the install itself succeeds")

	c.Eq(true, setOf(f, "c_old") == nil, "the legacy set stays legacy (nil)")
	c.Empty(log.of("c_old"), "no notice names a set that was not stored")
}

func TestLegacyInstallWithUnparsableSelectorLeavesTheSetUntouched(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_bad", "role in (")

	f.ctrl.addToExecutorSet("c_bad", "exec-1")

	c.Eq(true, setOf(f, "c_bad") == nil, "the set is untouched")
	c.Empty(log.of("c_bad"), "no notice")
}

// removalFixture is one owner's children plus a foreign one, all holding the
// soon-removed executor exec-1 in the shapes a set can take.
func removalFixture(t *testing.T) (*installFixture, *steerLog, *setRecordStore) {
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	store := &setRecordStore{}
	f.ctrl.children = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_ap", installTestOwner, protocol.StatusIdle, []string{"exec-A", "exec-1"})
	seedSetChild(t, f, "c_p", installTestOwner, protocol.StatusStreaming, []string{"exec-1"})
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_a", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_foreign", notifyOtherOwner, protocol.StatusIdle, []string{"exec-1"})
	seedSetChild(t, f, "c_gone", installTestOwner, protocol.StatusExited, []string{"exec-A", "exec-1"})
	seedNativeThread(t, f, "c_ap~t1", "c_ap", []string{"exec-A", "exec-1"})
	seedNativeThread(t, f, "c_foreign~t1", "c_foreign", []string{"exec-1"})
	return f, log, store
}

func assertRemovedEverywhere(t *testing.T, f *installFixture, log *steerLog, store *setRecordStore) {
	f.ctrl.execSets.persists.Wait()
	c := assert.NewCollecting(t)
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_ap"), "c_ap")
	c.EqDeep([]string{}, setOf(f, "c_p"), "c_p becomes empty, not nil")
	c.EqDeep([]string{childstore.ExecutorSetAll}, setOf(f, "c_all"), "c_all")
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_a"), "c_a")
	c.EqDeep([]string{"exec-1"}, setOf(f, "c_foreign"), "another owner's child is untouched")
	c.Eq(true, setOf(f, "c_p") != nil, "c_p is non-nil")
	// An exited child is resumable and reads its stored set on resume.
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_gone"), "an exited child's stored set loses the id")
	// A native subagent row has no owner of its own: its parent's decides.
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_ap~t1"), "a native thread of an owned parent loses the id")
	c.EqDeep([]string{"exec-1"}, setOf(f, "c_foreign~t1"), "a native thread of another owner's parent is untouched")

	c.EqDeep([]string{executorRemovedNotice("ublk-s2", []string{"alpha"})}, log.of("c_ap"), "c_ap notice")
	c.EqDeep([]string{executorRemovedNotice("ublk-s2", nil)}, log.of("c_p"), "c_p notice")
	c.EqDeep([]string{executorRemovedNotice("ublk-s2", []string{"all"})}, log.of("c_all"), "c_all notice")
	c.Empty(log.of("c_a"), "a child that never held it is not told")
	c.Empty(log.of("c_foreign"), "another owner's child is not told")
	c.Empty(log.of("c_gone"), "an exited child is not told")
	c.Empty(log.of("c_ap~t1"), "a native thread is not a notice recipient; its parent is")

	stored, ok := store.stored("c_gone")
	c.Eq(true, ok, "the exited child's row was written")
	c.EqDeep([]string{"exec-A"}, stored, "the exited child's persisted set")
	_, wroteThread := store.stored("c_ap~t1")
	c.Eq(false, wroteThread, "a native row has no durable record, so none is written")
}

func TestUninstallRemovesFromEverySet(t *testing.T) {
	f, log, store := removalFixture(t)
	assert.NewAborting(t).Require().NoError(
		f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")
	assertRemovedEverywhere(t, f, log, store)
}

func TestDeleteRemovesFromEverySet(t *testing.T) {
	f, log, store := removalFixture(t)
	a := connectExecutorAdmin{c: f.ctrl}
	assert.NewAborting(t).Require().NoError(a.Delete(context.Background(), "exec-1"), "delete")
	assertRemovedEverywhere(t, f, log, store)
}

func TestDeleteRemovesALegacySelectorMember(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	// A sandbox matches the selector's labels but is reached by ownership, never
	// by selection, so it is never part of a materialized set.
	f.execs.execs["exec-S"] = executors.Executor{
		ID: "exec-S", Labels: map[string]string{"machine": "sbx", "role": "w", sandbox.RowLabelSandbox: "1"},
		OwnerUserID: installTestOwner, Enabled: true,
	}
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_old", "role=w")
	seedLegacyChild(t, f, "c_other", "role=gpu")

	c.Require().NoError(connectExecutorAdmin{c: f.ctrl}.Delete(context.Background(), "exec-B"), "delete")

	c.EqDeep([]string{"exec-A"}, setOf(f, "c_old"), "materialized minus the removed id, sandbox excluded")
	c.EqDeep([]string{executorRemovedNotice("beta", []string{"alpha"})}, log.of("c_old"), "notice")
	c.Eq(true, setOf(f, "c_other") == nil, "a legacy child whose selector never reached the row is untouched")
	c.Empty(log.of("c_other"), "and is not told")
}

// A removal that cannot compute a legacy child's remaining set leaves the set
// alone and never states that nothing remains.
func TestRemovalWithUnlistableExecutorsOmitsTheRemainingLine(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	gate := gateExecStore(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_old", "role=w")
	row := f.execs.execs["exec-B"]
	delete(f.execs.execs, "exec-B")
	gate.failList(errors.New("store down"))

	f.ctrl.removeFromExecutorSets(row.ID, row.OwnerUserID, "beta", row.Labels)

	c.Eq(true, setOf(f, "c_old") == nil, "the legacy set is untouched")
	got := log.of("c_old")
	c.Require().Len(got, 1, "one notice")
	c.EqDeep(executorRemovedUnknownRestNotice("beta"), got[0], "notice")
	c.Eq(false, strings.Contains(got[0], "Remaining: none"), "never claims nothing remains")
}

// A stale id left in a stored set addresses nothing: reach is computed from the
// rows that exist, so a removed id that a late write restored changes no call.
func TestStaleExecutorIDInAStoredSetAddressesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-gone", "exec-A"})

	all, ids, selector := f.ctrl.executorSetSpec("c_kid")
	rows, err := f.execs.List(context.Background())
	c.Require().NoError(err, "list")
	kept := filterBySet(rows, all, ids, selector)

	c.EqDeep([]string{"exec-A"}, idsOf(kept), "only the row that exists is addressable")
}

func TestInstallUninstallReinstallTransitions(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	cur := "exec-1"
	connectHook(f, func() string { return cur })
	caller := childInstallCaller("c_kid")
	base := []string{installTestLauncher, "exec-A"}
	names := []string{installTestLauncherN, "alpha", "ublk-s2"}

	_, err := f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
	c.Require().NoError(err, "install")
	c.EqDeep(append(slices.Clone(base), "exec-1"), setOf(f, "c_kid"), "after install")

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller, "ublk-s2"), "uninstall")
	c.EqDeep(base, setOf(f, "c_kid"), "after uninstall")

	cur = "exec-2"
	_, err = f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
	c.Require().NoError(err, "reinstall")
	c.EqDeep(append(slices.Clone(base), "exec-2"), setOf(f, "c_kid"), "after reinstall")

	c.EqDeep([]string{
		executorAddedNotice("ublk-s2", names),
		executorRemovedNotice("ublk-s2", []string{installTestLauncherN, "alpha"}),
		executorAddedNotice("ublk-s2", names),
	}, log.of("c_kid"), "one notice per step, in order")
}

// With no event buffer the install and uninstall still succeed and the set
// still updates.
func TestSetUpdatesWithoutAnEventBuffer(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })
	f.ctrl.evbuf = nil

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install without a buffer")
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), childInstallCaller("c_kid"), "ublk-s2"), "uninstall without a buffer")
	c.EqDeep([]string{installTestLauncher, "exec-A"}, setOf(f, "c_kid"), "set still updates without a buffer")
}

// A failed row write is logged and never fails the install or uninstall whose
// set it was persisting.
func TestPersistFailureNeverFailsTheInstallOrUninstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	store := &setRecordStore{err: errors.New("child store down")}
	f.ctrl.children = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })
	caller := childInstallCaller("c_kid")

	_, err := f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
	c.Require().NoError(err, "install with a failing store")
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller, "ublk-s2"), "uninstall with a failing store")
	f.ctrl.execSets.persists.Wait()

	c.Eq(true, store.writes() >= 2, "the writes were attempted")
	c.EqDeep([]string{installTestLauncher, "exec-A"}, setOf(f, "c_kid"), "the in-memory set is right regardless")
}

// A child store that never answers must not hold the install or uninstall.
func TestStuckChildStoreDoesNotHangTheInstallOrUninstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.ctrl.children = &setRecordStore{block: release}
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })
	caller := childInstallCaller("c_kid")

	within(t, 2*time.Second, "install with a stuck child store", func() {
		_, err := f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
		c.NoError(err, "install")
	})
	within(t, 2*time.Second, "uninstall with a stuck child store", func() {
		c.NoError(f.ctrl.UninstallExecutor(context.Background(), caller, "ublk-s2"), "uninstall")
	})
}

// An executor store that never answers a listing must not hold a removal, and
// one that never answers a read must not hold the on-connect hook.
func TestStuckExecutorStoreDoesNotHangRemovalOrTheConnectHook(t *testing.T) {
	shortLookups(t)
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	gate := gateExecStore(f)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_old", "role=w")
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})

	gate.mu.Lock()
	gate.listHold, gate.getHold = release, release
	gate.mu.Unlock()

	within(t, 2*time.Second, "removal with a stuck listing", func() {
		f.ctrl.removeFromExecutorSets("exec-B", installTestOwner, "beta", map[string]string{"role": "w"})
	})
	c.Eq(true, setOf(f, "c_old") == nil, "the legacy set is untouched")
	c.Len(log.of("c_old"), 1, "the legacy child is told without a Remaining line")

	within(t, 2*time.Second, "the on-connect hook with a stuck row read", func() {
		f.ctrl.onExecutorConnect(context.Background(), "exec-NEW")
	})
	c.Len(log.of("c_all"), 1, "an unreadable row announces nothing beyond the removal notice")
	c.Eq(false, f.ctrl.execSets.wasAnnounced("exec-NEW"), "and is not remembered, so a later connect retries")
}

func TestAnnounceNewSkipsSeededAndSandboxes(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all1", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_all2", installTestOwner, protocol.StatusStreaming, nil)
	seedSetChild(t, f, "c_exited", installTestOwner, protocol.StatusExited, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_ids", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_foreign", notifyOtherOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedNativeThread(t, f, "c_all1~t1", "c_all1", []string{childstore.ExecutorSetAll})

	f.ctrl.announceIfNew("exec-A")
	c.Empty(log.of("c_all1"), "a seeded executor reconnecting is not new")

	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	f.ctrl.announceIfNew("exec-B")
	f.ctrl.announceIfNew("exec-B")
	want := []string{executorAvailableNotice("beta")}
	c.EqDeep(want, log.of("c_all1"), "an all child is told once")
	c.EqDeep(want, log.of("c_all2"), "a legacy child with no selector is all")
	c.Empty(log.of("c_exited"), "an exited child is not told")
	c.Empty(log.of("c_ids"), "an ids child is not told")
	c.Empty(log.of("c_foreign"), "another owner's child is not told")
	c.Empty(log.of("c_all1~t1"), "a native thread is not told; its parent is")

	f.execs.execs["exec-S"] = executors.Executor{
		ID: "exec-S", Labels: map[string]string{"machine": "sbx", "rafiki/sandbox": "1"},
		OwnerUserID: installTestOwner, Enabled: true,
	}
	f.ctrl.announceIfNew("exec-S")
	c.Len(log.of("c_all1"), 1, "a sandbox is never announced")
}

// An executor that does not admit a child is not announced to it: the call-time
// `all` resolution would refuse it.
func TestAnnounceHonoursAdmitsAgainstTheChildsLabels(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_in", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_out", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	c.Require().NoError(f.ctrl.st.Update("c_out", func(s *childstore.Session) { s.Labels["owner"] = "someone-else" }), "relabel")
	f.execs.execs["exec-G"] = executors.Executor{
		ID: "exec-G", Labels: map[string]string{"machine": "gamma"}, Admits: "owner=" + installTestOwnerName,
		OwnerUserID: installTestOwner, Enabled: true,
	}

	f.ctrl.announceIfNew("exec-G")

	c.EqDeep([]string{executorAvailableNotice("gamma")}, log.of("c_in"), "the admitted child is told")
	c.Empty(log.of("c_out"), "a child the executor does not admit is not told")
}

// The wiring main.go installs: the seed runs before the hook is registered, and
// the registered hook is the one that announces a new executor and skips a
// seeded one.
func TestWireExecutorConnectSeedsThenAnnounces(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	pool := &connectSetterFake{ctrl: f.ctrl}

	f.ctrl.wireExecutorConnect(context.Background(), pool)

	c.Require().NotNil(pool.fn, "the hook is registered")
	c.Eq(true, pool.seededAtRegistration, "the seed ran before the hook was registered")
	pool.fn("exec-A")
	c.Empty(log.of("c_all"), "an executor that already had a row is not announced")
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	pool.fn("exec-B")
	c.EqDeep([]string{executorAvailableNotice("beta")}, log.of("c_all"), "a new executor is announced")
}

type connectSetterFake struct {
	ctrl                 *Controller
	fn                   func(string)
	seededAtRegistration bool
}

func (p *connectSetterFake) SetOnConnect(fn func(string)) {
	p.fn = fn
	p.seededAtRegistration = p.ctrl.execSets.wasAnnounced("exec-A")
}

func TestExecutorDisableAndEnableNoticeAllChildrenWithoutChangingSets(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_ids", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_exited", installTestOwner, protocol.StatusExited, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_foreign", notifyOtherOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})

	c.Require().NoError(f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"}), "disable")
	c.EqDeep([]string{executorDisabledNotice("alpha")}, log.of("c_all"), "an all child is told of the disable")
	c.Empty(log.of("c_ids"), "an ids child is not an all child")
	c.Empty(log.of("c_exited"), "an exited child is not told")
	c.Empty(log.of("c_foreign"), "another owner's child is not told")
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_ids"), "disable changes no set")

	// The fake store does not flip Enabled; the row the controller reads decides
	// whether the transition is a change.
	row := f.execs.execs["exec-A"]
	row.Enabled = false
	f.execs.execs["exec-A"] = row
	c.Require().NoError(f.ctrl.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "exec-A"}), "enable")
	c.EqDeep([]string{executorDisabledNotice("alpha"), executorEnabledNotice("alpha")}, log.of("c_all"), "and of the re-enable")
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_ids"), "enable changes no set")

	row.Enabled = true
	f.execs.execs["exec-A"] = row
	c.Require().NoError(f.ctrl.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "exec-A"}), "enable again")
	c.Len(log.of("c_all"), 2, "enabling an enabled executor is not a change")
}

// A spawn-block sandbox is a member of its child's set. Removing it takes it out
// of that set, tells that child, and tells nobody else.
func TestSandboxRemovalUpdatesTheHoldersSet(t *testing.T) {
	c := assert.NewAborting(t)
	env := newSandboxEnv(t)
	log := attachSteerLog(env.ctrl)
	owner := sandboxOwner()
	env.pool.live = []execpool.LiveExecutor{sandboxLauncher("launcher", "box", owner.UserID)}
	env.exec.execs["exec-created"] = executors.Executor{
		ID: "exec-created", Enabled: true, OwnerUserID: owner.UserID,
		Labels: map[string]string{"machine": "sbx", sandbox.RowLabelSandbox: "1"},
	}
	row := sandbox.Row{
		ID: "sbx-1", OwnerUserID: owner.UserID, Name: "", ExecutorID: "exec-created",
		LauncherExecutorID: "launcher", ContainerID: "ctr-1", State: sandboxStateReady, OwnerChild: "c_holder",
	}
	c.Require().NoError(env.store.Insert(context.Background(), row), "seed sandbox row")
	insert := func(id string, set []string) {
		env.ctrl.st.Insert(&childstore.Session{
			ChildID: id, Status: protocol.StatusIdle, StartedAt: time.Now(), OwnerUserID: owner.UserID,
			Labels: map[string]string{childstore.LabelRoot: id},
		})
		c.Require().NoError(env.ctrl.st.SetExecutorSet(id, set), "seed set")
	}
	insert("c_holder", []string{"exec-created"})
	insert("c_all", []string{childstore.ExecutorSetAll})

	c.Require().NoError(env.ctrl.removeSandboxRow(context.Background(), row), "remove the sandbox")

	held, _ := env.ctrl.st.Get("c_holder")
	c.EqDeep([]string{}, held.ExecutorSet, "the holder lost the sandbox")
	c.EqDeep([]string{executorRemovedNotice(shortID("exec-created"), nil)}, log.of("c_holder"), "the holder is told")
	c.Empty(log.of("c_all"), "an all child never held it and is not told")
}

// A connect that raced a failed install's rollback was announced to `all`
// agents; the rollback tells them it is gone and forgets it.
func TestRollbackAfterARacingConnectTellsAllAgents(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetExecutor(f, "exec-R", "racer", installTestOwner)

	f.ctrl.announceIfNew("exec-R")
	c.Require().True(f.ctrl.execSets.wasAnnounced("exec-R"), "announced")
	f.ctrl.rollbackInstall("", "exec-R")

	c.Eq(false, f.ctrl.execSets.wasAnnounced("exec-R"), "the announced entry is cleared")
	c.EqDeep([]string{
		executorAvailableNotice("racer"),
		executorRemovedNotice("racer", []string{childstore.ExecutorSetAll}),
	}, log.of("c_all"), "told available, then removed")

	seedSetExecutor(f, "exec-Q", "quiet", installTestOwner)
	f.ctrl.rollbackInstall("", "exec-Q")
	c.Len(log.of("c_all"), 2, "a rollback nobody was told about tells nobody")
}

// A removal must not overtake the announcement of the same executor: an agent
// told "removed" and then "available" would believe a gone executor is usable.
func TestRemovalNeverOvertakesTheAnnouncement(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetExecutor(f, "exec-R", "racer", installTestOwner)

	inFlush := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	log.before = func(_, text string) {
		if text == executorAvailableNotice("racer") {
			once.Do(func() { close(inFlush) })
			<-release
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.ctrl.announceIfNew("exec-R") }()
	<-inFlush
	go func() {
		defer wg.Done()
		f.ctrl.removeFromExecutorSets("exec-R", installTestOwner, "racer", map[string]string{"machine": "racer"})
	}()
	// Give the removal every chance to run ahead of the blocked announcement.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	c.EqDeep([]string{
		executorAvailableNotice("racer"),
		executorRemovedNotice("racer", []string{childstore.ExecutorSetAll}),
	}, log.of("c_all"), "available first, then removed")
}
