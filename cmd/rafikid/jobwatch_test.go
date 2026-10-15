package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// --- helpers ---

// ackRecorder is the test's jobAcks seam: it records every AckJobExits call as
// "executorID|handle" and can also append into a shared order log when a test
// wants push/ack ordering.
type ackRecorder struct {
	mu  sync.Mutex
	got []string
	log *orderLog // optional: records "ack:<executor>|<handle>" for ordering
}

func (r *ackRecorder) AckJobExits(_ context.Context, executorID string, handles []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range handles {
		r.got = append(r.got, executorID+"|"+h)
		if r.log != nil {
			r.log.add("ack:" + executorID + "|" + h)
		}
	}
	return nil
}

func (r *ackRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// orderLog records the interleaving of pushes and acks.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, s)
}

func (o *orderLog) all() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// orderAcceptRecorder stands in for the durable inbox: it records each
// accepted fragment's key synchronously, on the Push call, so a test can prove
// the fragment was handed to the buffer BEFORE the executor was acked.
type orderAcceptRecorder struct{ log *orderLog }

func (a orderAcceptRecorder) Accept(_ context.Context, in inbox.Inbound) (string, error) {
	a.log.add("push:" + in.Key)
	return "row-" + in.Key, nil
}

// jobEventFixture is settleFixture plus the job-event seams: an ack recorder
// installed as the controller's jobAcks. The five workers and the coordinator
// give the handler deliverable owners to route to.
func jobEventFixture(t *testing.T) (*Controller, *clock.FakeClock, *capturedFlush, *ackRecorder) {
	t.Helper()
	c, clk, cap := settleFixture(t)
	acks := &ackRecorder{}
	c.jobAcks = acks
	return c, clk, cap, acks
}

func jobRunningEvent(executorID, handle, owner, command string) execpool.JobEvent {
	return execpool.JobEvent{
		ExecutorID: executorID, Kind: execpool.JobEventRunning,
		Handle: handle, Owner: owner, Command: command,
	}
}

func jobExitedEvent(executorID, handle, owner, command string, code int) execpool.JobEvent {
	return execpool.JobEvent{
		ExecutorID: executorID, Kind: execpool.JobEventExited,
		Handle: handle, Owner: owner, Command: command, ExitCode: code,
	}
}

func replayCompleteEvent(executorID string) execpool.JobEvent {
	return execpool.JobEvent{ExecutorID: executorID, Kind: execpool.JobEventReplayComplete}
}

func jobReplayStartEvent(executorID string) execpool.JobEvent {
	return execpool.JobEvent{ExecutorID: executorID, Kind: execpool.JobEventReplayStart}
}

// The no-loss rule, pinned directly: the exit fragment must be accepted by the
// buffer BEFORE the executor is acked. Reversing them means a crash between the
// two loses the notification the ack promised was delivered.
func TestJobEventExitedPushesThenAcks(t *testing.T) {
	c, clk, _, acks := jobEventFixture(t)
	order := &orderLog{}
	acks.log = order
	c.evbuf.SetAccepter(orderAcceptRecorder{log: order})

	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(jobExitedEvent("exec-1", "h1", "c_coord", "make check", 3))
	clk.Advance(6 * time.Second)

	assert.NewAborting(t).EqDeep([]string{"push:h1", "ack:exec-1|h1"}, order.all(),
		"the fragment must be pushed before the ack")
}

// An exit for a child that can no longer receive the news (it exited) is still
// acked — the executor may drop it — but nothing is pushed: there is nobody to
// tell. Acking is what stops the executor replaying it forever.
func TestJobEventExitedForExitedChildAcksWithoutPush(t *testing.T) {
	c, clk, cap, acks := jobEventFixture(t)
	c.st.SetStatus("c_w1", protocol.StatusExited)

	c.handleJobEvent(jobExitedEvent("exec-1", "h1", "c_w1", "make check", 0))
	clk.Advance(6 * time.Second)

	assert.NewAborting(t).Empty(cap.batches(), "an exited child must not be pushed to")
	assert.NewAborting(t).EqDeep([]string{"exec-1|h1"}, acks.calls(), "the exit must still be acked")
}

