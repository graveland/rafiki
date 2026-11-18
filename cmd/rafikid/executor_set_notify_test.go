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

// resolveSetThroughSpawn is the set the production spawn resolver
// (resolveSpawnExecutorSet) fixes for a top-level child of owner that names refs.
// The named rows are made live for the resolution only, the way the pool lists
// them, so the resolver reads them exactly as it does at a real spawn.
func resolveSetThroughSpawn(t *testing.T, f *installFixture, owner string, refs []string) []string {
	t.Helper()
	saved := f.pool.live
	live := slices.Clone(saved)
	for _, ref := range refs {
		row, ok := f.execs.execs[ref]
		if !ok {
			continue
		}
		if !slices.ContainsFunc(live, func(le execpool.LiveExecutor) bool { return le.Executor.ID == ref }) {
			live = append(live, execpool.LiveExecutor{Executor: row})
		}
	}
	f.pool.live = live
	defer func() { f.pool.live = saved }()
	req, err := f.ctrl.resolveSpawnExecutorSet(
		protocol.SpawnRequest{Kind: protocol.KindFundi, Executors: refs},
		executorOwner{Name: installTestOwnerName, UserID: owner})
	assert.NewAborting(t).Require().NoError(err, "resolve the spawn's executor set")
	return req.Executors
}

// seedSetChild inserts a live child whose stored set is the one the production
// spawn resolver produces for refs: a nil refs is a legacy row (written before
// sets existed, which no builder can produce), an empty one addresses nothing,
// and a non-empty one is resolved. A child of another owner cannot resolve this
// owner's executors, which is the point of the foreign-owner rows, so its set is
// stored as given.
func seedSetChild(t *testing.T, f *installFixture, id, owner string, status protocol.Status, refs []string) {
	t.Helper()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: id, Status: status, StartedAt: time.Now(), OwnerUserID: owner,
		Labels: map[string]string{"owner": installTestOwnerName, childstore.LabelRoot: id},
	})
	set := refs
	if len(refs) > 0 && owner == installTestOwner {
		set = resolveSetThroughSpawn(t, f, owner, refs)
	}
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

