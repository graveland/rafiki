package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sched"

	"github.com/multigres/testkit/assert"
)

// jobWatchFixture is a controller with a job watcher wired to a real event
// buffer on a manual clock and a manual scheduler. Polls and re-arms run
// synchronously inside clk.Advance, so a test drives the watch a tick at a
// time; the buffer's debounce also lives on the same clock, so a flush is one
// more Advance. interval is one second, so each Advance(time.Second) is one
// poll and Advance(5*time.Second) both flushes and (were a watch live) ticks.
func jobWatchFixture(t *testing.T) (*Controller, *clock.FakeClock, *capturedFlush, *jobPollScript) {
	t.Helper()
	clk := clock.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cap := &capturedFlush{}
	buf.SetFlush(cap.fn)
	buf.SetBusy(func(string) bool { return false })

	c := &Controller{st: childstore.New(), cm: newChildManager(), evbuf: buf}
	c.bound = make(map[string]*boundExecutor)
	c.clk = clk
	c.sched = sched.New(clk, time.Second)
	c.jobs = c.newControllerJobWatcher()
	c.jobs.interval = time.Second

	script := &jobPollScript{}
	c.jobs.poll = script.poll

	c.st.Insert(&childstore.Session{ChildID: "c_run", Status: protocol.StatusIdle, StartedAt: time.Now()})
	return c, clk, cap, script
}

// restorePoll puts the controller's real poll wiring back after the fixture
// scripted it, for the wiring tests that need to poll through the child's
// binding rather than a scripted answer.
func restorePoll(c *Controller) {
	c.jobs.poll = c.pollJob
}

type jobPoll struct {
	snap tools.JobSnapshot
	err  error
}

// jobPollScript scripts the watcher's poll with a fixed sequence: each call
// returns the next step, then repeats the last one forever. Polls run
// synchronously inside FakeClock.Advance, so no locking is needed.
type jobPollScript struct {
	steps []jobPoll
	i     int
}

func (s *jobPollScript) poll(context.Context, string, string) (tools.JobSnapshot, error) {
	if s.i < len(s.steps) {
		p := s.steps[s.i]
		s.i++
		return p.snap, p.err
	}
	if len(s.steps) == 0 {
		return tools.JobSnapshot{}, nil
	}
	last := s.steps[len(s.steps)-1]
	return last.snap, last.err
}

// The whole point: the agent learns the job finished as an injected frame,
// without calling bash_output once. The watch polls once while the job runs,
// then polls the exit and ends.
func TestJobWatchPushesOnExit(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{
		{snap: tools.JobSnapshot{Found: true}}, // still running
		{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 3}},
	}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second)     // poll: running; re-arm
	clk.Advance(time.Second)     // poll: exited; push and stop
	clk.Advance(5 * time.Second) // let the buffer's debounce flush

	batches := cap.batches()
	ck.Require().Len(batches, 1, "want exactly 1 batch, got")
	if batches[0].childID != "c_run" || batches[0].source != jobEventSource {
		t.Fatalf("batch went to (%s, %s), want (c_run, %s)", batches[0].childID, batches[0].source, jobEventSource)
	}
	frag := strings.Join(batches[0].fragments, "\n")
	for _, want := range []string{"job-1", "make check", "exit code 3", `bash_output {"handle":"job-1"}`} {
		ck.StrContains(frag, want, "fragment")
	}
	// The buffer says something happened; the output says what. The fragment
	// must point at bash_output rather than carry the tail itself.
	ck.LessOrEqual(400, len(frag), "fragment is trying to be the ledger (%d bytes): %q", len(frag), frag)
	// A definitive answer leaves no registration behind.
	ck.Empty(c.sched.Pending(), "an exited job must leave no pending registration")
}

// An exit-0 finish is still news worth one fragment.
func TestJobWatchNotifiesOnCleanExit(t *testing.T) {
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 0}}}
	c.jobs.watch("c_run", "job-1", "go build ./...")

	clk.Advance(time.Second)
	clk.Advance(5 * time.Second)

	batches := cap.batches()
	assert.NewAborting(t).False(len(batches) != 1, "want exactly 1 batch, got %+v", batches)
	assert.NewAborting(t).StrContains(batches[0].fragments[0], "exit code 0", "got")
}

// Polling while the job runs must stay silent — that silence is what the
// agent is told it can rely on.
func TestJobWatchStaysQuietWhileRunning(t *testing.T) {
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{{snap: tools.JobSnapshot{Found: true}}}
	c.jobs.watch("c_run", "job-1", "npm run dev")

	clk.Advance(time.Second)
	clk.Advance(time.Second)
	clk.Advance(time.Second)
	assert.NewAborting(t).Empty(cap.batches(), "a running job must not notify")
	c.jobs.forget("c_run", "job-1")
}

