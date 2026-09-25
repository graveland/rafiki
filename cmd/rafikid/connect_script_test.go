// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// scriptHubFixture: one controller with an in-memory childstore, a capturing
// event buffer (the capturedFlush fixture from subagent_events_test.go), and
// a durable in-memory event log for the top-level-report path.
func scriptHubFixture(t *testing.T) (*Controller, *capturedFlush, *eventlog.Memory, *eventbuf.FakeClock) {
	t.Helper()
	clk := eventbuf.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cap := &capturedFlush{}
	buf.SetFlush(cap.fn)
	buf.SetBusy(func(string) bool { return false })
	elog := eventlog.NewMemory()
	c := &Controller{st: childstore.New(), cm: newChildManager(), evbuf: buf, evlog: elog}
	return c, cap, elog, clk
}

// scriptParented adds a parented script child to st: c_script under c_coord.
func scriptParented(c *Controller) {
	c.st.Insert(&childstore.Session{
		ChildID: "c_coord", Name: "coord", Status: protocol.StatusIdle, StartedAt: time.Now(),
	})
	c.st.Insert(&childstore.Session{
		ChildID: "c_script", Name: "worker", Status: protocol.StatusStreaming, StartedAt: time.Now(),
		Labels: map[string]string{
			childstore.LabelParent: "c_coord",
			childstore.LabelRoot:   "c_coord",
		},
	})
}

// TestScriptReportPushesToTheParentsBuffer pins 2.1's outward half: a
// parented script's Report lands in its PARENT's event buffer, keyed on the
// script's own id, under the "script" source — so it coalesces and defers
// exactly like a subagent settle.
func TestScriptReportPushesToTheParentsBuffer(t *testing.T) {
	c, cap, _, clk := scriptHubFixture(t)
	scriptParented(c)
	hub := c.connectScriptHub()

	if err := hub.Report(context.Background(), "c_script", "progress", `{"step":1}`); err != nil {
		t.Fatalf("Report: %v", err)
	}

	// The buffer debounces; the fake clock flushes it.
	clk.Advance(6 * time.Second)
	batches := cap.batches()
	if len(batches) != 1 {
		t.Fatalf("want 1 batch to the parent, got %d: %+v", len(batches), batches)
	}
	if batches[0].childID != "c_coord" || batches[0].source != scriptEventSource {
		t.Fatalf("batch = %+v, want parent c_coord / source %q", batches[0], scriptEventSource)
	}
	if len(batches[0].fragments) != 1 ||
		!strings.Contains(batches[0].fragments[0], "c_script") ||
		!strings.Contains(batches[0].fragments[0], "progress") ||
		!strings.Contains(batches[0].fragments[0], `{"step":1}`) {
		t.Fatalf("fragment = %+v", batches[0].fragments)
	}
}