// seedNativeThread creates a native subagent row through EnsureThreadChild, the
// production builder, which clones the parent's set. thread is the id the row
// takes ("<parent>~<thread>").
func seedNativeThread(t *testing.T, f *installFixture, parent, thread string) string {
	t.Helper()
	assert.NewAborting(t).Require().NoError(f.ctrl.EnsureThreadChild(parent, thread, ""), "EnsureThreadChild")
	return threadChildID(parent, thread)
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

// setRecordStore is the executor-set writer the controller persists through. It
// keeps the set of each row it is asked to write. err fails every write; block
// holds every write until it is closed (or the write's context ends).
type setRecordStore struct {
	mu    sync.Mutex
	sets  map[string][]string
	calls int
	err   error
	block chan struct{}
	// hook, when set, runs with the 1-based call number before the write
	// is recorded and may block it.
	hook func(call int, set []string)
}

func (s *setRecordStore) SetExecutorSet(ctx context.Context, childID string, set []string) error {
	s.mu.Lock()
	block, err, hook := s.block, s.err, s.hook
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if hook != nil {
		hook(call, set)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		return err
	}
	if s.sets == nil {
		s.sets = map[string][]string{}
	}
	s.sets[childID] = slices.Clone(set)
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

// listGatedStore is the fixture's executor store with gates a test can close:
// a List that fails or blocks, a Get that blocks (every call, or only the first
// one, which reports that it arrived), a SetEnabled/Delete/SetLabels that block
// until their context ends, and a Get rendezvous that waits for several callers
// to be inside at once. dataMu makes the underlying map safe for the concurrent
// tests, and SetEnabled really flips the row, unlike the plain fake.
type listGatedStore struct {
	*installExecStore
	dataMu sync.Mutex

	mu          sync.Mutex
	listErr     error
	listHold    chan struct{}
	getHold     chan struct{}
	writeStalls bool

	firstGetHold     chan struct{}
	firstGetEntered  chan struct{}
	firstListHold    chan struct{}
	firstListEntered chan struct{}

	rendezvous        int
	rendezvousTimeout time.Duration
	arrivals          int
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

// takeFirst returns the one-shot hold and its entered signal, clearing them.
func takeFirst(mu *sync.Mutex, hold, entered *chan struct{}) (h, e chan struct{}) {
	mu.Lock()
	defer mu.Unlock()
	h, e = *hold, *entered
	*hold, *entered = nil, nil
	return h, e
}

func (s *listGatedStore) List(ctx context.Context) ([]executors.Executor, error) {
	if h, e := takeFirst(&s.mu, &s.firstListHold, &s.firstListEntered); h != nil {
		close(e)
		select {
		case <-h:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	return s.installExecStore.List(ctx)
}

// enterGet is the rendezvous: Get calls are grouped in arrival order into
// groups of n, and a call returns once its whole group has arrived, or after the
// timeout (the serialised case, where the group can never assemble).
func (s *listGatedStore) enterGet() {
	s.mu.Lock()
	n, d := s.rendezvous, s.rendezvousTimeout
	if n == 0 {
		s.mu.Unlock()
		return
	}
	s.arrivals++
	target := (s.arrivals + n - 1) / n * n
	s.mu.Unlock()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		together := s.arrivals >= target
		s.mu.Unlock()
		if together {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *listGatedStore) Get(ctx context.Context, id string) (executors.Executor, error) {
	if h, e := takeFirst(&s.mu, &s.firstGetHold, &s.firstGetEntered); h != nil {
		s.dataMu.Lock()
		row, err := s.installExecStore.Get(ctx, id)
		s.dataMu.Unlock()
		close(e)
		select {
		case <-h:
		case <-ctx.Done():
			return executors.Executor{}, ctx.Err()
		}
		return row, err
	}
	_, hold, _ := s.gates()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return executors.Executor{}, ctx.Err()
		}
	}
	// The row is read before the rendezvous, so callers that meet here all hold
	// a value read before any of them writes.
	s.dataMu.Lock()
	row, err := s.installExecStore.Get(ctx, id)
	s.dataMu.Unlock()
	s.enterGet()
	return row, err
}

func (s *listGatedStore) stallWrites() {
	s.mu.Lock()
	s.writeStalls = true
	s.mu.Unlock()
}

// stalled blocks until ctx ends when writes are stalled, and reports whether it did.
func (s *listGatedStore) stalled(ctx context.Context) (bool, error) {
	s.mu.Lock()
	st := s.writeStalls
	s.mu.Unlock()
	if !st {
		return false, nil
	}
	<-ctx.Done()
	return true, ctx.Err()
}

func (s *listGatedStore) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if ok, err := s.stalled(ctx); ok {
		return err
	}
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	e, ok := s.execs[id]
	if !ok {
		return executors.ErrNotFound
	}
	e.Enabled = enabled
	s.execs[id] = e
	return nil
}

func (s *listGatedStore) Delete(ctx context.Context, id string) error {
	if ok, err := s.stalled(ctx); ok {
		return err
	}
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	return s.installExecStore.Delete(ctx, id)
}

func (s *listGatedStore) SetLabels(ctx context.Context, id string, set map[string]string, remove []string) (executors.Executor, error) {
	if ok, err := s.stalled(ctx); ok {
		return executors.Executor{}, err
	}
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	return s.installExecStore.SetLabels(ctx, id, set, remove)
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

// waitPersists waits for the background set writes, failing the test instead of
// hanging when one never finishes.
func waitPersists(t *testing.T, ctrl *Controller) {
	t.Helper()
	within(t, 5*time.Second, "the background set writes", ctrl.execSets.persists.Wait)
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
	f.ctrl.setWriter = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")
	waitPersists(t, f.ctrl)

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
	f.ctrl.setWriter = store
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-1"})
	connectHook(f, func() string { return "exec-1" })
	f.launcher.probeResults = nil

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "reinstall")
	waitPersists(t, f.ctrl)

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
	f.ctrl.setWriter = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_ap", installTestOwner, protocol.StatusIdle, []string{"exec-A", "exec-1"})
	seedSetChild(t, f, "c_p", installTestOwner, protocol.StatusStreaming, []string{"exec-1"})
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_a", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_foreign", notifyOtherOwner, protocol.StatusIdle, []string{"exec-1"})
	seedSetChild(t, f, "c_gone", installTestOwner, protocol.StatusExited, []string{"exec-A", "exec-1"})
	seedNativeThread(t, f, "c_ap", "t1")
	seedNativeThread(t, f, "c_foreign", "t1")
	return f, log, store
}

func assertRemovedEverywhere(t *testing.T, f *installFixture, log *steerLog, store *setRecordStore) {
	waitPersists(t, f.ctrl)
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
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_ap:t1"), "a native thread of an owned parent loses the id")
	c.EqDeep([]string{"exec-1"}, setOf(f, "c_foreign:t1"), "a native thread of another owner's parent is untouched")

	c.EqDeep([]string{executorRemovedNotice("ublk-s2", []string{"alpha"})}, log.of("c_ap"), "c_ap notice")
	c.EqDeep([]string{executorRemovedNotice("ublk-s2", nil)}, log.of("c_p"), "c_p notice")
	c.EqDeep([]string{executorRemovedNotice("ublk-s2", []string{"all"})}, log.of("c_all"), "c_all notice")
	c.Empty(log.of("c_a"), "a child that never held it is not told")
	c.Empty(log.of("c_foreign"), "another owner's child is not told")
	c.Empty(log.of("c_gone"), "an exited child is not told")
	c.Empty(log.of("c_ap:t1"), "a native thread is not a notice recipient; its parent is")

	stored, ok := store.stored("c_gone")
	c.Eq(true, ok, "the exited child's row was written")
	c.EqDeep([]string{"exec-A"}, stored, "the exited child's persisted set")
	_, wroteThread := store.stored("c_ap:t1")
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

	f.ctrl.removeFromExecutorSets(row.ID, row.OwnerUserID, "beta", row.Labels, "")

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
	seedSetExecutor(f, "exec-gone", "gone", installTestOwner)
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-gone", "exec-A"})
	// The row disappears without set maintenance, as a stale stored write can
	// leave it.
	delete(f.execs.execs, "exec-gone")

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
	f.ctrl.setWriter = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{installTestLauncher, "exec-A"})
	connectHook(f, func() string { return "exec-1" })
	caller := childInstallCaller("c_kid")

	_, err := f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
	c.Require().NoError(err, "install with a failing store")
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller, "ublk-s2"), "uninstall with a failing store")
	waitPersists(t, f.ctrl)

	c.Eq(true, store.writes() >= 2, "the writes were attempted")
	c.EqDeep([]string{installTestLauncher, "exec-A"}, setOf(f, "c_kid"), "the in-memory set is right regardless")
}