// bash_kill resolved the job on purpose: no fragment, or the agent spends a
// turn learning something it did itself.
func TestJobWatchForgetCancelsWithoutPush(t *testing.T) {
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 1}}}
	c.jobs.watch("c_run", "job-1", "make check")
	c.jobs.forget("c_run", "job-1")

	clk.Advance(time.Second)     // no poll: the registration was cancelled
	clk.Advance(5 * time.Second) // nothing to flush

	assert.NewAborting(t).Empty(cap.batches(), "a killed job must not notify")
	assert.NewAborting(t).Empty(c.sched.Pending(), "forget must cancel the scheduler registration")
}

// A handle the executor no longer knows (workspace released, executor
// restarted) is definitive: the agent is told once, then the watch ends.
func TestJobWatchPushesWhenHandleIsGone(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{{snap: tools.JobSnapshot{Found: false}}}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second)
	clk.Advance(5 * time.Second)

	batches := cap.batches()
	ck.False(len(batches) != 1, "want exactly 1 batch, got %+v", batches)
	frag := batches[0].fragments[0]
	ck.StrContains(frag, "no longer on its executor", "fragment must say the job is gone, not finished")
	ck.NotStrContains(frag, "exit code", "a gone job has no exit code to report")
	ck.Empty(c.sched.Pending(), "a gone job must leave no pending registration")
}

// An executor blip is transient, not an answer: the watch must survive it and
// deliver the exit when the executor can be asked again. Ending the watch on
// the first error would strand every notification across a reconnect.
func TestJobWatchKeepsWatchingThroughTransientErrors(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{
		{err: execpool.ErrExecutorLost},
		{err: errors.New("connection reset")},
		{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 2}},
	}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second) // transient; re-arm
	clk.Advance(time.Second) // transient; re-arm
	clk.Advance(time.Second) // exit; push and stop
	clk.Advance(5 * time.Second)

	batches := cap.batches()
	ck.Len(batches, 1, "want exactly 1 batch after transient errors, got")
	ck.StrContains(batches[0].fragments[0], "exit code 2", "the watch must deliver after transient errors: %+v", batches)
}

// An exited child cannot receive a turn; its workspace release already killed
// the job. Nothing to say, and nothing to keep polling.
func TestJobWatchSkipsAnExitedChild(t *testing.T) {
	c, clk, cap, _ := jobWatchFixture(t)
	c.st.SetStatus("c_run", protocol.StatusExited)
	var polls atomic.Int64
	c.jobs.poll = func(context.Context, string, string) (tools.JobSnapshot, error) {
		polls.Add(1)
		return tools.JobSnapshot{Found: true, Exited: true, ExitCode: 0}, nil
	}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second)
	clk.Advance(5 * time.Second)
	assert.NewAborting(t).Empty(cap.batches(), "an exited child must not be notified")
	assert.NewAborting(t).Eq(int64(0), polls.Load(), "an exited child must not be polled")
}

// A child that can no longer receive the news ends its watch even when the
// poll never succeeds again — which is exactly the state Close leaves behind,
// since it drops the binding pollJob resolves through. The liveness check sits
// BEFORE the poll, so no poll ever happens and no registration survives; a
// check reachable only after a successful poll left the watch polling forever:
// one every 5s in production, for the daemon's life.
func TestJobWatchEndsWhenChildNotAlive(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, _ := jobWatchFixture(t)

	var polls atomic.Int64
	c.jobs.poll = func(context.Context, string, string) (tools.JobSnapshot, error) {
		polls.Add(1)
		// Verbatim what Controller.pollJob answers once the binding is gone.
		return tools.JobSnapshot{}, errors.New("no executor bound for child c_run")
	}
	c.jobs.watch("c_run", "job-1", "make check")

	// The child exits and the operator closes it: store entry deleted, and
	// with it every future poll's chance of succeeding.
	c.st.SetStatus("c_run", protocol.StatusExited)
	c.st.Delete("c_run")
	c.forgetBoundExecutor("c_run")

	clk.Advance(3 * time.Second)

	ck.Eq(int64(0), polls.Load(), "the pre-poll liveness check must run before the poll")
	ck.Empty(c.sched.Pending(), "a watch for a gone child must leave no registration")
	ck.Empty(cap.batches(), "a child that is gone must not be pushed to")
}

