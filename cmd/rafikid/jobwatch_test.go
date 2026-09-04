package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// jobWatchFixture is a controller with a job watcher wired to a real event
// buffer on a manual clock. The watcher's poll is scripted by the test via
// the returned channel; interval is 1ms so a loop test runs in real time
// while the buffer's debounce stays deterministic on the fake clock.
func jobWatchFixture(t *testing.T) (*Controller, *eventbuf.FakeClock, *capturedFlush, chan jobPoll) {
	t.Helper()
	clk := eventbuf.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cap := &capturedFlush{}
	buf.SetFlush(cap.fn)
	buf.SetBusy(func(string) bool { return false })

	c := &Controller{st: childstore.New(), cm: newChildManager(), evbuf: buf}
	c.bound = make(map[string]*boundExecutor)
	c.jobs = c.newControllerJobWatcher()
	c.jobs.interval = time.Millisecond

	polls := make(chan jobPoll, 16)
	c.jobs.poll = func(_ context.Context, _, _ string) (tools.JobSnapshot, error) {
		p := <-polls
		return p.snap, p.err
	}

	c.st.Insert(&childstore.Session{ChildID: "c_run", Status: protocol.StatusIdle, StartedAt: time.Now()})
	return c, clk, cap, polls
}

// restorePoll puts the controller's real poll wiring back after the fixture
// scripted it. Call it BEFORE arming any watch: the loop goroutine reads
// w.poll, so swapping it under a running watch is a data race.
func restorePoll(c *Controller) {
	c.jobs.poll = c.pollJob
}

type jobPoll struct {
	snap tools.JobSnapshot
	err  error
}

// waitJobGone waits until the watcher has dropped the entry — the
// synchronization point for "the loop saw the definitive answer and exited".
func waitJobGone(t *testing.T, w *jobWatcher, childID, handle string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		_, ok := w.jobs[jobKey{childID, handle}]
		w.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the watcher never dropped its entry")
}

func waitBatches(t *testing.T, cap *capturedFlush, n int) []capturedBatch {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := cap.batches(); len(got) >= n {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return cap.batches()
}

// The whole point: the agent learns the job finished as an injected frame,
// without calling bash_output once.
func TestJobWatchNotifiesOnExit(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "make check")

	polls <- jobPoll{snap: tools.JobSnapshot{Found: true}} // still running
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 3}}

	waitJobGone(t, c.jobs, "c_run", "job-1")
	clk.Advance(6 * time.Second)

	batches := waitBatches(t, cap, 1)
	if len(batches) != 1 {
		t.Fatalf("want exactly 1 batch, got %+v", batches)
	}
	if batches[0].childID != "c_run" || batches[0].source != jobEventSource {
		t.Fatalf("batch went to (%s, %s), want (c_run, %s)", batches[0].childID, batches[0].source, jobEventSource)
	}
	frag := strings.Join(batches[0].fragments, "\n")
	for _, want := range []string{"job-1", "make check", "exit code 3", `bash_output {"handle":"job-1"}`} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment %q must name %q", frag, want)
		}
	}
	// The buffer says something happened; the output says what. The fragment
	// must point at bash_output rather than carry the tail itself.
	if len(frag) > 400 {
		t.Errorf("fragment is trying to be the ledger (%d bytes): %q", len(frag), frag)
	}
}

// An exit-0 finish is still news worth one fragment.
func TestJobWatchNotifiesOnCleanExit(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "go build ./...")
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 0}}

	waitJobGone(t, c.jobs, "c_run", "job-1")
	clk.Advance(6 * time.Second)

	batches := waitBatches(t, cap, 1)
	if !strings.Contains(batches[0].fragments[0], "exit code 0") {
		t.Fatalf("got %q", batches[0].fragments[0])
	}
}

// Polling while the job runs must stay silent — that silence is what the
// agent is told it can rely on.
func TestJobWatchStaysQuietWhileRunning(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "npm run dev")
	for range 5 {
		polls <- jobPoll{snap: tools.JobSnapshot{Found: true}}
	}
	time.Sleep(20 * time.Millisecond)
	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a running job must not notify: %+v", got)
	}
	c.jobs.forget("c_run", "job-1")
}

// bash_kill resolved the job on purpose: no fragment, or the agent spends a
// turn learning something it did itself.
func TestForgetDropsTheWatchSilently(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "make check")
	c.jobs.forget("c_run", "job-1")
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 1}}
	time.Sleep(20 * time.Millisecond)
	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a killed job must not notify: %+v", got)
	}
}

// A handle the executor no longer knows (workspace released, executor
// restarted) is definitive: the agent is told once, then the watch ends.
func TestJobWatchNotifiesOnceForAGoneJob(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "make check")
	polls <- jobPoll{snap: tools.JobSnapshot{Found: false}}

	waitJobGone(t, c.jobs, "c_run", "job-1")
	clk.Advance(6 * time.Second)

	batches := waitBatches(t, cap, 1)
	frag := batches[0].fragments[0]
	if !strings.Contains(frag, "gone from its executor") {
		t.Fatalf("fragment must say the job is gone, not finished: %q", frag)
	}
	if strings.Contains(frag, "exit code") {
		t.Fatalf("a gone job has no exit code to report: %q", frag)
	}
}