// A child store that never answers must not hold the install or uninstall.
func TestStuckChildStoreDoesNotHangTheInstallOrUninstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.ctrl.setWriter = &setRecordStore{block: release}
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
		f.ctrl.removeFromExecutorSets("exec-B", installTestOwner, "beta", map[string]string{"role": "w"}, "")
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
	seedNativeThread(t, f, "c_all1", "t1")

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
	c.Empty(log.of("c_all1:t1"), "a native thread is not told; its parent is")

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
func TestRollbackAfterAConnectTellsAllAgents(t *testing.T) {
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

// The interleaving where a connect announces while the rollback is in flight:
// the announcement has marked the executor and is reading its row when the
// rollback starts. The rollback must wait for it, so the agents are told
// "available" and then "removed", never the reverse, and never "available"
// with no "removed" after the row is gone.
func TestRollbackDuringAnAnnouncementWaitsForIt(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	gate := gateExecStore(f)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetExecutor(f, "exec-R", "racer", installTestOwner)

	hold, entered := make(chan struct{}), make(chan struct{})
	gate.mu.Lock()
	gate.firstGetHold, gate.firstGetEntered = hold, entered
	gate.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { defer func() { done <- struct{}{} }(); f.ctrl.announceIfNew("exec-R") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the announcement never read the row")
	}
	go func() { defer func() { done <- struct{}{} }(); f.ctrl.rollbackInstall("", "exec-R") }()
	eventually(t, "the rollback to queue behind the announcement", func() bool { return waitersOn(f.ctrl, "exec-R") == 2 })
	close(hold)
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the announcement and rollback did not both finish")
		}
	}

	c.EqDeep([]string{
		executorAvailableNotice("racer"),
		executorRemovedNotice("racer", []string{childstore.ExecutorSetAll}),
	}, log.of("c_all"), "available, then removed")
	c.Eq(false, f.ctrl.execSets.wasAnnounced("exec-R"), "nothing is left announced")
	_, still := f.execs.execs["exec-R"]
	c.Eq(false, still, "the row is gone")
}

