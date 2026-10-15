// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// insertDisplayChild adds a session to st with an optional parent/root label
// pair, the shape the display-state tree walk reads.
func insertDisplayChild(st *childstore.Store, id, parent, root string, status protocol.Status) {
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
		labels[childstore.LabelRoot] = root
	}
	st.Insert(&childstore.Session{
		ChildID: id, Status: status, Kind: protocol.KindFundi,
		StartedAt: time.Now(), Labels: labels,
	})
}

// mustUpdate applies fn to a session, failing the test if the id is unknown.
func mustUpdate(t *testing.T, c *Controller, childID string, fn func(*childstore.Session)) {
	t.Helper()
	if err := c.st.Update(childID, fn); err != nil {
		t.Fatalf("update %s: %v", childID, err)
	}
}

// displayServiceController builds a Controller through NewController so the
// display-state refresh worker is running and the native bus is wired. The
// pool is nil, so the event log is in memory.
func displayServiceController(t *testing.T) *Controller {
	t.Helper()
	dir := testSocketDir(t)
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")
	for _, d := range []string{stateDir, logsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdirall %s: %v", d, err)
		}
	}
	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, filepath.Join(dir, "c.sock"), nil, nil, nil, false, t.Context(), nil, nil, nil, nil)
	t.Cleanup(func() { ctrl.Stop() })
	return ctrl
}

// displayLastOf reads the last published tuple for childID, or ok=false when// nothing has been published. Callers drain first.
func displayLastOf(c *Controller, childID string) (displayValue, bool) {
	c.displayMu.Lock()
	defer c.displayMu.Unlock()
	v, ok := c.displayLast[childID]
	return v, ok
}

// operatorStates drains a subscription channel of the operator_state payloads
// currently queued for it.
func operatorStates(ch <-chan *rafikiv1.Event) []*rafikiv1.OperatorState {
	var out []*rafikiv1.OperatorState
	for {
		select {
		case ev := <-ch:
			if os := ev.GetOperatorState(); os != nil {
				out = append(out, os)
			}
		default:
			return out
		}
	}
}

// blockingEventLog blocks every Append until released, so a test can prove a
// caller is never held up by the worker's publish.
type blockingEventLog struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingEventLog) Append(ctx context.Context, _ string, _ *rafikiv1.Event) (int32, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return 0, nil
}

func (b *blockingEventLog) Read(context.Context, string, int32, int) ([]eventlog.Record, error) {
	return nil, nil
}

func (b *blockingEventLog) Latest(context.Context, string) (int32, error) {
	return 0, eventlog.ErrNotFound
}

// ─── displayFacts ─────────────────────────────────────────────────────────────

// TestDisplayFactsCountsRunningJobsByOwner: the running-job count spans every
// executor's map and counts only the child's own jobs.
func TestDisplayFactsCountsRunningJobsByOwner(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New()}
	insertDisplayChild(c.st, "c_owner", "", "", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_other", "", "", protocol.StatusIdle)
	c.jobRunning = map[string]map[string]jobRef{
		"e1": {"h1": {owner: "c_owner"}, "h2": {owner: "c_other"}},
		"e2": {"h3": {owner: "c_owner"}},
	}

	f, ok := c.displayFacts("c_owner")
	a.Require().True(ok, "c_owner is known")
	a.Eq(2, f.RunningJobs, "only c_owner's two jobs count")
}