// Re-watching the same (child, handle) replaces the first watch, and the
// replaced registration must never poll: one scheduler entry per job means
// the second watch's At stops the first's timer.
func TestJobWatchReplacedWatchIsNotPolledByStaleRegistration(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, _ := jobWatchFixture(t)

	var polls atomic.Int64
	c.jobs.poll = func(context.Context, string, string) (tools.JobSnapshot, error) {
		polls.Add(1)
		return tools.JobSnapshot{Found: true}, nil
	}
	c.jobs.watch("c_run", "job-1", "make check")
	c.jobs.watch("c_run", "job-1", "make check") // replaces the first

	clk.Advance(time.Second)

	ck.Eq(int64(1), polls.Load(), "a re-watched job must be polled once, not once per registration")
	c.jobs.forget("c_run", "job-1")
}

// The stall guard, driven from inside the poll: a watch that replaces its own
// key while a poll is in flight must survive. The stale tick re-checks
// ownership under the lock that also arms the successor, so it cannot re-arm
// over the successor's registration — if it could, the successor's own later
// fire would decline to re-arm (it no longer owns the key) and the watch would
// never poll again. Exactly one registration must remain, and the next tick
// must poll the successor.
func TestJobWatchReplacementDuringPollKeepsSuccessorRegistration(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, _ := jobWatchFixture(t)

	var polls atomic.Int64
	var replaced atomic.Bool
	c.jobs.poll = func(context.Context, string, string) (tools.JobSnapshot, error) {
		polls.Add(1)
		if replaced.CompareAndSwap(false, true) {
			// Re-entrant same-key replacement, as a concurrent watch would do.
			c.jobs.watch("c_run", "job-1", "make check")
		}
		return tools.JobSnapshot{Found: true}, nil
	}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second) // poll 1 replaces its own key mid-poll

	ck.Len(c.sched.Pending(), 1, "the successor watch must keep exactly one registration")

	clk.Advance(time.Second) // the successor's own tick must poll again
	ck.Eq(int64(2), polls.Load(), "the stale tick clobbered the successor's registration; the watch stalled")

	c.jobs.forget("c_run", "job-1")
}

// The forget flavour of the same guard: a watch forgotten while its poll is in
// flight must leave no registration and must never poll again.
func TestJobWatchForgetDuringPollLeavesNoRegistration(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, _ := jobWatchFixture(t)

	var polls atomic.Int64
	var forgot atomic.Bool
	c.jobs.poll = func(context.Context, string, string) (tools.JobSnapshot, error) {
		polls.Add(1)
		if forgot.CompareAndSwap(false, true) {
			c.jobs.forget("c_run", "job-1")
		}
		return tools.JobSnapshot{Found: true}, nil
	}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second) // poll 1 forgets its own key mid-poll

	ck.Empty(c.sched.Pending(), "a watch forgotten during its poll must leave no registration")
	ck.Eq(int64(1), polls.Load(), "the forgotten watch must poll exactly once")

	clk.Advance(time.Second)
	ck.Eq(int64(1), polls.Load(), "the forgotten watch must never poll again")
}

// A cancelled base context stops the watch at the next tick without polling
// and without re-arming: the daemon is shutting down, so there is nothing left
// to say and nothing left to schedule.
func TestJobWatchStopsOnBaseContextCancel(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, _, _ := jobWatchFixture(t)

	var polls atomic.Int64
	c.jobs.poll = func(context.Context, string, string) (tools.JobSnapshot, error) {
		polls.Add(1)
		return tools.JobSnapshot{Found: true}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.jobs.baseCtx = ctx
	c.jobs.watch("c_run", "job-1", "make check")
	cancel()

	clk.Advance(time.Second)

	ck.Eq(int64(0), polls.Load(), "a cancelled base context must stop the watch before it polls")
	ck.Empty(c.sched.Pending(), "a cancelled watch must not re-arm")
}

// A watch is one scheduler registration, not a goroutine per job. Fifty
// watches on a shared scheduler must not grow the goroutine count.
func TestJobWatchSpawnsNoGoroutinePerJob(t *testing.T) {
	c, _, _, _ := jobWatchFixture(t)

	before := runtime.NumGoroutine()
	for i := range 50 {
		c.jobs.watch("c_run", fmt.Sprintf("job-%d", i), "make check")
	}
	after := runtime.NumGoroutine()
	assert.NewAborting(t).True(after-before < 5,
		"watching 50 jobs grew the goroutine count by %d; a watch must be a scheduler registration, not a goroutine", after-before)
}

// Two jobs finishing together ride one coalesced frame — the same property
// the subagent settle path is pinned on.
func TestTwoJobsFinishAsOneBatch(t *testing.T) {
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{
		{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 0}},
		{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 1}},
	}
	c.jobs.watch("c_run", "job-1", "make check")
	c.jobs.watch("c_run", "job-2", "npm test")

	clk.Advance(time.Second)     // both exit
	clk.Advance(5 * time.Second) // one coalesced flush

	batches := cap.batches()
	assert.NewAborting(t).False(len(batches) != 1 || len(batches[0].fragments) != 2, "want 1 coalesced batch of 2 fragments, got %+v", batches)
}