// waitersOn is how many operations hold or await executorID's ordering lock.
func waitersOn(ctrl *Controller, executorID string) int {
	k := &ctrl.execSets.ordering
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.entries[executorID]; e != nil {
		return e.refs
	}
	return 0
}

// eventually polls cond until it holds, failing the test at the deadline.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A removal must not overtake the announcement of the same executor: an agent
// told "removed" and then "available" would believe a gone executor is usable.
// The announcement is held inside its delivery, the removal is observed queued
// on the ordering lock, and only then is the announcement released.
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

	done := make(chan struct{}, 2)
	go func() { defer func() { done <- struct{}{} }(); f.ctrl.announceIfNew("exec-R") }()
	select {
	case <-inFlush:
	case <-time.After(5 * time.Second):
		t.Fatal("the announcement never reached delivery")
	}
	go func() {
		defer func() { done <- struct{}{} }()
		f.ctrl.removeFromExecutorSets("exec-R", installTestOwner, "racer", map[string]string{"machine": "racer"}, "")
	}()
	// The removal is now contending for the executor's lock: holder plus waiter.
	eventually(t, "the removal to queue behind the announcement", func() bool { return waitersOn(f.ctrl, "exec-R") == 2 })
	close(release)
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the announcement and removal did not both finish")
		}
	}

	c.EqDeep([]string{
		executorAvailableNotice("racer"),
		executorRemovedNotice("racer", []string{childstore.ExecutorSetAll}),
	}, log.of("c_all"), "available first, then removed")
}

// rowUpsertForbidden is a child store that fails the test on any full-row
// write: a set update must touch the executor_set column only.
type rowUpsertForbidden struct {
	stubChildStore
	t *testing.T
}

func (s rowUpsertForbidden) Upsert(_ context.Context, rec childstore.ChildRecord) error {
	s.t.Errorf("a full-row upsert of %s: a set update must write the executor_set column only", rec.ChildID)
	return nil
}

// A set update never writes the row. A full-row write from a stale snapshot,
// landing after a Close or an exit, clears closed_at or overwrites a newer
// status and resurrects the child.
func TestSetUpdateNeverUpsertsTheRow(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	attachSteerLog(f.ctrl)
	f.ctrl.children = rowUpsertForbidden{t: t}
	store := &setRecordStore{}
	f.ctrl.setWriter = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-A", "exec-1"})
	seedSetChild(t, f, "c_gone", installTestOwner, protocol.StatusExited, []string{"exec-A", "exec-1"})

	f.ctrl.removeFromExecutorSets("exec-1", installTestOwner, "ublk-s2", map[string]string{"machine": "ublk-s2"}, "")
	f.ctrl.addToExecutorSet("c_kid", "exec-B")
	waitPersists(t, f.ctrl)

	for _, id := range []string{"c_kid", "c_gone"} {
		got, ok := store.stored(id)
		c.Require().True(ok, "%s's set was written", id)
		snap, _ := f.ctrl.st.Get(id)
		c.EqDeep(snap.ExecutorSet, got, "%s: the stored set is the in-memory one", id)
	}
}

