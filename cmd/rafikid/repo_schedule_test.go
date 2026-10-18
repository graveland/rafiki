// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fakeAfter is time.AfterFunc with the firing left to the test: every call is
// recorded with its delay, and fireAt calls the function the scheduler handed
// over. The returned timers never fire on their own (an hour out), so the
// scheduler only ever advances when the test says so.
type fakeAfter struct {
	mu    sync.Mutex
	calls []afterCall
}

type afterCall struct {
	delay time.Duration
	fire  func()
	timer *time.Timer
}

func (f *fakeAfter) after(d time.Duration, fn func()) *time.Timer {
	t := time.NewTimer(time.Hour)
	f.mu.Lock()
	f.calls = append(f.calls, afterCall{delay: d, fire: fn, timer: t})
	f.mu.Unlock()
	return t
}

func (f *fakeAfter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// delays returns every recorded arm's delay.
func (f *fakeAfter) delays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, 0, len(f.calls))
	for _, call := range f.calls {
		out = append(out, call.delay)
	}
	return out
}

// last returns the most recent arm, which is the one a live timer would be.
func (f *fakeAfter) last() afterCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return afterCall{}
	}
	return f.calls[len(f.calls)-1]
}

// fireAt runs the function handed to the i'th arm, in this goroutine, so the
// test observes the fire's effects without waiting on anything.
func (f *fakeAfter) fireAt(t *testing.T, i int) {
	t.Helper()
	f.mu.Lock()
	if i >= len(f.calls) {
		f.mu.Unlock()
		t.Fatalf("no arm at index %d (%d recorded)", i, len(f.calls))
	}
	fn := f.calls[i].fire
	f.mu.Unlock()
	fn()
}

// newScheduleFixture builds a scheduler over the repo fixture's pusher and
// store, with the fake clock and timer factory, at a fixed instant so the
// computed delays are exact.
func newScheduleFixture(t *testing.T) (*repoScheduler, *repoFixture, *fakeAfter, time.Time) {
	t.Helper()
	f := newRepoFixture()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	timers := &fakeAfter{}
	s := newRepoScheduler(f.gp, f.store)
	s.now = func() time.Time { return base }
	s.after = timers.after
	return s, f, timers, base
}

// TestRepoScheduleFiresAtNext pins the arming rule: one timer per scheduled
// source, at ResolvedAt + RefreshInterval — and immediately (delay zero) for a
// source that has never been resolved, or whose next fire is already past.
func TestRepoScheduleFiresAtNext(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, timers, base := newScheduleFixture(t)
	ctx := context.Background()

	// ops_tools resolved two minutes ago on a five-minute schedule: three
	// minutes left.
	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_alice", "ops_tools", 5*time.Minute), "set interval")
	c.Require().NoError(f.store.SetResolved(ctx, "u_alice", "ops_tools", defaultResolveCommit, base.Add(-2*time.Minute)), "set resolved")
	// shared_lib is scheduled but was resolved an hour ago: the fire is overdue,
	// so it must arm for NOW rather than a negative delay.
	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_alice", "shared_lib", 5*time.Minute), "set shared_lib interval")
	c.Require().NoError(f.store.SetResolved(ctx, "u_alice", "shared_lib", defaultResolveCommit, base.Add(-time.Hour)), "set shared_lib resolved")
	// bob_lib is never resolved: due immediately.
	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_bob", "bob_lib", time.Hour), "set bob_lib interval")
	// A repo with no interval is never armed.

	s.start(ctx)
	c.Eq(1, f.store.allCalls, "ListAll calls on start")
	c.Require().Eq(3, timers.count(), "timers armed for the three scheduled sources")

	// Three minutes for ops_tools, zero for the two overdue/never-resolved.
	delays := timers.delays()
	sortDurations(delays)
	c.EqDiff([]time.Duration{0, 0, 3 * time.Minute}, delays, "armed delays")

	// Disarming one source leaves the others armed — `repo set --refresh off`
	// affects exactly the source it names.
	s.disarm("u_alice", "ops_tools")
	if _, ok := s.timers[repoKey("u_alice", "ops_tools")]; ok {
		t.Error("the disarmed source still has a timer")
	}
	c.Eq(2, len(s.timers), "timers after disarming one of three")
}

// sortDurations orders a duration slice in place, so the test can compare
// delays without depending on the order the store returned rows in.
func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// TestRepoScheduleRearmsAfterFire pins the fire's two halves: it refreshes the
// source, and it re-arms from NOW rather than from the schedule's ideal
// instant, so a refresh that overran its interval does not fire again at once.
func TestRepoScheduleRearmsAfterFire(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, timers, base := newScheduleFixture(t)
	ctx := context.Background()
	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_alice", "ops_tools", 5*time.Minute), "set interval")
	c.Require().NoError(f.store.SetResolved(ctx, "u_alice", "ops_tools", defaultResolveCommit, base), "set resolved")

	s.start(ctx)
	c.Require().Eq(1, timers.count(), "timers armed")
	c.Eq(5*time.Minute, timers.last().delay, "first arm delay")

	timers.fireAt(t, 0)

	// The fire refreshed through the pusher...
	c.Eq(1, f.clients["exec-alice"].callCount(), "SyncRepo calls after the fire")
	// ...recorded the resolution...
	if rec, ok := f.store.lastResolved("u_alice", "ops_tools"); !ok || rec.Commit != defaultResolveCommit {
		t.Fatalf("the fire recorded no commit: %+v (recorded %v)", rec, ok)
	}
	// ...and re-armed one interval out.
	c.Require().Eq(2, timers.count(), "arms after the fire")
	c.Eq(5*time.Minute, timers.last().delay, "re-arm delay")

	// Firing the re-armed timer refreshes again — the schedule keeps going.
	timers.fireAt(t, 1)
	c.Eq(2, f.clients["exec-alice"].callCount(), "SyncRepo calls after the second fire")
}

