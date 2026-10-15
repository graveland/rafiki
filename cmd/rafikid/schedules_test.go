// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sched"
	"go.graveland.dev/rafiki/pkg/schedules"

	"github.com/multigres/testkit/assert"
)

// scheduleFixture is settleFixture plus a wired schedule layer: an in-memory
// store, a daemon zone, the fake clock as the controller's clock, a scheduler
// on that clock, and the gate already open (so non-catch-up registrations arm
// immediately). The event buffer flushes after a 5s debounce, so tests advance
// an extra 6s before reading cap.batches().
func scheduleFixture(t *testing.T) (*Controller, *clock.FakeClock, *capturedFlush, *schedules.Memory) {
	t.Helper()
	c, clk, cap := settleFixture(t)
	mem := schedules.NewMemory()
	c.schedules = mem
	c.daemonZone = "UTC"
	c.clk = clk
	c.sched = sched.New(clk, time.Second)
	c.schedGate.open = true
	return c, clk, cap, mem
}

// totalFragments counts every delivered fragment across every batch.
func totalFragments(cap *capturedFlush) int {
	n := 0
	for _, b := range cap.batches() {
		n += len(b.fragments)
	}
	return n
}

// fragmentsContaining returns every delivered fragment containing sub.
func fragmentsContaining(cap *capturedFlush, sub string) []string {
	var out []string
	for _, b := range cap.batches() {
		for _, f := range b.fragments {
			if strings.Contains(f, sub) {
				out = append(out, f)
			}
		}
	}
	return out
}

// schedulePending reports whether the scheduler holds a job for id.
func schedulePending(c *Controller, id string) bool {
	for _, e := range c.sched.Pending() {
		if e.Key == scheduleKey(id) {
			return true
		}
	}
	return false
}

// failAdvanceStore wraps Memory and injects a fixed number of Advance failures.
type failAdvanceStore struct {
	*schedules.Memory
	mu       sync.Mutex
	failures int
}

func (s *failAdvanceStore) Advance(ctx context.Context, id string, next time.Time, firedAt *time.Time) error {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return errors.New("injected advance failure")
	}
	s.mu.Unlock()
	return s.Memory.Advance(ctx, id, next, firedAt)
}

// --- tools binding ---

func TestScheduleFiresIntoInboxAtDue(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(context.Background(), "*/5 * * * *", "wake and work", true)
	ck.Require().NoError(err)
	ck.Eq("UTC", info.TZ, "info.TZ")

	clk.Advance(5*time.Minute + time.Second) // fire at the 00:05:00 due instant
	clk.Advance(6 * time.Second)             // flush the debounce

	batches := cap.batches()
	ck.Len(batches, 1, "want one delivery batch, got %+v", batches)
	ck.Eq("c_w1", batches[0].childID, "batch child")
	ck.Eq(scheduleEventSource, batches[0].source, "batch source")
	ck.Len(batches[0].fragments, 1, "want one fragment")
	ck.StrContains(batches[0].fragments[0], "[scheduled job", "fragment header")
	ck.StrContains(batches[0].fragments[0], "wake and work", "fragment prompt")

	jobs, err := mem.ListOpen(context.Background(), "c_w1")
	ck.Require().NoError(err)
	ck.Len(jobs, 1, "the recurring job stays open")
	ck.True(jobs[0].NextFireAt.After(clk.Now()), "the row must have advanced to a future fire")
}