// A closed child is gone from memory, so a write queued for it finds nothing to
// write; the store's own closed_at guard covers a write already in flight.
func TestSetUpdateOfAChildClosedMeanwhileWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	store := &setRecordStore{}
	f.ctrl.setWriter = store
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusExited, []string{"exec-A"})
	f.ctrl.st.Delete("c_kid")

	f.ctrl.persistExecutorSets([]string{"c_kid"})
	waitPersists(t, f.ctrl)

	c.Eq(0, store.writes(), "nothing is written for a child that is no longer open")
}

// Two removals from one child's set, whose writes reach the store out of order:
// the first write is held while the second removal updates memory and queues its
// own write. The stored set must end as the newest in-memory set, not as the
// stale snapshot the older write carries.
func TestConcurrentRemovalsPersistTheNewestSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	seedSetExecutor(f, "exec-C", "gamma", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-A", "exec-B", "exec-C"})

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	store := &setRecordStore{hook: func(call int, _ []string) {
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
		}
	}}
	f.ctrl.setWriter = store

	f.ctrl.removeFromExecutorSets("exec-A", installTestOwner, "alpha", map[string]string{"machine": "alpha"}, "")
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first write never reached the store")
	}
	f.ctrl.removeFromExecutorSets("exec-B", installTestOwner, "beta", map[string]string{"machine": "beta"}, "")
	// The second write is queued behind the first on the child's write lock.
	eventually(t, "the second write to queue", func() bool {
		k := &f.ctrl.execSets.persistLocks
		k.mu.Lock()
		defer k.mu.Unlock()
		e := k.entries["c_kid"]
		return e != nil && e.refs == 2
	})
	close(releaseFirst)
	waitPersists(t, f.ctrl)

	c.EqDeep([]string{"exec-C"}, setOf(f, "c_kid"), "memory lost both")
	stored, ok := store.stored("c_kid")
	c.Require().True(ok, "written")
	c.EqDeep([]string{"exec-C"}, stored, "the last write is the newest set")
}

// Concurrent installs and removals by many children never lose a member.
func TestConcurrentRemovalsOfDifferentExecutorsLoseNoMember(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	attachSteerLog(f.ctrl)
	store := &setRecordStore{}
	f.ctrl.setWriter = store
	ids := []string{"exec-A", "exec-B", "exec-C", "exec-D", "exec-E", "exec-F"}
	for _, id := range ids {
		seedSetExecutor(f, id, "m-"+id, installTestOwner)
	}
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, ids)

	var wg sync.WaitGroup
	for _, id := range ids[:5] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.ctrl.removeFromExecutorSets(id, installTestOwner, "m-"+id, map[string]string{"machine": "m-" + id}, "")
		}()
	}
	within(t, 5*time.Second, "the removals", wg.Wait)
	waitPersists(t, f.ctrl)

	c.EqDeep([]string{"exec-F"}, setOf(f, "c_kid"), "only the one never removed remains")
	stored, _ := store.stored("c_kid")
	c.EqDeep([]string{"exec-F"}, stored, "and the stored set agrees")
}

