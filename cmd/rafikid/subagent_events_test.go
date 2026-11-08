package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

type capturedFlush struct {
	mu  sync.Mutex
	got []capturedBatch
}

type capturedBatch struct {
	childID   string
	source    string
	fragments []string
}

// fn stands in for Controller.flushInboxSource. The fixture attaches no
// Accepter, so every push arrives as an orphan; running them through
// inbox.Coalesce is exactly what deliverOrphans does, which is what makes
// these assertions about the fragments the child would actually see.
func (c *capturedFlush) fn(childID, source string, orphans []inbox.Inbound) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range inbox.Coalesce(orphans, inbox.BatchConfig{}) {
		c.got = append(c.got, capturedBatch{childID, source, b.Frags})
	}
}

func (c *capturedFlush) batches() []capturedBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedBatch(nil), c.got...)
}

// settleFixture: one coordinator with five workers, a manual clock so the
// debounce is deterministic, and a busy function that reports idle.
func settleFixture(t *testing.T) (*Controller, *clock.FakeClock, *capturedFlush) {
	t.Helper()
	clk := clock.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cap := &capturedFlush{}
	buf.SetFlush(cap.fn)
	buf.SetBusy(func(string) bool { return false })

	c := &Controller{st: childstore.New(), cm: newChildManager(), evbuf: buf}
	c.st.Insert(&childstore.Session{ChildID: "c_coord", Status: protocol.StatusIdle, StartedAt: time.Now()})
	for _, id := range []string{"c_w1", "c_w2", "c_w3", "c_w4", "c_w5"} {
		c.st.Insert(&childstore.Session{
			ChildID: id, Name: id, Status: protocol.StatusStreaming, StartedAt: time.Now(),
			Labels: map[string]string{
				childstore.LabelParent: "c_coord",
				childstore.LabelRoot:   "c_coord",
			},
		})
	}
	return c, clk, cap
}

// The phase's whole reason for depending on 03: five workers settling together
// must cost the coordinator ONE turn, not five.
func TestFiveWorkersSettleAsOneBatch(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	for _, id := range []string{"c_w1", "c_w2", "c_w3", "c_w4", "c_w5"} {
		c.handleStatusChange(id, protocol.StatusIdle, protocol.StatusStreaming)
	}
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	ck.Len(batches, 1, "want 1 coalesced batch, got %d", len(batches))
	ck.Eq("c_coord", batches[0].childID, "batch went to")
	ck.Eq(subagentEventSource, batches[0].source, "source")
	ck.Len(batches[0].fragments, 5, "want 5 fragments, got %d", len(batches[0].fragments))
}

// Keyed on the child: a worker that settles three times contributes ONE
// fragment, the latest.
func TestRepeatedSettlesFromOneWorkerCoalesce(t *testing.T) {
	c, clk, cap := settleFixture(t)
	for range 3 {
		c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
		c.st.SetStatus("c_w1", protocol.StatusStreaming)
	}
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	assert.NewAborting(t).False(len(batches) != 1 || len(batches[0].fragments) != 1, "want 1 batch of 1 fragment, got %+v", batches)
}

// TestIsWorkingStatusBatchWait pins batch_wait's membership in the working
// set: a child parked on a provider Batch API is mid-turn, so the idle
// transition after delivery must still fire the settle notification and
// parent heartbeats keep reporting elapsed time.
func TestIsWorkingStatusBatchWait(t *testing.T) {
	assert.NewCollecting(t).True(isWorkingStatus(protocol.StatusBatchWait), "isWorkingStatus(batch_wait) = false, want true")
}

// spawning -> idle is not a settle. Without this guard every spawn immediately
// wakes the parent to announce that the child it just created exists.
func TestSpawningToIdleIsNotASettle(t *testing.T) {
	c, clk, cap := settleFixture(t)
	// Set the worker's store status to spawning so the transition is
	// genuinely spawning->idle, not the fixture's default streaming->idle.
	c.st.SetStatus("c_w1", protocol.StatusSpawning)
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusSpawning)
	clk.Advance(6 * time.Second)
	assert.NewAborting(t).Empty(cap.batches(), "a fresh spawn must not notify the parent")
}

// A top-level agent has no parent; the push must be skipped, not sent to "".
func TestTopLevelSettleNotifiesNobody(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.handleStatusChange("c_coord", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)
	assert.NewAborting(t).Empty(cap.batches(), "want no batch, got")
}

func TestSettleFragmentNamesTheAgentAndPointsAtTheLedger(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := settleFixture(t)
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	ck.StrContains(frag, "c_w1", "fragment must name the agent")
	// The buffer says something happened; the ledger says what it was. The
	// fragment must point at the ledger rather than trying to be one.
	ck.StrContains(frag, "task_list", "fragment must point at the ledger")
}

func TestExitNotifiesTheParent(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.notifySubagentSettled("c_w1", "exited", "", "")
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	assert.NewAborting(t).False(len(batches) != 1 || !strings.Contains(batches[0].fragments[0], "exited"), "got %+v", batches)
}