func TestScheduleWakeupReplacesPending(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, _ := scheduleFixture(t)
	b := newScheduleBinding(c, "c_w1")
	ctx := context.Background()

	if _, err := b.ScheduleWakeup(ctx, 5*time.Minute, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ScheduleWakeup(ctx, 5*time.Minute, "second"); err != nil {
		t.Fatal(err)
	}

	clk.Advance(5*time.Minute + time.Second)
	clk.Advance(6 * time.Second)

	frags := fragmentsContaining(cap, "second")
	ck.Len(frags, 1, "only the replacement wakeup delivers; got %+v", cap.batches())
	ck.Len(fragmentsContaining(cap, "first"), 0, "the replaced wakeup must not deliver")
	ck.StrContains(frags[0], "wakeup", "fragment names the wakeup")
}

func TestCronListNeverReturnsAnotherChildsJob(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scheduleFixture(t)
	ctx := context.Background()

	b1 := newScheduleBinding(c, "c_w1")
	b2 := newScheduleBinding(c, "c_w2")
	if _, err := b1.CronCreate(ctx, "*/5 * * * *", "one's job", true); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.CronCreate(ctx, "*/5 * * * *", "two's job", true); err != nil {
		t.Fatal(err)
	}

	jobs, err := b1.CronList(ctx)
	ck.Require().NoError(err)
	ck.Len(jobs, 1, "c_w1 listing another child's jobs")
	ck.Eq("one's job", jobs[0].Prompt, "c_w1's own job")
}

func TestScheduleCapReachedIsAToolError(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, mem := scheduleFixture(t)
	ctx := context.Background()

	for i := 0; i < schedules.MaxOpenPerChild; i++ {
		_, err := mem.Create(ctx, schedules.Job{
			ID: fmt.Sprintf("sch_cap%d", i), ChildID: "c_w1", Kind: schedules.KindCron,
			Cron: "*/5 * * * *", TZ: "UTC", Recurring: true, NextFireAt: time.Unix(10, 0),
		})
		if err != nil {
			t.Fatalf("seed create %d: %v", i, err)
		}
	}

	b := newScheduleBinding(c, "c_w1")
	_, err := b.CronCreate(ctx, "*/5 * * * *", "over the cap", true)
	ck.Require().Error(err, "creating past the cap must be refused")
	ck.StrContains(err.Error(), "CronDelete", "the refusal must tell the model how to make room; got %v", err)
}

func TestScheduleMinGapRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, mem := scheduleFixture(t)

	b := newScheduleBinding(c, "c_w1")
	_, err := b.CronCreate(context.Background(), "*/2 * * * *", "too often", true)
	ck.Require().Error(err, "a sub-5-minute recurring schedule must be refused")
	ck.StrContains(err.Error(), "minimum", "the refusal names the minimum gap; got %v", err)

	jobs, _ := mem.ListOpen(context.Background(), "c_w1")
	ck.Len(jobs, 0, "a refused create writes no row")
}

func TestScheduleUnsetOwnerZoneFallsBackToDaemonZone(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scheduleFixture(t)
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.OwnerUserID = "u_owner" })
	// mem.SetOwnerTimezone is deliberately NOT called: the owner has no zone.

	b := newScheduleBinding(c, "c_w1")
	info, err := b.CronCreate(context.Background(), "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	ck.Eq("UTC", info.TZ, "an owner with no zone falls back to the daemon zone")
}

func TestScheduleOwnerZoneWins(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, mem := scheduleFixture(t)
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.OwnerUserID = "u_owner" })
	mem.SetOwnerTimezone("u_owner", "Europe/Paris")

	b := newScheduleBinding(c, "c_w1")
	info, err := b.CronCreate(context.Background(), "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	ck.Eq("Europe/Paris", info.TZ, "the owner's zone must win over the daemon's")
}

func TestScheduleNoZoneRefusesCronCreate(t *testing.T) {
	ck := assert.NewAborting(t)
	c, _, _, _ := scheduleFixture(t)
	c.daemonZone = "" // and no owner zone anywhere

	b := newScheduleBinding(c, "c_w1")
	_, err := b.CronCreate(context.Background(), "*/5 * * * *", "hello", true)
	ck.Require().Error(err, "no resolvable zone must refuse CronCreate")
	ck.True(errors.Is(err, schedules.ErrNoTimezone), "the refusal must wrap ErrNoTimezone; got %v", err)
}

// --- firing guards ---