// Two legacy children's removals race: X's materialization lists the owner's
// executors while Y's removal already converted the row to a finite set. X's
// notice must name the true remainder, not claim nothing remains.
func TestConcurrentLegacyRemovalsReportTheTrueRemainder(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	gate := gateExecStore(f)
	seedSetExecutor(f, "exec-Z", "zeta", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedLegacyChild(t, f, "c_old", "role=w")
	labels := map[string]string{"role": "w"}

	hold, entered := make(chan struct{}), make(chan struct{})
	gate.mu.Lock()
	gate.firstListHold, gate.firstListEntered = hold, entered
	gate.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		f.ctrl.removeFromExecutorSets("exec-X", installTestOwner, "xray", labels, "")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first removal never listed the executors")
	}
	f.ctrl.removeFromExecutorSets("exec-Y", installTestOwner, "yankee", labels, "")
	close(hold)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first removal did not finish")
	}

	got := log.waitForNotices(t, "c_old", 2)
	c.Len(got, 2, "one notice per removal")
	for _, n := range got {
		c.True(!strings.Contains(n, "Remaining: none"), "no notice may claim nothing remains: %q", n)
		c.True(strings.Contains(n, "Remaining: zeta"), "each names the true remainder: %q", n)
	}
	c.EqDeep([]string{"exec-Z"}, setOf(f, "c_old"), "the stored set")
}

// Concurrent disables of one executor notify once, and the last notice always
// agrees with the executor's final state.
func TestConcurrentDisablesNoticeOnce(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	gate := gateExecStore(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	// Callers are held after reading the row until all four have read it; a
	// serialised implementation lets only one in at a time, so the group never
	// assembles and the rendezvous times out.
	gate.mu.Lock()
	gate.rendezvous, gate.rendezvousTimeout = 4, 100*time.Millisecond
	gate.mu.Unlock()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.NoError(f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"}), "disable")
		}()
	}
	within(t, 10*time.Second, "the disables", wg.Wait)

	c.EqDeep([]string{executorDisabledNotice("alpha")}, log.of("c_all"), "one DISABLED notice")
}

// An enable and a disable racing leave the last notice in step with the final
// state: a delayed disable must not announce DISABLED after an enable won.
func TestRacingEnableAndDisableEndWithTheFinalState(t *testing.T) {
	for range 20 {
		f := newInstallFixture(t)
		log := attachSteerLog(f.ctrl)
		gate := gateExecStore(f)
		seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
		f.ctrl.seedAnnouncedExecutors(context.Background())
		seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"})
		}()
		go func() {
			defer wg.Done()
			_ = f.ctrl.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "exec-A"})
		}()
		within(t, 10*time.Second, "enable and disable", wg.Wait)

		gate.dataMu.Lock()
		final := f.execs.execs["exec-A"].Enabled
		gate.dataMu.Unlock()
		notices := log.of("c_all")
		if len(notices) == 0 {
			continue
		}
		want := executorDisabledNotice("alpha")
		if final {
			want = executorEnabledNotice("alpha")
		}
		if notices[len(notices)-1] != want {
			t.Fatalf("the last notice %q disagrees with the final state (enabled=%v): %q", notices[len(notices)-1], final, notices)
		}
	}
}

