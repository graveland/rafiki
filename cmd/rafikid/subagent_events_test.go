package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
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

func (c *capturedFlush) fn(childID, source string, fragments []string, _ eventbuf.Delivery) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, capturedBatch{childID, source, append([]string(nil), fragments...)})
}

func (c *capturedFlush) batches() []capturedBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedBatch(nil), c.got...)
}

// settleFixture: one coordinator with five workers, a manual clock so the
// debounce is deterministic, and a busy function that reports idle.
func settleFixture(t *testing.T) (*Controller, *eventbuf.FakeClock, *capturedFlush) {
	t.Helper()
	clk := eventbuf.NewFakeClock(time.Unix(0, 0))
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
	c, clk, cap := settleFixture(t)
	for _, id := range []string{"c_w1", "c_w2", "c_w3", "c_w4", "c_w5"} {
		c.handleStatusChange(id, protocol.StatusIdle, protocol.StatusStreaming)
	}
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	if len(batches) != 1 {
		t.Fatalf("want 1 coalesced batch, got %d: %+v", len(batches), batches)
	}
	if batches[0].childID != "c_coord" {
		t.Fatalf("batch went to %s, not the coordinator", batches[0].childID)
	}
	if batches[0].source != subagentEventSource {
		t.Fatalf("source %q", batches[0].source)
	}
	if len(batches[0].fragments) != 5 {
		t.Fatalf("want 5 fragments, got %d: %v", len(batches[0].fragments), batches[0].fragments)
	}
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
	if len(batches) != 1 || len(batches[0].fragments) != 1 {
		t.Fatalf("want 1 batch of 1 fragment, got %+v", batches)
	}
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
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a fresh spawn must not notify the parent: %+v", got)
	}
}

// A top-level agent has no parent; the push must be skipped, not sent to "".
func TestTopLevelSettleNotifiesNobody(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.handleStatusChange("c_coord", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("want no batch, got %+v", got)
	}
}

func TestSettleFragmentNamesTheAgentAndPointsAtTheLedger(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	frag := cap.batches()[0].fragments[0]
	if !strings.Contains(frag, "c_w1") {
		t.Errorf("fragment must name the agent: %q", frag)
	}
	// The buffer says something happened; the ledger says what it was. The
	// fragment must point at the ledger rather than trying to be one.
	if !strings.Contains(frag, "task_list") {
		t.Errorf("fragment must point at the ledger: %q", frag)
	}
}

func TestExitNotifiesTheParent(t *testing.T) {
	c, clk, cap := settleFixture(t)
	c.notifySubagentSettled("c_w1", "exited")
	clk.Advance(6 * time.Second)

	batches := cap.batches()
	if len(batches) != 1 || !strings.Contains(batches[0].fragments[0], "exited") {
		t.Fatalf("got %+v", batches)
	}
}

// The rule is checkable, so it is checked — not written into a prompt paid on
// every request forever.
func TestSettleWithResidueNudgesTheAgentItself(t *testing.T) {
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
	if _, err := store.Update(ctx, "conv-w1", []tasks.Change{{Handle: "1", Status: tasks.StatusCompleted}}); err != nil {
		t.Fatal(err)
	}

	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	var nudge string
	for _, b := range cap.batches() {
		if b.childID == "c_w1" {
			nudge = strings.Join(b.fragments, "\n")
		}
	}
	if nudge == "" {
		t.Fatal("the settling agent must be nudged about its own residue")
	}
	if !strings.Contains(nudge, "2") {
		t.Errorf("the nudge must name the unresolved handle; got %q", nudge)
	}
	if strings.Contains(nudge, "1 ") && strings.Contains(nudge, "done thing") {
		t.Errorf("a resolved task must not be listed; got %q", nudge)
	}
}

// Bound the loop. A model that ignored the first nudge is not more likely to
// honour the fifth, and each one costs a full turn.
func TestSecondSettleWithResidueEscalatesInsteadOfNudging(t *testing.T) {
	c, clk, cap := settleFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.SessionID = "conv-w1" })
	if _, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "never finished"}}); err != nil {
		t.Fatal(err)
	}

	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)
	c.st.SetStatus("c_w1", protocol.StatusStreaming)
	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	var toWorker, toCoord int
	var coordText string
	for _, b := range cap.batches() {
		switch b.childID {
		case "c_w1":
			toWorker++
		case "c_coord":
			toCoord++
			coordText += strings.Join(b.fragments, "\n")
		}
	}
	if toWorker != 1 {
		t.Errorf("want exactly one nudge to the worker, got %d", toWorker)
	}
	if toCoord == 0 || !strings.Contains(coordText, "unresolved") {
		t.Errorf("the second settle must escalate to the coordinator; got %q", coordText)
	}
}

func TestCleanSettleIsNotNudged(t *testing.T) {
	c, clk, cap := settleFixture(t)
	store := tasks.NewMemoryStore()
	c.tasks = store
	ctx := context.Background()

	_ = c.st.Update("c_w1", func(s *childstore.Session) { s.SessionID = "conv-w1" })
	if _, err := store.Add(ctx, "conv-w1", "", []tasks.NewTask{{Content: "done"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, "conv-w1", []tasks.Change{{Handle: "1", Status: tasks.StatusCompleted}}); err != nil {
		t.Fatal(err)
	}

	c.handleStatusChange("c_w1", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)

	for _, b := range cap.batches() {
		if b.childID == "c_w1" {
			t.Fatalf("a clean settle must not cost a turn: %+v", b)
		}
	}
}