func TestScheduleDeletedJobDoesNotFire(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	ck.Require().NoError(b.CronDelete(ctx, info.ID))
	// CronDelete cancels the timer, so drive the stale fire by hand — exactly
	// the state a fire callback races into when it outlives a delete.
	c.fireSchedule(info.ID)

	clk.Advance(6 * time.Second)

	ck.Len(cap.batches(), 0, "a deleted job must not deliver")
	ck.False(schedulePending(c, info.ID), "a deleted job must not be re-armed")
	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt != nil, "the deleted row stays tombstoned")
}

func TestScheduleClosedChildTombstonesAndDoesNotRearm(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID)) // drive the stale timer by hand
	mem.SetChild("c_w1", true, "")      // the child's durable row is closed

	clk.Advance(5 * time.Minute)
	c.fireSchedule(info.ID)

	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt != nil, "a closed child's job must be tombstoned")
	ck.False(schedulePending(c, info.ID), "a closed child's job must not be re-armed")
	ck.Len(cap.batches(), 0, "a closed child's fire must not deliver")
}

func TestScheduleForeignOwnedChildDoesNotDeliver(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.daemonID = "this-daemon"
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID))
	mem.SetChild("c_w1", false, "other-daemon")

	clk.Advance(5 * time.Minute)
	c.fireSchedule(info.ID)
	clk.Advance(6 * time.Second) // flush the debounce: a missing guard would deliver here

	ck.Len(cap.batches(), 0, "another daemon's child must not receive this fire")
	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt == nil, "a foreign child's row must be left untouched")
}

func TestScheduleExitedChildNeitherRearmsNorTombstones(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID))
	c.st.SetStatus("c_w1", protocol.StatusExited)

	clk.Advance(5 * time.Minute)
	c.fireSchedule(info.ID)

	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt == nil, "an exited child's job must not be tombstoned")
	ck.False(schedulePending(c, info.ID), "an exited child's job must not be re-armed")
	ck.Len(cap.batches(), 0, "an exited child's fire must not deliver")
}

func TestScheduleOverBudgetFireIsHeld(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID))
	c.breaches.mark("c_w1")

	clk.Advance(5 * time.Minute)
	c.fireSchedule(info.ID)

	ck.Len(cap.batches(), 0, "a held fire delivers nothing")
	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.Eq(1, row.Job.HeldCount, "the held fire is counted")
	ck.True(schedulePending(c, info.ID), "a held recurring job re-arms at its next fire")
}

func TestScheduleAncestorBreachHoldsDescendantFire(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID))
	c.breaches.mark("c_coord") // the ancestor, not the child

	clk.Advance(5 * time.Minute)
	c.fireSchedule(info.ID)

	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.Eq(1, row.Job.HeldCount, "an ancestor breach must hold the descendant's fire")
}

func TestScheduleHeldOneShotIsNotRecountedOnRearm(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "3 * * * *", "remind me", false)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID))
	c.breaches.mark("c_w1")

	clk.Advance(3 * time.Minute)
	c.fireSchedule(info.ID) // first hold
	c.fireSchedule(info.ID) // a re-armed held one-shot is the same fire

	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.Eq(1, row.Job.HeldCount, "a held one-shot must not be recounted when re-armed")
}

func TestScheduleAdvanceFailureRetriesWithoutRedelivering(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.schedules = &failAdvanceStore{Memory: mem, failures: 1}

	b := newScheduleBinding(c, "c_w1")
	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	_ = info

	clk.Advance(5*time.Minute + time.Second) // fire; the Advance fails
	clk.Advance(6 * time.Second)             // flush the first (and only) push
	ck.Eq(1, totalFragments(cap), "exactly one fragment for the due instant")

	clk.Advance(3*time.Minute + 6*time.Second) // the retry runs and the Advance succeeds
	ck.Eq(1, totalFragments(cap), "the retry must not redeliver the fire")

	jobs, err := mem.ListOpen(ctx, "c_w1")
	ck.Require().NoError(err)
	ck.Len(jobs, 1, "the job stays open")
	ck.True(jobs[0].NextFireAt.After(clk.Now()), "the row must have moved on after the retry")
}