// The rule is checkable, so it is checked — not written into a prompt paid on
// every request forever.
func TestSettleWithResidueNudgesTheAgentItself(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, clk, cap := settleFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.SessionID = "conv-w1" })
	if _, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{
		{Content: "done thing"}, {Content: "half-done thing"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update(ctx, "conv-w1", []tasks.Change{{Handle: "1", Status: tasks.StatusCompleted}})
	ck.Require().NoError(err)

	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	var nudge string
	for _, b := range cap.batches() {
		if b.childID == "c_w1" {
			nudge = strings.Join(b.fragments, "\n")
		}
	}
	ck.Require().NotEq("", nudge, "the settling agent must be nudged about its own residue")
	ck.StrContains(nudge, "2", "the nudge must name the unresolved handle; got")
	ck.False(strings.Contains(nudge, "1 ") && strings.Contains(nudge, "done thing"), "a resolved task must not be listed; got %q", nudge)
}

// Bound the loop. A model that ignored the first nudge is not more likely to
// honour the fifth, and each one costs a full turn.
//
// And it stops there: residue never reaches the spawner, under EITHER notify
// policy. A pending row is not news — it blocks nothing, fails nothing and
// retargets nothing — and the ledger is one pull away (`task_list(assignee=…)`,
// `rafiki tasks`). Waking a spawner to describe state it cannot act on is the
// cost a quiet spawner exists to avoid, and a coordinator driving a plan settles
// with residue on EVERY turn, so an escalation would be a flood on the default
// policy and a wasted turn even once on any policy.
func TestResidueNeverWakesTheSpawner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet bool
	}{
		{name: "default policy", quiet: false},
		{name: "notify=result", quiet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			c, clk, cap := settleFixture(t)
			store := tasks.NewMemoryStore()
			c.tasks = store
			ctx := context.Background()

			ck.Require().NoError(c.st.Update("c_w1", func(s *childstore.Session) {
				s.SessionID = "conv-w1"
				if tc.quiet {
					s.NotifyPolicy = protocol.NotifyResult
				}
			}))
			_, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "never finished"}})
			ck.Require().NoError(err)

			settle := func() {
				c.st.SetStatus("c_w1", protocol.StatusStreaming)
				c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
				clk.Advance(6 * time.Second)
			}

			settle() // nudge to the child
			settle() // second settle: the old ladder escalated from here
			_, err = store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "and another"}})
			ck.Require().NoError(err)
			settle() // changed residue set: still nobody above the child is told

			var toWorker int
			for _, b := range cap.batches() {
				if b.childID == "c_w1" {
					toWorker++
				}
			}
			ck.Eq(1, toWorker, "the child is nudged about its own residue exactly once")
			ck.Empty(coordResidue(cap), "residue must never be injected into the spawner")
		})
	}
}

// coordResidue collects any fragment about unresolved tasks among the
// coordinator's batches. Every settle also pushes its own settle fragment, which
// is not residue and must not be mistaken for one.
func coordResidue(cap *capturedFlush) []string {
	var out []string
	for _, b := range coordBatches(cap) {
		for _, f := range b.fragments {
			if strings.Contains(f, "unresolved") {
				out = append(out, f)
			}
		}
	}
	return out
}

func TestCleanSettleIsNotNudged(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.SessionID = "conv-w1" })
	if _, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "done"}}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update(ctx, "conv-w1", []tasks.Change{{Handle: "1", Status: tasks.StatusCompleted}})
	ck.NoError(err)

	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	for _, b := range cap.batches() {
		ck.NotEq("c_w1", b.childID, "a clean settle must not cost a turn: %+v", b)
	}
}

// TestSettleFragmentDecodesStringResult pins the settle side of payload
// rendering: a stored result that is a JSON string is shown decoded — the
// parent reads two lines, not an escaped JSON literal.
func TestSettleFragmentDecodesStringResult(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cap := settleFixture(t)
	_ = c.st.Update("c_w1", func(s *childstore.Session) {
		s.Result = "\"DONE\\nF1 x.go:40\""
	})
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	ck.StrContains(frag, "final result of c_w1: DONE\nF1 x.go:40",
		"a JSON-string result must render decoded, as two lines; got %q", frag)
}

func TestSettleFragmentNamesTheGuardrailReasonWhenPresent(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.turnOutcomes.set("c_w1", fundi.TurnOutcome{LimitReason: "agent's own cost budget ($5.00 of $5.00) exhausted"})
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	assert.NewCollecting(t).StrContains(frag, "cost budget", "fragment must name the guardrail reason")
}

func TestSettleFragmentNamesTheErrorWhenTurnFailed(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.turnOutcomes.set("c_w1", fundi.TurnOutcome{Err: errors.New("upstream unavailable")})
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	assert.NewCollecting(t).StrContains(frag, "upstream unavailable", "fragment must name the real error")
}

func TestSettleFragmentFallsBackToGenericMessageWhenClean(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.turnOutcomes.set("c_w1", fundi.TurnOutcome{Clean: true})
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	assert.NewCollecting(t).StrContains(frag, "settled (idle)", "a clean outcome must still read as settled (idle)")
}

func TestSettleFragmentFallsBackWhenNoOutcomeStored(t *testing.T) {
	c, clk, cap := settleFixture(t)
	// No turnOutcomes.set call: the old behavior, e.g. a claude-kind child
	// with no fundi Engine behind it to report anything.
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	assert.NewCollecting(t).StrContains(frag, "settled (idle)", "no stored outcome must still read as settled (idle)")
}