// TestDisplayFactsLiveDescendantsCountsOnlyNonIdleNonExited: a live descendant
// is one whose own machine status is neither idle nor exited; a settled worker
// sits at idle until closed, so counting "not exited" would hold its
// coordinator in waiting forever.
func TestDisplayFactsLiveDescendantsCountsOnlyNonIdleNonExited(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New()}
	// c_root tree: one streaming (with a running grandchild) + one idle leaf +
	// one exited descendant.
	insertDisplayChild(c.st, "c_root", "", "", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_stream", "c_root", "c_root", protocol.StatusStreaming)
	insertDisplayChild(c.st, "c_run", "c_stream", "c_root", protocol.StatusRunning)
	insertDisplayChild(c.st, "c_idle", "c_root", "c_root", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_exited", "c_root", "c_root", protocol.StatusExited)

	// A second tree: an idle child whose grandchild is working.
	insertDisplayChild(c.st, "c_top2", "", "", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_mid", "c_top2", "c_top2", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_grand", "c_mid", "c_top2", protocol.StatusToolRunning)

	f, ok := c.displayFacts("c_root")
	a.Require().True(ok, "c_root is known")
	a.Eq(2, f.LiveDescendants, "c_stream and c_run count; idle and exited do not")

	f, _ = c.displayFacts("c_idle")
	a.Eq(0, f.LiveDescendants, "an all-idle subtree has no live descendants")

	f, _ = c.displayFacts("c_top2")
	a.Eq(1, f.LiveDescendants, "an idle child with a working grandchild counts 1")
}

// TestDisplayFactsTopLevel: TopLevel is "no parent label".
func TestDisplayFactsTopLevel(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New()}
	insertDisplayChild(c.st, "c_root", "", "", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_kid", "c_root", "c_root", protocol.StatusIdle)

	f, ok := c.displayFacts("c_root")
	a.Require().True(ok, "c_root is known")
	a.True(f.TopLevel, "a child with no parent is top level")

	f, _ = c.displayFacts("c_kid")
	a.False(f.TopLevel, "a child with a parent is not top level")
}

// TestDisplayFactsPendingEventsFollowsBuffer: PendingEvents mirrors the event
// buffer's Pending, and a nil buffer is never pending.
func TestDisplayFactsPendingEventsFollowsBuffer(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New()}
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)

	f, _ := c.displayFacts("c_1")
	a.False(f.PendingEvents, "no buffer means never pending")

	clk := clock.NewFakeClock(time.Now())
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	buf.SetBusy(func(string) bool { return false })
	c.evbuf = buf

	f, _ = c.displayFacts("c_1")
	a.False(f.PendingEvents, "an empty buffer is not pending")

	buf.Push("c_1", "subagents", "", "news")
	f, _ = c.displayFacts("c_1")
	a.True(f.PendingEvents, "a queued fragment is pending")

	clk.Advance(6 * time.Second) // past the debounce: the batch flushes
	f, _ = c.displayFacts("c_1")
	a.False(f.PendingEvents, "a flushed batch is no longer pending")
}

// ─── refreshDisplayState ──────────────────────────────────────────────────────

// TestRefreshDisplayStateSettleWindowIsWaitingNotDone: a coordinator with a
// settled idle worker and an undelivered settle fragment reads waiting/events,
// never idle, until the buffer flushes; then it reads idle.
func TestRefreshDisplayStateSettleWindowIsWaitingNotDone(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_coord", "", "", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_w", "c_coord", "c_coord", protocol.StatusIdle)

	clk := clock.NewFakeClock(time.Now())
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	buf.SetBusy(func(string) bool { return false })
	buf.SetFlush(func(string, string, []inbox.Inbound) {})
	buf.SetOnChange(c.refreshDisplayState)
	c.evbuf = buf

	buf.Push("c_coord", "subagents", "", "worker settled")
	c.refreshDisplayState("c_coord")
	c.drainDisplayRefresh()

	last, ok := displayLastOf(c, "c_coord")
	a.Require().True(ok, "an operator state was published for the coordinator")
	a.Eq(protocol.DisplayWaiting, last.state, "a settle in flight is waiting, not idle")
	a.Eq(protocol.WaitingOnEvents, last.on, "waiting on the event buffer")

	clk.Advance(6 * time.Second) // the buffer flushes and notifies
	c.drainDisplayRefresh()

	last, _ = displayLastOf(c, "c_coord")
	a.Eq(protocol.DisplayIdle, last.state, "after the flush the coordinator is idle")
	a.Eq(protocol.WaitingOn(""), last.on, "idle waits on nothing")
}

// TestRefreshDisplayStatePublishesOnlyOnChange: an unchanged tuple republishes
// nothing; a changed fact publishes once more.
func TestRefreshDisplayStatePublishesOnlyOnChange(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.PendingQuestion = "proceed?" })

	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	c.refreshDisplayState("c_1")
	c.drainDisplayRefresh()
	a.Len(operatorStates(ch), 1, "the first refresh publishes the waiting/operator state")

	c.refreshDisplayState("c_1")
	c.drainDisplayRefresh()
	a.Len(operatorStates(ch), 0, "an unchanged tuple republishes nothing")

	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.PendingQuestion = "" })
	c.refreshDisplayState("c_1")
	c.drainDisplayRefresh()
	a.Len(operatorStates(ch), 1, "a changed tuple publishes once more")
}