func TestScheduleReplayWhilePendingCollapses(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.schedules = &failAdvanceStore{Memory: mem, failures: 1}

	b := newScheduleBinding(c, "c_w1")
	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)

	clk.Advance(5*time.Minute + time.Second) // fire; the Advance fails
	c.fireSchedule(info.ID)                  // a restart's catch-up replays the same due instant
	clk.Advance(6 * time.Second)             // flush

	ck.Eq(1, totalFragments(cap), "a replay of the same due instant must collapse to one fragment")
}

// --- release on a budget raise ---

func TestScheduleBudgetRaiseReleasesOneFramePerHeldJob(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()

	for i, prompt := range []string{"one", "two"} {
		id := fmt.Sprintf("sch_held%d", i)
		_, err := mem.Create(ctx, schedules.Job{
			ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
			TZ: "UTC", Prompt: prompt, Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
		})
		ck.Require().NoError(err)
		ck.Require().NoError(mem.RecordHeld(ctx, id, clk.Now(), 1, nil))
	}
	c.breaches.mark("c_w1")
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.MaxCost = 10 })
	ck.Require().NoError(c.applyBudgetChange("c_w1", 100))

	clk.Advance(6 * time.Second)

	held := fragmentsContaining(cap, "were held")
	ck.Len(held, 2, "one held fragment per job; got %+v", cap.batches())
	for _, f := range held {
		ck.StrContains(f, "held while the agent tree was over budget", "each fragment carries the held note")
	}
}

func TestScheduleReleaseSkipsJobsStillUnderABreachedAncestor(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()

	id := "sch_under"
	_, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "held", Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)
	ck.Require().NoError(mem.RecordHeld(ctx, id, clk.Now(), 1, nil))

	c.breaches.mark("c_coord") // the ancestor is still breached
	c.releaseHeldSchedules("c_coord")
	clk.Advance(6 * time.Second)
	ck.Len(fragmentsContaining(cap, "were held"), 0, "a job under a breached ancestor must not release")

	// Positive control: clearing the breach releases it.
	c.breaches.clear("c_coord")
	c.releaseHeldSchedules("c_coord")
	clk.Advance(6 * time.Second)
	ck.Len(fragmentsContaining(cap, "were held"), 1, "clearing the breach releases the held job")
}

func TestScheduleHeldOneShotDeliversOnResumeAfterRaiseWhileExited(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "3 * * * *", "remind me", false)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID))
	c.breaches.mark("c_w1")

	clk.Advance(3 * time.Minute)
	c.fireSchedule(info.ID) // held: the tree is over budget
	row, err := mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.Eq(1, row.Job.HeldCount, "test setup: the one-shot is held")

	// The child exits, then the budget is raised: the release skips an exited
	// child, so the held one-shot is not delivered here.
	c.st.SetStatus("c_w1", protocol.StatusExited)
	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.MaxCost = 10 })
	ck.Require().NoError(c.applyBudgetChange("c_w1", 100))
	clk.Advance(6 * time.Second)
	ck.Len(fragmentsContaining(cap, "were held"), 0, "release skips an exited child")

	// The child resumes: registration re-arms the held one-shot at its old due
	// instant, which is now overdue, and it delivers once with its held note.
	c.st.SetStatus("c_w1", protocol.StatusIdle)
	c.registerSchedules(ctx, "c_w1", false)
	clk.Advance(0) // fire the overdue one-shot
	clk.Advance(6 * time.Second)

	held := fragmentsContaining(cap, "were held")
	ck.Len(held, 1, "the held one-shot delivers once on resume; got %+v", cap.batches())
	ck.StrContains(held[0], "remind me", "the delivered prompt")
	row, err = mem.Get(ctx, info.ID)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt != nil, "the delivered one-shot is tombstoned")
}

// --- catch-up / manual resume ---