// TestScriptReportTopLevelWritesItsOwnDurableLog pins 2.1's top-level half: a
// script with no parent appends its report to its OWN event log as a durable
// script_report event.
func TestScriptReportTopLevelWritesItsOwnDurableLog(t *testing.T) {
	c, cap, elog, clk := scriptHubFixture(t)
	c.st.Insert(&childstore.Session{
		ChildID: "c_top", Name: "top", Status: protocol.StatusStreaming, StartedAt: time.Now(),
	})
	hub := c.connectScriptHub()

	if err := hub.Report(context.Background(), "c_top", "error", `{"msg":"boom"}`); err != nil {
		t.Fatalf("Report: %v", err)
	}
	clk.Advance(6 * time.Second)
	if batches := cap.batches(); len(batches) != 0 {
		t.Fatalf("a top-level report must not reach any buffer: %+v", batches)
	}
	recs, err := elog.Read(context.Background(), "c_top", -1, 10)
	if err != nil {
		t.Fatalf("event log read: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 durable event, got %d", len(recs))
	}
	var ev rafikiv1.Event
	if err := protojson.Unmarshal(recs[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	report := ev.GetScriptReport()
	if report == nil || report.GetKind() != "error" || report.GetDataJson() != `{"msg":"boom"}` {
		t.Fatalf("payload = %+v", &ev)
	}
	if recs[0].Ordinal != 0 {
		t.Fatalf("durable report has ordinal %d, want 0", recs[0].Ordinal)
	}
}

// TestSetResultStoresLastWriteWins pins 2.3's storage half: the result lands
// on the calling child's session, the durable write is attempted, and a
// second call replaces the first.
func TestSetResultStoresLastWriteWins(t *testing.T) {
	c, _, _, _ := scriptHubFixture(t)
	scriptParented(c)
	hub := c.connectScriptHub()

	if err := hub.SetResult(context.Background(), "c_script", `{"answer":1}`); err != nil {
		t.Fatalf("first SetResult: %v", err)
	}
	if err := hub.SetResult(context.Background(), "c_script", `{"answer":2}`); err != nil {
		t.Fatalf("second SetResult: %v", err)
	}
	snap, ok := c.st.Get("c_script")
	if !ok {
		t.Fatal("child vanished")
	}
	if snap.Result != `{"answer":2}` {
		t.Fatalf("Result = %q, want the last write", snap.Result)
	}

	err := hub.SetResult(context.Background(), "c_unknown", `{}`)
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("SetResult(unknown) = %v, want %v", err, connect.CodeNotFound)
	}
}

// TestSettleFragmentCarriesResult pins the settle half of 2.3: a child with a
// stored result injects it verbatim into the parent's fragment; one without a
// result is unchanged.
func TestSettleFragmentCarriesResult(t *testing.T) {
	c, cap, _, clk := scriptHubFixture(t)
	scriptParented(c)
	if err := c.st.Update("c_script", func(s *childstore.Session) {
		s.Result = `{"answer":42}`
	}); err != nil {
		t.Fatalf("store result: %v", err)
	}

	c.handleStatusChange("c_script", protocol.StatusIdle, protocol.StatusStreaming)
	clk.Advance(6 * time.Second)
	batches := cap.batches()
	if len(batches) != 1 || len(batches[0].fragments) != 1 {
		t.Fatalf("want 1 batch of 1 fragment, got %+v", batches)
	}
	frag := batches[0].fragments[0]
	if !strings.Contains(frag, "final result of c_script") || !strings.Contains(frag, `{"answer":42}`) {
		t.Fatalf("fragment = %q", frag)
	}
}

// TestScriptReceiveDrainsThePulledBatch pins the F1 fix: Queue.Pull consumes
// EVERY pending row at once, so the stream must retain and deliver the whole
// batch — every text row ahead of an abort, then the abort's stop, and
// nothing behind it (a stop was requested). Driven through Recv, the way the
// Connect handler drives it, so the done latch engages on the abort stop.
func TestScriptReceiveDrainsThePulledBatch(t *testing.T) {
	c, _, _, _ := scriptHubFixture(t)
	scriptParented(c)
	mem := inbox.NewMemory()
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: mem})
	oldInterval := scriptReceiveInterval
	scriptReceiveInterval = time.Millisecond
	t.Cleanup(func() { scriptReceiveInterval = oldInterval })

	// Four rows pend before the first poll: two texts ahead of an abort, one
	// behind it. This is the shape the first version of pollOnce silently
	// destroyed rows 2..N of — the backlog drain and the two-sends-in-one-tick
	// window.
	accept := func(mode inbox.Mode, text string) {
		t.Helper()
		if _, err := c.inbox.Accept(context.Background(), inbox.Inbound{
			ChildID: "c_script", Mode: mode, Text: text,
		}); err != nil {
			t.Fatalf("accept %q: %v", text, err)
		}
	}
	accept(inbox.ModePrompt, "row-a")
	accept(inbox.ModeSteer, "row-b")
	accept(inbox.ModeAbort, "")
	accept(inbox.ModePrompt, "row-c")

	hub := c.connectScriptHub()
	stream, err := hub.Receive(context.Background(), "c_script")
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	ctx := context.Background()

	// One pull consumed all four rows — none stays pending after the first
	// delivery, which is what makes retention on the stream load-bearing.
	m, err := stream.Recv(ctx)
	if err != nil || m.GetText().GetText() != "row-a" ||
		m.GetText().GetMode() != rafikiv1.SendMode_SEND_MODE_PROMPT {
		t.Fatalf("first delivery = %+v err=%v", m, err)
	}
	pending, err := mem.Pending(ctx, "c_script")
	if err != nil {
		t.Fatalf("pending read: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pull consumed %d rows, want 0 pending", len(pending))
	}

	// The rest of the retained batch: row-b (steer), then the abort's stop.
	m, err = stream.Recv(ctx)
	if err != nil || m.GetText().GetText() != "row-b" ||
		m.GetText().GetMode() != rafikiv1.SendMode_SEND_MODE_STEER {
		t.Fatalf("second delivery = %+v err=%v", m, err)
	}
	m, err = stream.Recv(ctx)
	if err != nil || m.GetStop().GetReason() != "abort requested" {
		t.Fatalf("abort delivery = %+v err=%v", m, err)
	}

	// The abort ends the stream: row-c — accepted after the stop was
	// requested — is discarded, not delivered, and the stream stays ended.
	if _, err := stream.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after the abort stop = %v, want io.EOF", err)
	}
}