// TestRefreshDisplayStateWalksAncestors: a grandchild's refresh also publishes
// for the parent and grandparent, and a parent-chain cycle terminates.
func TestRefreshDisplayStateWalksAncestors(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_gp", "", "", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_p", "c_gp", "c_gp", protocol.StatusIdle)
	insertDisplayChild(c.st, "c_gc", "c_p", "c_gp", protocol.StatusIdle)

	// The grandchild starts working: its own state changes, and each ancestor
	// gains a live descendant.
	c.st.SetStatus("c_gc", protocol.StatusStreaming)
	c.refreshDisplayState("c_gc")
	c.drainDisplayRefresh()

	gc, ok := displayLastOf(c, "c_gc")
	a.Require().True(ok, "the grandchild published")
	a.Eq(protocol.DisplayWorking, gc.state, "the grandchild is working")

	p, ok := displayLastOf(c, "c_p")
	a.Require().True(ok, "the parent published")
	a.Eq(protocol.DisplayWaiting, p.state, "the parent waits on its child")
	a.Eq(protocol.WaitingOnChildren, p.on, "waiting on children")

	gp, ok := displayLastOf(c, "c_gp")
	a.Require().True(ok, "the grandparent published")
	a.Eq(protocol.DisplayWaiting, gp.state, "the grandparent waits too")

	// A cycle in the parent chain must terminate, not loop.
	cyc := &Controller{
		st: childstore.New(), cm: newChildManager(),
		displayWake:    make(chan struct{}, 1),
		displayPending: map[string]struct{}{},
		displayLast:    map[string]displayValue{},
		displayDone:    make(chan struct{}),
	}
	insertDisplayChild(cyc.st, "c_a", "c_b", "c_a", protocol.StatusIdle)
	insertDisplayChild(cyc.st, "c_b", "c_a", "c_a", protocol.StatusIdle)
	done := make(chan struct{})
	go func() {
		cyc.publishDisplayState("c_a")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a parent-chain cycle did not terminate")
	}
}

// TestRefreshDisplayStateAbsentBaselineIsIdle: the first refresh of an
// already-idle child publishes nothing.
func TestRefreshDisplayStateAbsentBaselineIsIdle(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)

	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	c.refreshDisplayState("c_1")
	c.drainDisplayRefresh()
	a.Empty(operatorStates(ch), "an idle child with no waits publishes nothing")
}

// TestRefreshDisplayStateDoesNotBlockTheCaller: with publishEvent's durable
// append blocked, refreshDisplayState still returns immediately — the caller is
// never held up by the worker's I/O.
func TestRefreshDisplayStateDoesNotBlockTheCaller(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.PendingQuestion = "proceed?" })

	bl := &blockingEventLog{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(bl.release)
	c.evlog = bl

	c.refreshDisplayState("c_1")
	select {
	case <-bl.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never reached the blocked publish")
	}

	start := time.Now()
	c.refreshDisplayState("c_1")
	a.True(time.Since(start) < time.Second, "refreshDisplayState returned without waiting on the blocked publish")
}

// TestRefreshDisplayStateCoalesces: many refreshes of one child before the
// worker runs publish at most one event for an unchanged tuple.
func TestRefreshDisplayStateCoalesces(t *testing.T) {
	a := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.PendingQuestion = "proceed?" })

	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	for range 10 {
		c.refreshDisplayState("c_1")
	}
	c.drainDisplayRefresh()
	a.Len(operatorStates(ch), 1, "ten coalesced refreshes publish at most one event")
}

// ─── turn-start clear ─────────────────────────────────────────────────────────

// TestTurnStartClearsPendingQuestionAndLastTurnError: a new turn clears the
// pending question and the last turn's error; a manual /compact from idle
// (not a turn) keeps them.
func TestTurnStartClearsPendingQuestionAndLastTurnError(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{
		ChildID: "c_1", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		PendingQuestion: "proceed?", LastTurnError: "boom", StartedAt: time.Now(),
	})

	c.handleStatusChange("c_1", protocol.StatusStreaming, protocol.StatusIdle)
	snap, _ := c.st.Get("c_1")
	a.Eq("", snap.PendingQuestion, "a new turn clears the pending question")
	a.Eq("", snap.LastTurnError, "a new turn clears the last turn's error")

	// A manual /compact from idle is not a turn: the fields survive.
	c.st.SetStatus("c_1", protocol.StatusIdle)
	mustUpdate(t, c, "c_1", func(s *childstore.Session) {
		s.PendingQuestion = "which one?"
		s.LastTurnError = "still here"
	})
	c.handleStatusChange("c_1", protocol.StatusCompacting, protocol.StatusIdle)
	snap, _ = c.st.Get("c_1")
	a.Eq("which one?", snap.PendingQuestion, "a manual /compact keeps the pending question")
	a.Eq("still here", snap.LastTurnError, "a manual /compact keeps the last turn's error")
}

// TestTurnStartClearsNothingWithinTheDeclaringTurn: a question set while the
// turn is running survives the turn's own end (working -> idle is not a turn
// start).
func TestTurnStartClearsNothingWithinTheDeclaringTurn(t *testing.T) {
	a := assert.NewCollecting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager()}
	c.st.Insert(&childstore.Session{ChildID: "c_1", Status: protocol.StatusIdle, Kind: protocol.KindFundi, StartedAt: time.Now()})

	c.handleStatusChange("c_1", protocol.StatusStreaming, protocol.StatusIdle)
	mustUpdate(t, c, "c_1", func(s *childstore.Session) { s.PendingQuestion = "asked mid-turn" })

	c.handleStatusChange("c_1", protocol.StatusIdle, protocol.StatusStreaming)
	snap, _ := c.st.Get("c_1")
	a.Eq("asked mid-turn", snap.PendingQuestion, "a question set mid-turn survives the turn's end")
}