func TestCatchUpCollapsesMissedFiresIntoOneWake(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()

	// A daemon that was DOWN when the job was created: c.sched is nil, so the
	// create arms nothing.
	c.sched = nil
	b := newScheduleBinding(c, "c_w1")
	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	_ = info

	clk.Advance(time.Hour) // many occurrences go by while the daemon is down

	c.sched = sched.New(clk, time.Second)
	c.schedGate.open = false
	c.registerSchedules(ctx, "c_w1", true) // queued until the gate opens
	c.openScheduleGate(ctx)

	clk.Advance(6 * time.Second) // fire the overdue row
	clk.Advance(6 * time.Second) // flush

	frags := fragmentsContaining(cap, "were missed")
	ck.Len(frags, 1, "N missed occurrences must collapse into ONE wake; got %+v", cap.batches())
	ck.Eq(1, totalFragments(cap), "no per-occurrence delivery")

	jobs, err := mem.ListOpen(ctx, "c_w1")
	ck.Require().NoError(err)
	ck.Len(jobs, 1, "the recurring job stays open")
	ck.True(jobs[0].NextFireAt.After(clk.Now()), "the row advances to a future fire after catch-up")
}

func TestManualResumeSkipsMissedRecurringButFiresOverdueOneShot(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()

	c.sched = nil
	b := newScheduleBinding(c, "c_w1")
	rec, err := b.CronCreate(ctx, "*/5 * * * *", "recurring", true)
	ck.Require().NoError(err)
	_, err = b.CronCreate(ctx, "10 * * * *", "one-shot", false)
	ck.Require().NoError(err)

	clk.Advance(time.Hour)

	c.sched = sched.New(clk, time.Second)
	c.registerSchedules(ctx, "c_w1", false) // manual resume: skip to the next future occurrence

	clk.Advance(6 * time.Second) // fire the overdue one-shot
	clk.Advance(6 * time.Second) // flush

	frags := fragmentsContaining(cap, "one-shot")
	ck.Len(frags, 1, "only the overdue one-shot fires on a manual resume; got %+v", cap.batches())
	ck.Len(fragmentsContaining(cap, "recurring"), 0, "a missed recurring job must not fire on a manual resume")

	jobs, err := mem.ListOpen(ctx, "c_w1")
	ck.Require().NoError(err)
	var found bool
	for _, j := range jobs {
		if j.ID == rec.ID {
			found = true
			ck.True(j.NextFireAt.After(clk.Now()), "the recurring row is advanced to a future fire")
		}
	}
	ck.True(found, "the recurring job is still open")
}

func TestCatchUpRegistrationWaitsForScheduleGate(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, mem := scheduleFixture(t)
	ctx := context.Background()
	c.schedGate.open = false

	id := "sch_gate"
	_, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "hello", Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)

	c.registerSchedules(ctx, "c_w1", true)
	ck.False(schedulePending(c, id), "a catch-up registration must queue while the gate is closed")

	c.openScheduleGate(ctx)
	ck.True(schedulePending(c, id), "opening the gate must drain the queued catch-up registration")
}

// --- additional guard and release coverage ---

// failGetStore wraps Memory and injects a fixed number of Get failures.
type failGetStore struct {
	*schedules.Memory
	mu       sync.Mutex
	failures int
}

func (s *failGetStore) Get(ctx context.Context, id string) (schedules.FireRow, error) {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return schedules.FireRow{}, errors.New("injected get failure")
	}
	s.mu.Unlock()
	return s.Memory.Get(ctx, id)
}

// F2: openScheduleGate must SEED the budget set before it DRAINS the queued
// catch-up registrations. Deleting the seed leaves the breach unmarked but
// still arms the job, which is what this pins.
func TestOpenScheduleGateSeedsBeforeDraining(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, mem := scheduleFixture(t)
	ctx := context.Background()

	// A budgeted child already over its cap: the seed must mark it.
	c.coster = fakeCoster{spend: 99}
	_ = c.st.Update("c_coord", func(s *childstore.Session) { s.MaxCost = 10 })

	id := "sch_gate_seed"
	_, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "hello", Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)

	c.schedGate.open = false
	c.registerSchedules(ctx, "c_w1", true) // queued: the gate is closed
	ck.False(schedulePending(c, id), "a registration queued while the gate is closed must not arm")

	c.openScheduleGate(ctx)

	ck.True(c.breaches.has("c_coord"), "openScheduleGate must run the budget seed")
	ck.True(schedulePending(c, id), "openScheduleGate must drain the queued catch-up registration")
}