// Every administrative executor call ends with its deadline when the store
// never answers, including reference resolution and the row write.
func TestAdminExecutorCallsAreBoundedByTheirDeadline(t *testing.T) {
	prev := executorAdminTimeout
	executorAdminTimeout = 80 * time.Millisecond
	t.Cleanup(func() { executorAdminTimeout = prev })
	f := newInstallFixture(t)
	gate := gateExecStore(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	gate.stallWrites()

	within(t, 3*time.Second, "disable with a stuck write", func() {
		assert.NewAborting(t).Error(f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"}), "disable")
	})
	within(t, 3*time.Second, "enable with a stuck write", func() {
		assert.NewAborting(t).Error(f.ctrl.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "exec-A"}), "enable")
	})
	within(t, 3*time.Second, "delete with a stuck write", func() {
		assert.NewAborting(t).Error(f.ctrl.ExecutorDelete(protocol.ExecutorDeleteRequest{ExecutorID: "exec-A"}), "delete")
	})
	within(t, 3*time.Second, "label with a stuck write", func() {
		_, err := f.ctrl.ExecutorLabel(protocol.ExecutorLabelRequest{ExecutorID: "exec-A", Set: map[string]string{"k": "v"}})
		assert.NewAborting(t).Error(err, "label")
	})

	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	gate.mu.Lock()
	gate.getHold = hold
	gate.mu.Unlock()
	within(t, 3*time.Second, "reference resolution with a stuck read", func() {
		assert.NewAborting(t).Error(f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"}), "disable")
	})
}

// A wait on another operation's ordering lock ends at its deadline, and each
// path degrades the way its contract says: a removal still maintains the sets
// (unordered), an announcement is skipped and retried on the next connect, and
// an enable or disable is refused as unavailable.
func TestOrderingWaitsAreBounded(t *testing.T) {
	prev := executorOrderTimeout
	executorOrderTimeout = 60 * time.Millisecond
	t.Cleanup(func() { executorOrderTimeout = prev })
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})

	hold, ok := f.ctrl.execSets.ordering.lock(context.Background(), "exec-A")
	c.Require().True(ok, "hold the executor's ordering lock")
	t.Cleanup(hold)

	within(t, 3*time.Second, "an announcement behind a held lock", func() {
		f.ctrl.announceIfNew("exec-A")
	})
	c.Eq(false, f.ctrl.execSets.wasAnnounced("exec-A") && len(log.of("c_all")) > 0, "no announcement was delivered")

	within(t, 3*time.Second, "a disable behind a held lock", func() {
		err := f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"})
		c.Error(err, "refused")
	})

	within(t, 3*time.Second, "a removal behind a held lock", func() {
		f.ctrl.removeFromExecutorSets("exec-A", installTestOwner, "alpha", map[string]string{"machine": "alpha", "role": "w"}, "")
	})
	c.EqDeep([]string{}, setOf(f, "c_kid"), "the removal still took the id out of the set")
}

// Disable and enable notices honour Admits on the child's labels exactly as the
// announce does: a child the executor never admitted is not told it was
// switched off or on.
func TestDisableAndEnableNoticesHonourAdmitsAgainstTheChildsLabels(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	row := f.execs.execs["exec-A"]
	row.Admits = "owner=" + installTestOwnerName
	f.execs.execs["exec-A"] = row
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_in", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_out", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	c.Require().NoError(f.ctrl.st.Update("c_out", func(s *childstore.Session) { s.Labels["owner"] = "someone-else" }), "relabel")

	c.Require().NoError(f.ctrl.ExecutorDisable(protocol.ExecutorDisableRequest{ExecutorID: "exec-A"}), "disable")
	row = f.execs.execs["exec-A"]
	row.Enabled = false
	f.execs.execs["exec-A"] = row
	c.Require().NoError(f.ctrl.ExecutorEnable(protocol.ExecutorEnableRequest{ExecutorID: "exec-A"}), "enable")

	c.EqDeep([]string{executorDisabledNotice("alpha"), executorEnabledNotice("alpha")}, log.of("c_in"), "the admitted child hears both")
	c.Empty(log.of("c_out"), "a child the executor does not admit hears neither")
}

func TestRemovalNoticeHonoursAdmitsAgainstTheChildsLabels(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f.ctrl)
	seedSetChild(t, f, "c_in", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_out", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	c.Require().NoError(f.ctrl.st.Update("c_in", func(s *childstore.Session) { s.Labels["owner"] = installTestOwnerName }), "label")
	c.Require().NoError(f.ctrl.st.Update("c_out", func(s *childstore.Session) { s.Labels["owner"] = "someone-else" }), "relabel")

	f.ctrl.removeFromExecutorSets("exec-Z", installTestOwner, "zulu", map[string]string{"machine": "zulu"}, "owner="+installTestOwnerName)

	c.EqDeep([]string{executorRemovedNotice("zulu", []string{childstore.ExecutorSetAll})}, log.of("c_in"), "the admitted child is told")
	c.Empty(log.of("c_out"), "a child the removed executor never admitted is not told")
}
