// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

const notifyOtherOwner = "u_other"

// steerLog records every executor notice the controller pushed, per child, the
// way the event buffer hands them to delivery.
type steerLog struct {
	mu   sync.Mutex
	byID map[string][]string
}

func (l *steerLog) of(childID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.byID[childID]...)
}

// attachSteerLog gives the fixture's controller an event buffer whose flush is
// recorded. PushSteer flushes immediately, so a notice is visible on return.
func attachSteerLog(f *installFixture) *steerLog {
	l := &steerLog{byID: map[string][]string{}}
	buf := eventbuf.New(eventbuf.Config{}, clock.NewFakeClock(time.Now()))
	buf.SetFlush(func(childID, source string, orphans []inbox.Inbound) {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, o := range orphans {
			l.byID[childID] = append(l.byID[childID], o.Text)
		}
	})
	buf.SetBusy(func(string) bool { return false })
	f.ctrl.evbuf = buf
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

func setOf(f *installFixture, id string) []string {
	s, _ := f.ctrl.st.Get(id)
	return s.ExecutorSet
}

// connectHook makes the fixture's tunnel stand in for the pool's on-connect
// callback as main.go installs it: installer waiters, then the announcement.
func connectHook(f *installFixture, id func() string) {
	f.state.serve = func(ctx context.Context, _ net.Conn) error {
		f.ctrl.installConnected(id())
		f.ctrl.announceIfNew(id())
		<-ctx.Done()
		return ctx.Err()
	}
}

func childInstallCaller(id string) installCaller {
	return installCaller{OwnerUserID: installTestOwner, OwnerName: installTestOwnerName, ChildID: id}
}

func TestInstallAddsToTheInstallersSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	connectHook(f, func() string { return "exec-1" })

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")

	c.EqDeep([]string{"exec-A", "exec-1"}, setOf(f, "c_kid"), "the installer's set")
	c.EqDeep([]string{executorAddedNotice("ublk-s2", []string{"alpha", "ublk-s2"})}, log.of("c_kid"), "exactly one notice")
}

// The row's author does not decide who may use it: a reinstall of a row the CLI
// made still joins the reinstaller's set.
func TestReinstallAddsToTheReinstallersSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	connectHook(f, func() string { return "exec-1" })
	f.launcher.probeResults = nil

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_kid"), installReq(installTestLauncherN))
	c.Require().NoError(err, "reinstall")

	c.Eq("", f.installs.liveRows()[0].InstalledByChild, "the recipe's author is unchanged")
	c.EqDeep([]string{"exec-A", "exec-1"}, setOf(f, "c_kid"), "the reinstaller's set")
	c.Len(log.of("c_kid"), 1, "notices")
}

func TestInstallByAnAllChildChangesNothingButAnnounces(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	connectHook(f, func() string { return "exec-1" })

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_all"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")

	c.EqDeep([]string{childstore.ExecutorSetAll}, setOf(f, "c_all"), "an all set stays all")
	c.EqDeep([]string{executorAvailableNotice("ublk-s2")}, log.of("c_all"), "one announcement, from the connect hook only")
}

func TestLegacyRowMaterializesOnInstall(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_old", installTestOwner, protocol.StatusIdle, nil)
	c.Require().NoError(f.ctrl.st.Update("c_old", func(s *childstore.Session) { s.ExecutorSelector = "role=w" }), "legacy selector")
	connectHook(f, func() string { return "exec-1" })
	// The legacy child reaches its launcher through the same selector.
	f.pool.live[0].Executor.Labels["role"] = "w"

	_, err := f.ctrl.InstallExecutor(context.Background(), childInstallCaller("c_old"), installReq(installTestLauncherN))
	c.Require().NoError(err, "install")

	c.EqDeep([]string{"exec-A", "exec-B", "exec-1"}, setOf(f, "c_old"), "the selector's members plus the install")
	c.Len(log.of("c_old"), 1, "notices")
}

// removalFixture is one owner's four children plus a foreign one, all holding
// the soon-removed executor exec-1 in the shapes a set can take.
func removalFixture(t *testing.T) (*installFixture, *steerLog) {
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.seedInstalledExecutor()
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_ap", installTestOwner, protocol.StatusIdle, []string{"exec-A", "exec-1"})
	seedSetChild(t, f, "c_p", installTestOwner, protocol.StatusStreaming, []string{"exec-1"})
	seedSetChild(t, f, "c_all", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_a", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_foreign", notifyOtherOwner, protocol.StatusIdle, []string{"exec-1"})
	seedSetChild(t, f, "c_gone", installTestOwner, protocol.StatusExited, []string{"exec-1"})
	return f, log
}