// F3(a)/(b): release clears a recurring job's hold, and tombstones a one-shot
// rather than leaving it open to redeliver on the next boot.
func TestScheduleReleaseClearsRecurringHoldAndTombstonesOneShot(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, mem := scheduleFixture(t)
	ctx := context.Background()

	recID, oneID := "sch_rec_clear", "sch_one_close"
	_, err := mem.Create(ctx, schedules.Job{
		ID: recID, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "recurring", Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)
	_, err = mem.Create(ctx, schedules.Job{
		ID: oneID, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "3 * * * *",
		TZ: "UTC", Prompt: "one-shot", Recurring: false, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)
	for _, id := range []string{recID, oneID} {
		ck.Require().NoError(mem.RecordHeld(ctx, id, clk.Now(), 1, nil))
	}

	c.releaseHeldSchedules("c_w1")
	clk.Advance(6 * time.Second)

	rec, err := mem.Get(ctx, recID)
	ck.Require().NoError(err)
	ck.Eq(0, rec.Job.HeldCount, "a released recurring job's hold must clear")
	ck.True(rec.Job.HeldFirstDue == nil && rec.Job.HeldLastDue == nil, "held instants must clear")
	ck.True(rec.Job.ClosedAt == nil, "a released recurring job stays open")

	one, err := mem.Get(ctx, oneID)
	ck.Require().NoError(err)
	ck.True(one.Job.ClosedAt != nil, "a released one-shot must be tombstoned or it redelivers on the next boot")
	ck.False(schedulePending(c, oneID), "release must not arm a released one-shot")
}

// F3(c): a held job whose child belongs to another daemon is skipped.
func TestScheduleReleaseSkipsForeignOwnedJob(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.daemonID = "this-daemon"

	id := "sch_foreign"
	_, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "foreign", Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)
	ck.Require().NoError(mem.RecordHeld(ctx, id, clk.Now(), 1, nil))
	mem.SetChild("c_w1", false, "other-daemon")

	c.releaseHeldSchedules("c_w1")
	clk.Advance(6 * time.Second)

	ck.Len(fragmentsContaining(cap, "were held"), 0, "a foreign-owned held job must not be released")
	row, err := mem.Get(ctx, id)
	ck.Require().NoError(err)
	ck.Eq(1, row.Job.HeldCount, "the foreign job's hold must be left intact")
}

// F5(a): a load (Get) failure arms a retry and delivers exactly once.
func TestScheduleLoadFailureRetries(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.schedules = &failGetStore{Memory: mem, failures: 1}

	b := newScheduleBinding(c, "c_w1")
	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)

	clk.Advance(5*time.Minute + time.Second) // fire: the load fails once
	ck.True(schedulePending(c, info.ID), "a load failure must arm a retry")
	ck.Eq(0, totalFragments(cap), "a failed load must deliver nothing")

	clk.Advance(time.Minute + 6*time.Second) // the retry loads, fires, and pushes
	clk.Advance(6 * time.Second)             // flush
	ck.Eq(1, totalFragments(cap), "the retry must deliver exactly once")
}

// F5(b): a timer that fires before the row's NextFireAt re-arms, delivering
// nothing.
func TestScheduleEarlyTimerRearmsAtRowDeadline(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	b := newScheduleBinding(c, "c_w1")

	info, err := b.CronCreate(ctx, "*/5 * * * *", "hello", true)
	ck.Require().NoError(err)
	c.cancelTimer(scheduleKey(info.ID)) // drive the timer by hand, while it is early

	later := clk.Now().Add(time.Hour)
	ck.Require().NoError(mem.Advance(ctx, info.ID, later, nil)) // the row moved on

	c.fireSchedule(info.ID) // now is before the row's NextFireAt
	clk.Advance(6 * time.Second)

	ck.Eq(0, totalFragments(cap), "an early timer must not deliver")
	var pending bool
	for _, e := range c.sched.Pending() {
		if e.Key == scheduleKey(info.ID) {
			pending = true
			ck.True(e.Deadline.Equal(later), "an early timer must re-arm at the row's NextFireAt; got %v want %v", e.Deadline, later)
		}
	}
	ck.True(pending, "an early timer must be re-armed")
}