// TestScriptReceiveStreamsInboxAndStops pins 2.2: pending inbox rows stream
// as text, an abort row arrives as the stop variant, and the child's exit
// ends the stream — each pulled row consumed exactly once.
func TestScriptReceiveStreamsInboxAndStops(t *testing.T) {
	c, _, _, _ := scriptHubFixture(t)
	scriptParented(c)
	mem := inbox.NewMemory()
	c.inbox = inbox.NewQueue(inbox.QueueConfig{Store: mem})
	oldInterval := scriptReceiveInterval
	scriptReceiveInterval = time.Millisecond
	t.Cleanup(func() { scriptReceiveInterval = oldInterval })

	hub := c.connectScriptHub()
	stream, err := hub.Receive(context.Background(), "c_script")
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	// Nothing yet: the poll fires on the interval, so drive one poll with a
	// short-deadline context and expect it to time out, not deliver.
	pollCtx, cancel := context.WithTimeout(context.Background(), 5*scriptReceiveInterval)
	cancel()
	if m, _, err := stream.(*scriptStream).pollOnce(pollCtx); err != nil || m != nil {
		t.Fatalf("empty inbox produced %+v / %v, want nothing", m, err)
	}

	// One prompt row, one abort row.
	if _, err := c.inbox.Accept(context.Background(), inbox.Inbound{
		ChildID: "c_script", Mode: inbox.ModePrompt, Text: "do the work",
	}); err != nil {
		t.Fatalf("accept prompt: %v", err)
	}
	m, stop, err := stream.(*scriptStream).pollOnce(context.Background())
	if err != nil || stop || m == nil || m.GetText().GetText() != "do the work" ||
		m.GetText().GetMode() != rafikiv1.SendMode_SEND_MODE_PROMPT ||
		len(m.GetText().GetMessageIds()) != 1 {
		t.Fatalf("prompt delivery = %+v stop=%v err=%v", m, stop, err)
	}

	if _, err := c.inbox.Accept(context.Background(), inbox.Inbound{
		ChildID: "c_script", Mode: inbox.ModeSteer, Text: "faster",
	}); err != nil {
		t.Fatalf("accept steer: %v", err)
	}
	m, stop, err = stream.(*scriptStream).pollOnce(context.Background())
	if err != nil || stop || m.GetText().GetMode() != rafikiv1.SendMode_SEND_MODE_STEER {
		t.Fatalf("steer delivery = %+v stop=%v err=%v", m, stop, err)
	}

	if _, err := c.inbox.Accept(context.Background(), inbox.Inbound{
		ChildID: "c_script", Mode: inbox.ModeAbort,
	}); err != nil {
		t.Fatalf("accept abort: %v", err)
	}
	m, stop, err = stream.(*scriptStream).pollOnce(context.Background())
	if err != nil || !stop || m.GetStop().GetReason() != "abort requested" {
		t.Fatalf("abort delivery = %+v stop=%v err=%v", m, stop, err)
	}

	// Every pulled row is consumed: the store holds nothing pending.
	rows, err := mem.Pending(context.Background(), "c_script")
	if err != nil {
		t.Fatalf("pending read: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("pulled rows are still pending: %+v", rows)
	}

	// The child exits: the stream stops and stays stopped. The terminal stop
	// goes through Recv (as the handler drives it), so the done latch engages.
	_, _ = c.st.SetStatus("c_script", protocol.StatusExited)
	m, err = stream.Recv(context.Background())
	if err != nil || m.GetStop().GetReason() != "child exited" {
		t.Fatalf("exit delivery = %+v err=%v", m, err)
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after terminal stop = %v, want io.EOF", err)
	}
}