// A job that was running at the end of one subscription and is NOT running at
// the next one's replay is gone: the executor that held it no longer knows it,
// and neither its output nor its exit survived.
func TestJobEventReplayCompleteReportsGoneHandles(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, _ := jobEventFixture(t)

	// First replay: h1 and h2 are both running.
	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h2", "c_coord", "npm test"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))

	// A later replay reports only h1 still running: h2 is gone.
	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Len(batches, 1, "want exactly one gone fragment, got")
	ck.Eq("c_coord", batches[0].childID, "gone fragment went to")
	ck.Eq(jobEventSource, batches[0].source, "source")
	ck.Len(batches[0].fragments, 1, "want exactly one fragment, got")
	frag := batches[0].fragments[0]
	ck.StrContains(frag, "h2", "the gone fragment must name the lost handle")
	ck.StrContains(frag, "no longer on its executor", "the fragment must say the job is gone")
	ck.NotStrContains(frag, "h1", "the still-running handle must not be reported gone")
}

// The first subscription after a daemon start has an empty running set, so its
// ReplayComplete reports nothing gone. Reporting everything the executor did
// NOT replay would spam every child on every reconnect.
func TestJobEventFirstReplayReportsNothingGone(t *testing.T) {
	c, clk, cap, _ := jobEventFixture(t)

	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))
	clk.Advance(6 * time.Second)

	assert.NewAborting(t).Empty(cap.batches(), "the first replay must report nothing gone")
}

// A handle whose Running was learned from the CURRENT replay's own stream is in
// the replay set, so the ReplayComplete that closes that replay must NOT call
// it gone — the executor just told us it is still running.
func TestJobEventRunningLearnedThisReplayIsNotGone(t *testing.T) {
	c, clk, cap, _ := jobEventFixture(t)

	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))
	clk.Advance(6 * time.Second)

	assert.NewAborting(t).Empty(cap.batches(), "a job running at replay time must not be reported gone")
}

// A job the agent killed itself is not "gone": gone means an executor restart
// lost it. boundExecutor.KillJob drops the handle from the running set, so the
// next replay must report nothing.
func TestJobEventSelfKillIsNotReportedGone(t *testing.T) {
	c, clk, cap, _ := jobEventFixture(t)

	// h1 is running on exec-1, learned in a first replay.
	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))

	// The agent kills it: boundExecutor.KillJob calls the binder, which lands
	// here.
	c.forgetRunningJob("exec-1", "h1")

	// The next replay reports nothing running. Without the kill dropping the
	// entry, h1 would be reported gone.
	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))
	clk.Advance(6 * time.Second)

	assert.NewAborting(t).Empty(cap.batches(), "a self-killed job must never be reported gone")
}

// A job started LIVE (after a subscription's ReplayComplete) that is missing
// from the NEXT replay is gone. This is F2's scenario: without an explicit
// replay phase the live Running leaks into the seen-set, so the next replay
// skips the lost handle and never reports it — for a whole reconnect cycle.
func TestJobEventLiveJobMissingFromNextReplayIsGone(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap, _ := jobEventFixture(t)

	// Replay 1: the executor reports h1 running.
	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))

	// h2 starts LIVE, after the replay: not a replayed event.
	c.handleJobEvent(jobRunningEvent("exec-1", "h2", "c_coord", "npm test"))

	// Replay 2: the executor reports only h1 — it has lost h2.
	c.handleJobEvent(jobReplayStartEvent("exec-1"))
	c.handleJobEvent(jobRunningEvent("exec-1", "h1", "c_coord", "make check"))
	c.handleJobEvent(replayCompleteEvent("exec-1"))
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Len(batches, 1, "want exactly one gone fragment, got")
	ck.Len(batches[0].fragments, 1, "want exactly one fragment, got")
	frag := batches[0].fragments[0]
	ck.StrContains(frag, "h2", "the live-started, now-lost handle must be reported gone")
	ck.NotStrContains(frag, "h1", "the still-running handle must not be reported gone")
}

// A repeated exit collapses on the (child, source, handle) key while the first
// fragment is still pending, so the child reads one notice, not two. This is
// what lets an unacked exit be replayed without double-notifying.
func TestJobEventDuplicateExitCollapses(t *testing.T) {
	c, clk, cap, acks := jobEventFixture(t)

	c.handleJobEvent(jobExitedEvent("exec-1", "h1", "c_coord", "make check", 0))
	c.handleJobEvent(jobExitedEvent("exec-1", "h1", "c_coord", "make check", 0))
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	assert.NewAborting(t).False(len(batches) != 1 || len(batches[0].fragments) != 1,
		"a duplicate exit must collapse to one fragment, got %+v", batches)
	assert.NewAborting(t).Len(acks.calls(), 2, "each exit is acked; got")
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