// F5(c): an unparseable stored cron is tombstoned.
func TestScheduleUnparseableStoredCronIsTombstoned(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()

	id := "sch_bad"
	_, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "not a cron",
		TZ: "UTC", Prompt: "bad", Recurring: true, NextFireAt: clk.Now(),
	})
	ck.Require().NoError(err)

	c.fireSchedule(id)
	clk.Advance(6 * time.Second)

	ck.Len(cap.batches(), 0, "an unparseable schedule must not deliver")
	row, err := mem.Get(ctx, id)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt != nil, "an unparseable schedule must be tombstoned")
	ck.False(schedulePending(c, id), "an unparseable schedule must not be re-armed")
}

// F6: Close and CloseAllExited tombstone a closed child's open schedules.
func TestCloseAndCloseAllExitedTombstoneSchedules(t *testing.T) {
	ck := assert.NewAborting(t)
	ctx := context.Background()

	// Close.
	c, _, _, mem := scheduleFixture(t)
	c.children = stubChildStore{}
	_, err := mem.Create(ctx, schedules.Job{
		ID: "sch_close", ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "p", Recurring: true, NextFireAt: time.Unix(10, 0),
	})
	ck.Require().NoError(err)
	c.st.SetStatus("c_w1", protocol.StatusExited)
	ck.Require().NoError(c.Close("c_w1"))

	jobs, err := mem.ListOpen(ctx, "c_w1")
	ck.Require().NoError(err)
	ck.Len(jobs, 0, "Close must tombstone the child's open schedules")
	row, err := mem.Get(ctx, "sch_close")
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt != nil, "the closed child's row must be tombstoned")

	// CloseAllExited.
	c2, _, _, mem2 := scheduleFixture(t)
	c2.children = stubChildStore{}
	for _, id := range []string{"c_w1", "c_w2"} {
		_, err := mem2.Create(ctx, schedules.Job{
			ID: "sch_" + id, ChildID: id, Kind: schedules.KindCron, Cron: "*/5 * * * *",
			TZ: "UTC", Prompt: "p", Recurring: true, NextFireAt: time.Unix(10, 0),
		})
		ck.Require().NoError(err)
		c2.st.SetStatus(id, protocol.StatusExited)
	}
	_, err = c2.CloseAllExited(0)
	ck.Require().NoError(err)
	for _, id := range []string{"c_w1", "c_w2"} {
		jobs, err := mem2.ListOpen(ctx, id)
		ck.Require().NoError(err)
		ck.Len(jobs, 0, "CloseAllExited must tombstone %s's open schedules", id)
		row, err := mem2.Get(ctx, "sch_"+id)
		ck.Require().NoError(err)
		ck.True(row.Job.ClosedAt != nil, "%s's row must be tombstoned", id)
	}
}

// failClearHeldStore wraps Memory and injects a fixed number of ClearHeld
// failures — the store error F1 turns into a bookkeeping-only retry.
type failClearHeldStore struct {
	*schedules.Memory
	mu       sync.Mutex
	failures int
}

func (s *failClearHeldStore) ClearHeld(ctx context.Context, id string) error {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return errors.New("injected clear-held failure")
	}
	s.mu.Unlock()
	return s.Memory.ClearHeld(ctx, id)
}

// failTombstoneStore wraps Memory and injects a fixed number of Tombstone
// failures, for a released one-shot's tombstone retry.
type failTombstoneStore struct {
	*schedules.Memory
	mu       sync.Mutex
	failures int
}