// The pre-poll check must not cost a live child its notification: a child
// still able to receive a turn is polled and told, as before.
func TestJobWatchStillNotifiesALiveChild(t *testing.T) {
	c, clk, cap, script := jobWatchFixture(t)
	script.steps = []jobPoll{{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 7}}}
	c.jobs.watch("c_run", "job-1", "make check")

	clk.Advance(time.Second)
	clk.Advance(5 * time.Second)

	batches := cap.batches()
	assert.NewAborting(t).False(len(batches) != 1 || !strings.Contains(batches[0].fragments[0], "exit code 7"), "a live child must still be notified: %+v", batches)
}

// --- wiring through controllerBinder and boundExecutor ---

// The daemon path end to end: boundExecutor.StartJob arms a watch on the
// controller's watcher, and the watch polls through peekJob (the CURRENT
// binding), never recover.
func TestStartJobWiresTheWatcherEndToEnd(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, _ := jobWatchFixture(t)
	fb := newFakeBinder()
	c.noteBoundExecutor("c_run", newBoundExecutor("c_run", fb))

	// The watch polls through the child's boundExecutor, so restore the
	// controller's real wiring before arming it.
	restorePoll(c)
	cb := &controllerBinder{c: c}
	cb.WatchJob("c_run", "job-1", "make check")

	c.jobs.mu.Lock()
	_, ok := c.jobs.jobs[jobKey{"c_run", "job-1"}]
	c.jobs.mu.Unlock()
	ck.True(ok, "WatchJob did not arm a watch")

	// peekJob reads the binding's client; clientFor is what binds it, so bind
	// before the tick that polls.
	be := c.boundExecutorFor("c_run")
	cl, _, err := be.clientFor(context.Background())
	ck.NoError(err)
	fe := cl.(*fakeExec)

	clk.Advance(time.Second) // one tick, polling the child's binding

	fe.mu.Lock()
	polled := fe.calls
	fe.mu.Unlock()
	ck.NotEq(0, polled, "the watcher never polled the child's binding")
	// The fake answers Found:true forever, so the watch would tick forever.
	c.jobs.forget("c_run", "job-1")
	clk.Advance(5 * time.Second)
	ck.Empty(cap.batches(), "a running job must stay quiet")
}

// peekJob is the watcher's read, and it must NOT rebind: a re-provisioned
// workspace cannot resurrect a job, so recovering for a poll would spend a
// migration to learn the job is gone.
func TestPeekJobNeverRecovers(t *testing.T) {
	c := assert.NewAborting(t)
	fb := newFakeBinder()
	fb.failWith = execpool.ErrExecutorLost // liveness failure: recover WOULD trigger on JobOutput
	be := newBoundExecutor("child-1", fb)

	_, err := be.peekJob(context.Background(), "job-1")
	c.Error(err, "peekJob against an unbound child must fail; there is nothing to poll")
	c.Eq(0, fb.provisions, "peekJob provisioned")
}

// A child's binding must not outlive the child. Close already dropped it, but
// Close is OPTIONAL — a child that exits and is never closed would otherwise
// hold its boundExecutor, and the executor client it references, for the
// daemon's lifetime. This drives the real exit path (Kill waits for cm.Remove,
// the final step of handleChildExit) rather than calling handleChildExit by
// hand, because the drop has to sit on that path to be worth anything.
func TestExitDropsTheChildsBoundExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)
	ctrl.noteBoundExecutor(childID, newBoundExecutor(childID, newFakeBinder()))

	c.NotNil(ctrl.boundExecutorFor(childID), "the fixture did not retain a binding to begin with")

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := ctrl.Kill(killCtx, childID, 2*time.Second, 500*time.Millisecond)
	c.NoError(err, "kill")
	waitForExited(t, ctrl.st, childID, 5*time.Second)

	c.Nil(ctrl.boundExecutorFor(childID), "an exited child still holds its boundExecutor; nothing will ever drop it")
}