func assertRemovedEverywhere(t *testing.T, f *installFixture, log *steerLog) {
	c := assert.NewCollecting(t)
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_ap"), "c_ap")
	c.EqDeep([]string{}, setOf(f, "c_p"), "c_p becomes empty, not nil")
	c.EqDeep([]string{childstore.ExecutorSetAll}, setOf(f, "c_all"), "c_all")
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_a"), "c_a")
	c.EqDeep([]string{"exec-1"}, setOf(f, "c_foreign"), "another owner's child is untouched")
	c.EqDeep([]string{"exec-1"}, setOf(f, "c_gone"), "an exited child is untouched")
	c.Eq(true, setOf(f, "c_p") != nil, "c_p is non-nil")

	c.EqDeep([]string{executorRemovedNotice("ublk-s2", []string{"alpha"})}, log.of("c_ap"), "c_ap notice")
	c.EqDeep([]string{executorRemovedNotice("ublk-s2", nil)}, log.of("c_p"), "c_p notice")
	c.EqDeep([]string{executorRemovedNotice("ublk-s2", []string{"all"})}, log.of("c_all"), "c_all notice")
	c.Empty(log.of("c_a"), "a child that never held it is not told")
	c.Empty(log.of("c_foreign"), "another owner's child is not told")
	c.Empty(log.of("c_gone"), "an exited child is not told")
}

func TestUninstallRemovesFromEverySet(t *testing.T) {
	f, log := removalFixture(t)
	assert.NewAborting(t).Require().NoError(
		f.ctrl.UninstallExecutor(context.Background(), installUserCaller(), "ublk-s2"), "uninstall")
	assertRemovedEverywhere(t, f, log)
}

func TestDeleteRemovesFromEverySet(t *testing.T) {
	f, log := removalFixture(t)
	a := connectExecutorAdmin{c: f.ctrl}
	assert.NewAborting(t).Require().NoError(a.Delete(context.Background(), "exec-1"), "delete")
	assertRemovedEverywhere(t, f, log)
}

func TestDeleteRemovesALegacySelectorMember(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	seedSetExecutor(f, "exec-B", "beta", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_old", installTestOwner, protocol.StatusIdle, nil)
	c.Require().NoError(f.ctrl.st.Update("c_old", func(s *childstore.Session) { s.ExecutorSelector = "role=w" }), "legacy selector")

	c.Require().NoError(connectExecutorAdmin{c: f.ctrl}.Delete(context.Background(), "exec-B"), "delete")

	c.EqDeep([]string{"exec-A"}, setOf(f, "c_old"), "materialized minus the removed id")
	c.EqDeep([]string{executorRemovedNotice("beta", []string{"alpha"})}, log.of("c_old"), "notice")
}

// A notify path with no event buffer must not block the install or uninstall
// that triggered it, and install then uninstall then reinstall keeps the set
// and the notices coherent at every step.
func TestInstallUninstallReinstallTransitions(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_kid", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	cur := "exec-1"
	connectHook(f, func() string { return cur })
	caller := childInstallCaller("c_kid")

	_, err := f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
	c.Require().NoError(err, "install")
	c.EqDeep([]string{"exec-A", "exec-1"}, setOf(f, "c_kid"), "after install")

	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller, "ublk-s2"), "uninstall")
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_kid"), "after uninstall")

	cur = "exec-2"
	_, err = f.ctrl.InstallExecutor(context.Background(), caller, installReq(installTestLauncherN))
	c.Require().NoError(err, "reinstall")
	c.EqDeep([]string{"exec-A", "exec-2"}, setOf(f, "c_kid"), "after reinstall")

	c.EqDeep([]string{
		executorAddedNotice("ublk-s2", []string{"alpha", "ublk-s2"}),
		executorRemovedNotice("ublk-s2", []string{"alpha"}),
		executorAddedNotice("ublk-s2", []string{"alpha", "ublk-s2"}),
	}, log.of("c_kid"), "one notice per step, in order")

	// With no event buffer the same operations still succeed.
	f.ctrl.evbuf = nil
	c.Require().NoError(f.ctrl.UninstallExecutor(context.Background(), caller, "ublk-s2"), "uninstall without a buffer")
	c.EqDeep([]string{"exec-A"}, setOf(f, "c_kid"), "set still updates without a buffer")
}

func TestAnnounceNewSkipsSeededAndSandboxes(t *testing.T) {
	c := assert.NewAborting(t)
	f := newInstallFixture(t)
	log := attachSteerLog(f)
	seedSetExecutor(f, "exec-A", "alpha", installTestOwner)
	f.ctrl.seedAnnouncedExecutors(context.Background())
	seedSetChild(t, f, "c_all1", installTestOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_all2", installTestOwner, protocol.StatusStreaming, nil)
	seedSetChild(t, f, "c_exited", installTestOwner, protocol.StatusExited, []string{childstore.ExecutorSetAll})
	seedSetChild(t, f, "c_ids", installTestOwner, protocol.StatusIdle, []string{"exec-A"})
	seedSetChild(t, f, "c_foreign", notifyOtherOwner, protocol.StatusIdle, []string{childstore.ExecutorSetAll})

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

	f.execs.execs["exec-S"] = executors.Executor{
		ID: "exec-S", Labels: map[string]string{"machine": "sbx", "rafiki/sandbox": "1"},
		OwnerUserID: installTestOwner, Enabled: true,
	}
	f.ctrl.announceIfNew("exec-S")
	c.Len(log.of("c_all1"), 1, "a sandbox is never announced")
}