func (s *failTombstoneStore) Tombstone(ctx context.Context, id string, firedAt *time.Time) error {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return errors.New("injected tombstone failure")
	}
	s.mu.Unlock()
	return s.Memory.Tombstone(ctx, id, firedAt)
}

// schedulePendingUnderKey reports whether the scheduler holds a job for key.
func schedulePendingUnderKey(c *Controller, key string) bool {
	for _, e := range c.sched.Pending() {
		if e.Key == key {
			return true
		}
	}
	return false
}

// F1(a): a transient ClearHeld failure on a released recurring job must arm a
// BOOKKEEPING-ONLY retry under a key distinct from the job's next-fire key. The
// retry must not replace the next-fire timer, must not re-push, and once it
// succeeds the held state clears with exactly one fragment delivered.
func TestScheduleReleaseClearHeldFailureRetriesWithoutRepushing(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.schedules = &failClearHeldStore{Memory: mem, failures: 1}

	id := "sch_clear_retry"
	job, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "*/5 * * * *",
		TZ: "UTC", Prompt: "recurring", Recurring: true, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)
	c.armSchedule(job) // the job's independently armed next-fire timer
	ck.Require().NoError(mem.RecordHeld(ctx, id, clk.Now(), 1, nil))

	c.releaseHeldSchedules("c_w1")
	clk.Advance(6 * time.Second) // flush the one release push

	ck.Len(fragmentsContaining(cap, "were held"), 1, "exactly one held fragment; got %+v", cap.batches())

	// The next-fire timer survives; the retry is armed under the distinct key.
	ck.True(schedulePending(c, id), "the clear-held retry must not replace the next-fire timer")
	ck.True(schedulePendingUnderKey(c, holdClearKey(id)), "the retry must be armed under the distinct holdClearKey")

	row, err := mem.Get(ctx, id)
	ck.Require().NoError(err)
	ck.Eq(1, row.Job.HeldCount, "the failed clear leaves the hold in place")

	clk.Advance(time.Minute + 6*time.Second) // the retry runs; ClearHeld succeeds
	row, err = mem.Get(ctx, id)
	ck.Require().NoError(err)
	ck.Eq(0, row.Job.HeldCount, "the retry must clear the hold")
	ck.True(row.Job.HeldFirstDue == nil && row.Job.HeldLastDue == nil, "the retry must clear the held instants")
	ck.True(row.Job.ClosedAt == nil, "a released recurring job stays open")
	ck.Len(fragmentsContaining(cap, "were held"), 1, "the retry must not push a second fragment")
}

// F1(b): a transient Tombstone failure on a released one-shot must reuse the
// ordinary tombstone retry: exactly one fragment, tombstoned after the retry.
func TestScheduleReleaseTombstoneFailureRetriesWithoutRepushing(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, mem := scheduleFixture(t)
	ctx := context.Background()
	c.schedules = &failTombstoneStore{Memory: mem, failures: 1}

	id := "sch_tomb_retry"
	_, err := mem.Create(ctx, schedules.Job{
		ID: id, ChildID: "c_w1", Kind: schedules.KindCron, Cron: "3 * * * *",
		TZ: "UTC", Prompt: "one-shot", Recurring: false, NextFireAt: clk.Now().Add(time.Hour),
	})
	ck.Require().NoError(err)
	ck.Require().NoError(mem.RecordHeld(ctx, id, clk.Now(), 1, nil))

	c.releaseHeldSchedules("c_w1")
	clk.Advance(6 * time.Second)

	ck.Len(fragmentsContaining(cap, "were held"), 1, "exactly one held fragment; got %+v", cap.batches())
	row, err := mem.Get(ctx, id)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt == nil, "the failed release tombstone leaves the row open")

	clk.Advance(time.Minute + 6*time.Second) // the retry tombstones without re-pushing
	row, err = mem.Get(ctx, id)
	ck.Require().NoError(err)
	ck.True(row.Job.ClosedAt != nil, "the retry must tombstone the released one-shot")
	ck.Len(fragmentsContaining(cap, "were held"), 1, "the retry must not push a second fragment")
}