// An executor blip is transient, not an answer: the watch must survive it and
// deliver the exit when the executor can be asked again. Ending the watch on
// the first error would strand every notification across a reconnect.
func TestJobWatchSurvivesTransientErrors(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "make check")
	polls <- jobPoll{err: execpool.ErrExecutorLost}
	polls <- jobPoll{err: errors.New("connection reset")}
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 2}}

	waitJobGone(t, c.jobs, "c_run", "job-1")
	clk.Advance(6 * time.Second)

	batches := waitBatches(t, cap, 1)
	if len(batches) != 1 {
		t.Fatalf("want exactly 1 batch after transient errors, got %+v", batches)
	}
	if !strings.Contains(batches[0].fragments[0], "exit code 2") {
		t.Fatalf("the watch must deliver after transient errors: %+v", batches)
	}
}

// An exited child cannot receive a turn; its workspace release already killed
// the job. Nothing to say, and nothing to keep polling.
func TestJobWatchSkipsAnExitedChild(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.st.SetStatus("c_run", protocol.StatusExited)
	c.jobs.watch("c_run", "job-1", "make check")
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 0}}

	waitJobGone(t, c.jobs, "c_run", "job-1")
	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("an exited child must not be notified: %+v", got)
	}
}

// Two jobs finishing together ride one coalesced frame — the same property
// the subagent settle path is pinned on.
func TestTwoJobsFinishAsOneBatch(t *testing.T) {
	c, clk, cap, polls := jobWatchFixture(t)
	c.jobs.watch("c_run", "job-1", "make check")
	c.jobs.watch("c_run", "job-2", "npm test")
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 0}}
	polls <- jobPoll{snap: tools.JobSnapshot{Found: true, Exited: true, ExitCode: 1}}

	waitJobGone(t, c.jobs, "c_run", "job-1")
	waitJobGone(t, c.jobs, "c_run", "job-2")
	clk.Advance(6 * time.Second)

	batches := waitBatches(t, cap, 1)
	if len(batches) != 1 || len(batches[0].fragments) != 2 {
		t.Fatalf("want 1 coalesced batch of 2 fragments, got %+v", batches)
	}
}

// --- wiring through controllerBinder and boundExecutor ---

// The daemon path end to end: boundExecutor.StartJob arms a watch on the
// controller's watcher, and the watch polls through peekJob (the CURRENT
// binding), never recover.
func TestStartJobWiresTheWatcherEndToEnd(t *testing.T) {
	c, clk, cap, _ := jobWatchFixture(t)
	fb := newFakeBinder()
	c.noteBoundExecutor("c_run", newBoundExecutor("c_run", fb))

	// The loop polls through the child's boundExecutor. Its fakeExec answers
	// Found: true forever, so the watch stays quiet — and the poll proving it
	// ran is the fakeExec's own call counter. The fixture's scripted poll is
	// restored to the controller's real wiring FIRST, before the watch is
	// armed: the loop goroutine reads w.poll.
	restorePoll(c)
	cb := &controllerBinder{c: c}
	cb.WatchJob("c_run", "job-1", "make check")

	c.jobs.mu.Lock()
	_, ok := c.jobs.jobs[jobKey{"c_run", "job-1"}]
	c.jobs.mu.Unlock()
	if !ok {
		t.Fatal("WatchJob did not arm a watch")
	}

	be := c.boundExecutorFor("c_run")
	cl, _, err := be.clientFor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fe := cl.(*fakeExec)
	time.Sleep(20 * time.Millisecond)
	fe.mu.Lock()
	polled := fe.calls
	fe.mu.Unlock()
	if polled == 0 {
		t.Fatal("the watcher never polled the child's binding")
	}
	// The fake answers Found:true forever, so the watch would tick forever.
	c.jobs.forget("c_run", "job-1")
	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a running job must stay quiet: %+v", got)
	}
}

// peekJob is the watcher's read, and it must NOT rebind: a re-provisioned
// workspace cannot resurrect a job, so recovering for a poll would spend a
// migration to learn the job is gone.
func TestPeekJobNeverRecovers(t *testing.T) {
	fb := newFakeBinder()
	fb.failWith = execpool.ErrExecutorLost // liveness failure: recover WOULD trigger on JobOutput
	be := newBoundExecutor("child-1", fb)

	_, err := be.peekJob(context.Background(), "job-1")
	if err == nil {
		t.Fatal("peekJob against an unbound child must fail; there is nothing to poll")
	}
	if fb.provisions != 0 {
		t.Fatalf("peekJob provisioned %d workspaces; a poll must never rebind", fb.provisions)
	}
}