// TestRepoScheduleDisarmOnRemove pins that a removed source stops firing: the
// timer is dropped, and the fire function the timer already captured does
// nothing when it runs — a remove racing a due timer must not refresh a repo
// that no longer exists.
func TestRepoScheduleDisarmOnRemove(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, timers, base := newScheduleFixture(t)
	ctx := context.Background()
	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_alice", "ops_tools", 5*time.Minute), "set interval")
	c.Require().NoError(f.store.SetResolved(ctx, "u_alice", "ops_tools", defaultResolveCommit, base), "set resolved")

	s.start(ctx)
	c.Require().Eq(1, timers.count(), "timers armed")

	c.Require().NoError(f.store.Delete(ctx, "u_alice", "ops_tools"), "delete the registration")
	s.disarm("u_alice", "ops_tools")
	c.Eq(0, len(s.timers), "timers after disarm")

	// The timer that was armed before the removal still exists in the fake;
	// running it must be a no-op.
	timers.fireAt(t, 0)
	c.Eq(0, f.clients["exec-alice"].callCount(), "a stale timer refreshed a removed source")
	c.Eq(1, timers.count(), "a stale timer re-armed itself")
}

// TestRepoScheduleLogsOncePerOutage pins the logging rule: a source failing
// fire after fire is ONE outage and one warning, and the first success after
// it is ONE recovery line. Without it a five-minute schedule would fill the
// log with the same failure all day.
func TestRepoScheduleLogsOncePerOutage(t *testing.T) {
	c := assert.NewAborting(t)
	s, f, timers, base := newScheduleFixture(t)
	ctx := context.Background()
	var buf bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c.NoError(f.store.SetRefreshInterval(ctx, "u_alice", "ops_tools", 5*time.Minute), "set interval")
	c.NoError(f.store.SetResolved(ctx, "u_alice", "ops_tools", defaultResolveCommit, base), "set resolved")

	s.start(ctx)
	c.Require().Eq(1, timers.count(), "timers armed")

	f.clients["exec-alice"].err = errors.New("git fetch https://example.net/ops.git main: no route to host")

	// Three failing fires: one warning.
	for i := 0; i < 3; i++ {
		timers.fireAt(t, timers.count()-1)
	}
	c.Eq(1, strings.Count(buf.String(), "scheduled repo refresh failed"), "warnings across three identical failures:\n%s", buf.String())

	// Recovery: one line, and only once.
	f.clients["exec-alice"].err = nil
	timers.fireAt(t, timers.count()-1)
	timers.fireAt(t, timers.count()-1)
	c.Eq(1, strings.Count(buf.String(), "scheduled repo refresh recovered"), "recovery lines across two successes:\n%s", buf.String())

	// A DIFFERENT failure is a new outage, so it is logged again.
	f.clients["exec-alice"].err = errors.New("git fetch https://example.net/ops.git main: repository not found")
	timers.fireAt(t, timers.count()-1)
	c.Eq(2, strings.Count(buf.String(), "scheduled repo refresh failed"), "a new failure must be logged:\n%s", buf.String())
}

// TestRepoScheduleArmFollowsTheStore pins arm's contract as the three write
// verbs use it: it reads the registration and schedules it, and a source whose
// interval is on demand (or gone) is disarmed instead.
func TestRepoScheduleArmFollowsTheStore(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f, timers, base := newScheduleFixture(t)
	ctx := context.Background()
	s.start(ctx) // no scheduled sources yet
	c.Require().Eq(0, timers.count(), "timers armed on an empty schedule")

	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_alice", "ops_tools", 30*time.Minute), "set interval")
	c.Require().NoError(f.store.SetResolved(ctx, "u_alice", "ops_tools", defaultResolveCommit, base), "set resolved")
	s.arm("u_alice", "ops_tools")
	c.Require().Eq(1, timers.count(), "timers after arm")
	c.Eq(30*time.Minute, timers.last().delay, "armed delay")

	// Turning the interval off disarms.
	c.Require().NoError(f.store.SetRefreshInterval(ctx, "u_alice", "ops_tools", 0), "clear interval")
	s.arm("u_alice", "ops_tools")
	c.Eq(0, len(s.timers), "timers after the interval was cleared")

	// An unknown source is disarmed too, not armed with a zero delay.
	s.arm("u_alice", "never_registered")
	c.Eq(0, len(s.timers), "timers after arming an unknown source")

	// A nil scheduler is safe to arm and disarm: a daemon with no repo store
	// wires no scheduler, and the write verbs must not care.
	var none *repoScheduler
	none.arm("u_alice", "ops_tools")
	none.disarm("u_alice", "ops_tools")
	none.stop()
}
