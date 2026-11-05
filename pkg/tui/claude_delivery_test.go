// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/charmbracelet/x/ansi"
	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui/session"
)

// ── Claude delivery acknowledgement ─────────────────────────────────────────
//
// A claude send is acknowledged by a durable message_delivered receipt naming
// the inbox row id SendResponse returned — never by matching the captured text.
// The row moves pending (grey ghost) → sent (a normal user row) on the receipt,
// and disappears from the overlay only when a capture that arrived AFTER the
// send proves the transcript already holds it.
//
// Everything here drives the real Update path: the Send RPC against an HTTP
// server, the stream channels' message types, and the cockpit's own tick. The
// ledger is exercised directly only where the subject IS the ledger (the
// bounded early-receipt buffer and its clock).

// fakeSend is the control server a delivery test talks to: Send answers with
// the next inbox row id, or fails.
type fakeSend struct {
	rafikiv1connect.UnimplementedControlHandler
	mu   sync.Mutex
	ids  []string
	fail bool
	next int
	got  []string
}

func (f *fakeSend) Send(_ context.Context, req *connect.Request[rafikiv1.SendRequest]) (*connect.Response[rafikiv1.SendResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, connect.NewError(connect.CodeInternal, errors.New("send refused"))
	}
	f.got = append(f.got, req.Msg.GetChildId())
	id := ""
	if f.next < len(f.ids) {
		id = f.ids[f.next]
	}
	f.next++
	return connect.NewResponse(&rafikiv1.SendResponse{MessageId: id}), nil
}

// claudeCockpit is a cockpit on one claude child, with the second child present
// so a focus move has somewhere to go.
func claudeCockpit(t *testing.T, f *fakeSend) *Cockpit {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(rafikiv1connect.NewControlHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewCockpit(Options{BaseURL: srv.URL, ChildID: "c_1"})
	t.Cleanup(c.shutdown)
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{
		claudeSummary("c_1", "one", 0), claudeSummary("c_2", "two", 0),
	}})
	// A session with a cursor, so nothing here opens a stream: the tests feed
	// the feed's own message types instead.
	c.focusChild = "c_1"
	c.sessions["c_1"].Cursor, c.sessions["c_1"].HasCursor = -1, true
	return c
}

func claudeSummary(id, name string, latest int32) *rafikiv1.ChildSummary {
	s := summaryFor(id, name, latest)
	s.Kind = protocol.KindClaude
	return s
}

// deliveredFor is a durable acceptance receipt naming an inbox row.
func deliveredFor(child, id string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: child, Ordinal: &ord,
		Payload: &rafikiv1.Event_MessageDelivered{
			MessageDelivered: &rafikiv1.MessageDelivered{MessageId: id}}}
}

// capturedFor is a stored user row as the focus stream delivers it: a
// timestamp (the stored node's created_at) and the message's content.
func capturedFor(child, text string, ord int32, at time.Time) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: child, Ordinal: &ord, Ts: timestamppb.New(at),
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}}}
}

// sendNow issues a send through the real RPC and applies whatever the command
// returned, exactly as Update would.
func sendNow(t *testing.T, c *Cockpit, child, text string, images ...stagedAttachment) {
	t.Helper()
	for _, m := range runCmdDeep(t, c.sendTo(child, rafikiv1.SendMode_SEND_MODE_PROMPT, text, images)) {
		if m != nil {
			c.Update(m)
		}
	}
}

// feedFocus delivers events the way the focus stream does.
func feedFocus(c *Cockpit, evs ...*rafikiv1.Event) { c.Update(eventMsg{evs: evs}) }

// feedRail delivers events the way the rail stream does.
func feedRail(c *Cockpit, evs ...*rafikiv1.Event) { c.Update(railEventMsg{evs: evs}) }

// sends is child's optimistic rows.
func sends(c *Cockpit, child string) []optimisticSend {
	if st := c.optimistic.get(child); st != nil {
		return st.sends
	}
	return nil
}

func TestClaudeDeliveryReceiptAfterTheResponseMarksTheRowSent(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 1, "the send must be recorded before its RPC answers")
	ck.False(sends(c, "c_1")[0].sent, "nothing acknowledged it yet")
	ck.Eq("row-1", sends(c, "c_1")[0].inboxID, "the inbox id from SendResponse")

	feedFocus(c, deliveredFor("c_1", "row-1", 1))

	ck.Require().Len(sends(c, "c_1"), 1, "a receipt must not retire the row: the capture has not landed")
	ck.True(sends(c, "c_1")[0].sent, "the matching delivery id must move the row to sent")
	ck.Eq("hello", sends(c, "c_1")[0].text, "the row keeps its original text")
}

// The receipt can beat its own RPC: the daemon publishes it from the child's
// echo, which for an idle child lands before the reply. The row must still be
// advanced — and must stay PENDING until then, not be guessed at.
func TestClaudeDeliveryReceiptBeforeTheResponseIsJoined(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	// Issue the RPC but hold its message back.
	cmd := c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "hello", nil)
	ck.Require().Len(sends(c, "c_1"), 1, "the row exists before the RPC is even run")

	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.False(sends(c, "c_1")[0].sent, "an id no send claims yet must not acknowledge anything")
	ck.Len(c.optimistic.get("c_1").early, 1, "the receipt must be buffered while an RPC is outstanding")

	for _, m := range runCmdDeep(t, cmd) {
		if m != nil {
			c.Update(m)
		}
	}
	ck.True(sends(c, "c_1")[0].sent, "the response must join the receipt that beat it")
	ck.Len(c.optimistic.get("c_1").early, 0, "the buffer is cleared once the RPC settles")
}

// Two identical sends, answered in reverse order: the receipt names an id, not
// a position, so each row is advanced by its own.
func TestClaudeDeliveryReversedResponsesForIdenticalSends(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	first := c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "continue", nil)
	second := c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "continue", nil)

	// The SECOND response arrives first, and so takes the first id.
	for _, m := range runCmdDeep(t, second) {
		if m != nil {
			c.Update(m)
		}
	}
	for _, m := range runCmdDeep(t, first) {
		if m != nil {
			c.Update(m)
		}
	}
	secondID, firstID := sends(c, "c_1")[1].inboxID, sends(c, "c_1")[0].inboxID
	ck.Require().NotEq(firstID, secondID, "two sends get two ids")

	feedFocus(c, deliveredFor("c_1", secondID, 1))
	ck.True(sends(c, "c_1")[1].sent, "the receipt names the SECOND attempt, whichever id it took")
	ck.False(sends(c, "c_1")[0].sent, "the first attempt has not been acknowledged")

	feedFocus(c, deliveredFor("c_1", firstID, 2))
	ck.True(sends(c, "c_1")[0].sent, "ids are joined to attempts, not to positions")
}

// The early buffer is a reordering tolerance with hard bounds: an id past its
// TTL, or past the cap, is dropped rather than used to guess.
func TestClaudeDeliveryEarlyBufferExpiresAndEvicts(t *testing.T) {
	ck := assert.NewCollecting(t)
	clk := &fakeClock{at: time.Unix(1_700_000_000, 0)}
	l := newOptimisticLedger(clk.now)

	attempt := l.add("c_1", ackReceipt, "hello", nil)
	l.deliver("c_1", "row-1")
	ck.Require().Len(l.get("c_1").early, 1, "buffered")

	clk.advance(earlyReceiptTTL + time.Second)
	l.expire()
	ck.Len(l.get("c_1").early, 0, "a receipt past its TTL must be dropped")

	l.settle("c_1", attempt, "row-1")
	ck.False(l.get("c_1").sends[0].sent, "an expired receipt must never be joined")

	// Overflow: the oldest goes, because a receipt is only useful for a row
	// whose response is still outstanding.
	l.add("c_1", ackReceipt, "again", nil)
	for i := 0; i <= earlyReceiptCap; i++ {
		l.deliver("c_1", "id-"+itoa(int64(i)))
	}
	st := l.get("c_1")
	ck.Require().Len(st.early, earlyReceiptCap, "the buffer is capped")
	ck.Eq("id-1", st.early[0].id, "the oldest entry is the one evicted")
	ck.Eq("id-"+itoa(int64(earlyReceiptCap)), st.early[len(st.early)-1].id, "the newest survives")
}

// A receipt for a child whose RPCs have all settled is unusable state, and is
// not retained: nothing will ever claim it.
func TestClaudeDeliveryUnclaimedReceiptsAreNotRetained(t *testing.T) {
	ck := assert.NewCollecting(t)
	l := newOptimisticLedger(time.Now)

	attempt := l.add("c_1", ackReceipt, "hello", nil)
	l.settle("c_1", attempt, "row-1")
	l.deliver("c_1", "somebody-elses-row")

	ck.Len(l.get("c_1").early, 0, "no RPC is outstanding, so nothing can be buffered")
	ck.False(l.get("c_1").sends[0].sent, "a foreign id must not acknowledge this send")
}

// Pending is a grey ghost; sent is the transcript's own user decoration. The
// message is visible in both, and after the capture it is visible once, as the
// real row — the row never disappears between states.
func TestClaudeDeliveryPendingGreyThenSentNormalWithNoGap(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	width := c.convWidth()

	sendNow(t, c, "c_1", "hello there")
	ck.StrContains(c.View().Content, styleGhost.Width(width).Render("▌ hello there"),
		"a pending row must draw as the grey ghost")
	ck.NotStrContains(c.View().Content, styleUser.Render("▌ hello there"),
		"a pending row must not draw as a real user row")

	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.StrContains(c.View().Content, styleUser.Render("▌ hello there"),
		"a sent row must draw with the transcript's own user decoration")
	ck.NotStrContains(c.View().Content, styleGhost.Width(width).Render("▌ hello there"),
		"the ghost must not survive the acknowledgement")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello there"), "the message must be visible exactly once")

	feedFocus(c, capturedFor("c_1", "hello there", 2, time.Unix(100, 0)))
	ck.Len(sends(c, "c_1"), 0, "the captured row replaces the provisional one")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello there"), "no duplicate, and no gap")
}

// The overlay sits INSIDE the transcript pane, so the working spinner keeps
// drawing below it in both states.
func TestClaudeDeliverySentRowStaysAboveTheWorkingSpinner(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	c.rail.Apply(statusEventFor("c_1", "streaming", 0))

	sendNow(t, c, "c_1", "hello there")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))

	got := ansi.Strip(c.View().Content)
	ck.StrContains(got, "streaming…", "the spinner is missing")
	ck.True(strings.Index(got, "hello there") < strings.Index(got, "streaming…"),
		"a sent row must stay above the spinner")
}

// The captured text is the harness's wrapper around the user's words, so
// matching is by whitespace-normalized SUBSTRING, never by equality.
func TestClaudeDeliveryMatchesThroughTheHarnessWrapper(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello there")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))

	captured := "The user sent a new message while you were working:\n" +
		"  hello   there\n\nThis is how Claude Code surfaces messages the user sends mid-turn."
	feedFocus(c, capturedFor("c_1", captured, 2, time.Unix(100, 0)))

	ck.Len(sends(c, "c_1"), 0, "a wrapped capture must still retire the sent row")
}

// Two sends merged into one user message: each occurrence answers one row, and
// nothing is left standing.
func TestClaudeDeliveryMergedCaptureRetiresBothSends(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	sendNow(t, c, "c_1", "first")
	sendNow(t, c, "c_1", "second")
	feedFocus(c, deliveredFor("c_1", "row-1", 1), deliveredFor("c_1", "row-2", 2))

	feedFocus(c, capturedFor("c_1", "first\n\nsecond", 3, time.Unix(100, 0)))
	ck.Len(sends(c, "c_1"), 0, "one merged capture answers both sends")
}

// One occurrence retires one row — including when a later acknowledgement
// arrives, which must not spend an occurrence a previous row already took.
func TestClaudeDeliveryRepeatedTextRetiresOneRowPerOccurrence(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2", "row-3"}})

	for i := 0; i < 3; i++ {
		sendNow(t, c, "c_1", "continue")
	}
	feedFocus(c, deliveredFor("c_1", "row-1", 1), deliveredFor("c_1", "row-2", 2))
	ck.Require().Len(sends(c, "c_1"), 3, "nothing is retired before a capture arrives")

	feedFocus(c, capturedFor("c_1", "continue", 3, time.Unix(100, 0)))
	ck.Require().Len(sends(c, "c_1"), 2, "one occurrence retires exactly one row")
	ck.False(sends(c, "c_1")[1].sent, "the row that was not acknowledged must be untouched")
	ck.Eq("row-3", sends(c, "c_1")[1].inboxID, "row-3 is the one still pending")

	// The third acknowledgement arrives AFTER the first occurrence was spent:
	// it must not re-spend it.
	feedFocus(c, deliveredFor("c_1", "row-3", 4))
	ck.Require().Len(sends(c, "c_1"), 2, "a later acknowledgement must not retire on spent evidence")

	feedFocus(c, capturedFor("c_1", "continue", 5, time.Unix(101, 0)))
	ck.Require().Len(sends(c, "c_1"), 1, "the second capture retires the second sent row")

	feedFocus(c, capturedFor("c_1", "continue", 6, time.Unix(102, 0)))
	ck.Len(sends(c, "c_1"), 0, "the third capture retires the last row")
}

// A replayed event and a history copy of it are ONE piece of evidence: a
// reconnect must not let the same capture retire a second row.
func TestClaudeDeliveryReplayAndHistoryCopiesSpendAnOccurrenceOnce(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	sendNow(t, c, "c_1", "continue")
	sendNow(t, c, "c_1", "continue")
	feedFocus(c, deliveredFor("c_1", "row-1", 1), deliveredFor("c_1", "row-2", 2))

	capture := capturedFor("c_1", "continue", 3, time.Unix(100, 0))
	feedFocus(c, capture)
	ck.Require().Len(sends(c, "c_1"), 1, "the first delivery retires one row")

	// The same event again, as a stream replay after a reconnect.
	feedFocus(c, capture)
	ck.Len(sends(c, "c_1"), 1, "a replayed capture must not spend a second occurrence")

	// And as a history copy of the same stored message.
	c.Update(historyMsg{childID: "c_1", after: 3, events: []*rafikiv1.Event{capture}})
	ck.Len(sends(c, "c_1"), 1, "a history copy of a known capture is not new evidence")
}

// A message the transcript already held before the send is not this send's
// capture, however identical its text.
func TestClaudeDeliveryOldIdenticalHistoryDoesNotRetire(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	old := capturedFor("c_1", "hello", 1, time.Unix(50, 0))
	c.Update(historyMsg{childID: "c_1", after: 1, events: []*rafikiv1.Event{old}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 2))
	ck.Require().Len(sends(c, "c_1"), 1, "the send is pending")

	// A replay of the OLD message, which the baseline already knows.
	feedFocus(c, capturedFor("c_1", "hello", 1, time.Unix(50, 0)))
	ck.Len(sends(c, "c_1"), 1, "a pre-existing identical message must not retire a fresh send")
}

// A first history fetch that lands AFTER the send cannot establish novelty, so
// it is baseline-only — while a capture that really arrives on the stream
// still retires the row (a brand-new child has no history at all).
func TestClaudeDeliveryFirstFetchUncertaintyRetainsTheRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))

	c.Update(historyMsg{childID: "c_1", after: 2, events: []*rafikiv1.Event{
		capturedFor("c_1", "hello", 2, time.Unix(100, 0)),
	}})
	ck.Len(sends(c, "c_1"), 1, "a fetch that was not established before the send is baseline-only")

	// A capture that arrives on the stream, with a timestamp the fetch did not
	// carry, IS new evidence — and a child with no history at all works too.
	feedFocus(c, capturedFor("c_1", "hello", 3, time.Unix(101, 0)))
	ck.Len(sends(c, "c_1"), 0, "a genuinely new capture retires the row")
}

// The capture can arrive before the receipt: it is kept, changes nothing while
// the row is pending, and is used the moment the acknowledgement lands.
func TestClaudeDeliveryCaptureBeforeEchoIsKeptAndUsed(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, capturedFor("c_1", "hello", 2, time.Unix(100, 0)))
	ck.Require().Len(sends(c, "c_1"), 1, "a capture cannot discharge a claim about custody")
	ck.False(sends(c, "c_1")[0].sent, "the row is still pending")

	feedFocus(c, deliveredFor("c_1", "row-1", 3))
	ck.Len(sends(c, "c_1"), 0, "the receipt must reconcile against the retained evidence")
}

// An unrelated capture — and one that arrived BEFORE the send — retires
// nothing. Only evidence ingested after the send is eligible.
func TestClaudeDeliveryEarlierUnrelatedCaptureRetiresNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, capturedFor("c_1", "something else entirely", 2, time.Unix(100, 0)))
	feedFocus(c, deliveredFor("c_1", "row-1", 3))

	ck.Require().Len(sends(c, "c_1"), 1, "an unrelated capture must not retire the row")
	ck.True(sends(c, "c_1")[0].sent, "the receipt still advanced it")

	// A second send, with evidence already in hand from before it: the earlier
	// evidence is not eligible for it.
	feedFocus(c, capturedFor("c_1", "hello", 4, time.Unix(101, 0)))
	ck.Len(sends(c, "c_1"), 0, "the row's own capture retires it")
}

// Evidence the ledger already held when a send was issued is not that send's
// capture: the generation snapshot taken at send time is what says so.
func TestClaudeDeliveryEvidenceFromBeforeASendIsNotEligible(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	sendNow(t, c, "c_1", "hello")
	// The first send's capture arrives while that send is still pending, so it
	// is retained as evidence for the next acknowledgement — and is already in
	// the ledger when the SECOND send is issued.
	feedFocus(c, capturedFor("c_1", "hello", 1, time.Unix(100, 0)))
	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 2, "neither row was retired by a capture")

	feedFocus(c, deliveredFor("c_1", "row-2", 2))
	ck.Require().Len(sends(c, "c_1"), 2, "evidence older than the send cannot retire it")
	ck.True(sends(c, "c_1")[1].sent, "the receipt still advanced it")

	// The first send's own receipt may use that evidence: it predates the send.
	feedFocus(c, deliveredFor("c_1", "row-1", 3))
	ck.Require().Len(sends(c, "c_1"), 1, "the earlier send takes the earlier evidence")
	ck.Eq("row-2", sends(c, "c_1")[0].inboxID, "the newer send is the one left standing")

	// A capture that really is later does retire it.
	feedFocus(c, capturedFor("c_1", "hello", 4, time.Unix(101, 0)))
	ck.Len(sends(c, "c_1"), 0, "a later capture retires the row")
}

// An attachment-bearing send is never matched by text: retiring the row would
// drop its images from the display.
func TestClaudeDeliveryAttachmentBearingSendIsNeverMatched(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	img := stagedAttachment{mediaType: "image/png", data: []byte("png")}

	sendNow(t, c, "c_1", "look at this", img)
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	feedFocus(c, capturedFor("c_1", "look at this", 2, time.Unix(100, 0)))

	ck.Require().Len(sends(c, "c_1"), 1, "an image send stays in the overlay")
	ck.StrContains(strings.Join(c.overlayFor("c_1", c.convWidth()).rows, "\n"),
		session.ImagePlaceholder(&rafikiv1.ImageBlock{MediaType: "image/png", Data: []byte("png")}),
		"the image must still be drawn")
}

// Another client's message text cannot acknowledge ours: only a receipt naming
// our inbox row can move a claude row.
func TestClaudeDeliveryOtherClientTextCannotAcknowledgePending(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	// Someone else's send, identical text, captured in the same conversation.
	feedFocus(c, capturedFor("c_1", "hello", 2, time.Unix(100, 0)))

	ck.Require().Len(sends(c, "c_1"), 1, "matching text must never acknowledge a pending claude row")
	ck.False(sends(c, "c_1")[0].sent, "nothing was acknowledged")
	ck.StrContains(c.View().Content, styleGhost.Width(c.convWidth()).Render("▌ hello"),
		"the ghost still stands for the message")
}

// The rail feed may advance an unfocused child's optimistic row, and must never
// touch that child's session — nor any other child's.
func TestClaudeDeliveryRailReceiptAdvancesWithoutMovingACursor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	before := c.sessions["c_1"].Cursor

	// The rail stream is the real one: the event goes down railCh, through the
	// waiter the cockpit armed, and Update applies what it returns.
	c.railCh <- deliveredFor("c_1", "row-1", 7)
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForRailEvent(c.railCh)() }))

	ck.True(sends(c, "c_1")[0].sent, "the rail must advance an unfocused child's row")
	ck.Eq(before, c.sessions["c_1"].Cursor, "the rail feed must never advance a session cursor")
	ck.Eq(int32(7), railCursor(c, "c_1"), "the rail's own cursor does advance")

	// The focus stream's own copy arrives afterwards. It advances the session
	// cursor and changes nothing else — no transcript block, no duplicate row.
	blocks := len(c.sessions["c_1"].Blocks)
	feedFocus(c, deliveredFor("c_1", "row-1", 7))
	ck.Eq(int32(7), c.sessions["c_1"].Cursor, "the focus feed owns the session cursor")
	ck.Eq(blocks, len(c.sessions["c_1"].Blocks), "a receipt is not transcript content")
	ck.Len(sends(c, "c_1"), 1, "the duplicate delivery is idempotent")
}

// The whole lifecycle through the REAL focus channel: pending, sent on the
// receipt, gone once the capture arrives — one Update each, no direct calls to
// the ledger.
func TestClaudeDeliveryFocusChannelDrivesTheWholeLifecycle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 1, "pending")

	c.focusCh <- deliveredFor("c_1", "row-1", 1)
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForEvent(c.focusCh)() }))
	ck.Require().Len(sends(c, "c_1"), 1, "sent, but not yet captured")
	ck.True(sends(c, "c_1")[0].sent, "the focus feed must advance it")

	c.focusCh <- capturedFor("c_1", "hello", 2, time.Unix(100, 0))
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForEvent(c.focusCh)() }))
	ck.Len(sends(c, "c_1"), 0, "the capture retires it")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello"), "the real row is the one left on screen")
}

// Switching focus must not let a rail delivery for the child being LEFT move
// the new focus child's cursor, nor the other way round.
func TestClaudeDeliveryFocusSwitchKeepsCursorsApart(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	sendNow(t, c, "c_1", "hello")
	c.hop("c_2")
	c.focusChild = "c_2"
	c.sessions["c_2"].Cursor, c.sessions["c_2"].HasCursor = -1, true
	c1Before, c2Before := c.sessions["c_1"].Cursor, c.sessions["c_2"].Cursor

	feedRail(c, deliveredFor("c_1", "row-1", 9))

	ck.True(sends(c, "c_1")[0].sent, "the unfocused child's row advances")
	ck.Eq(c1Before, c.sessions["c_1"].Cursor, "c_1's cursor is untouched by the rail")
	ck.Eq(c2Before, c.sessions["c_2"].Cursor, "the focused child's cursor is untouched by c_1's receipt")
}

// A failed send removes exactly its own attempt: identical text is still a
// different send.
func TestClaudeDeliveryFailureAffectsOnlyItsAttempt(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{})

	// Two attempts of the same text; the second one fails.
	first := c.optimistic.add("c_1", ackReceipt, "continue", nil)
	second := c.optimistic.add("c_1", ackReceipt, "continue", nil)

	f := &fakeSend{fail: true}
	mux := http.NewServeMux()
	mux.Handle(rafikiv1connect.NewControlHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c.client = rafikiv1connect.NewControlClient(srv.Client(), srv.URL)

	for _, m := range runCmdDeep(t, c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "continue", nil)) {
		if m != nil {
			c.Update(m)
		}
	}

	ck.Require().Len(sends(c, "c_1"), 2, "the failed attempt is gone; its identical siblings are not")
	ck.Eq(first, sends(c, "c_1")[0].attempt, "the first attempt survived")
	ck.Eq(second, sends(c, "c_1")[1].attempt, "the second survived, and the failed third did not")
	ck.StrContains(c.status, "send failed", "the failure still reports")
}

// The evidence key is the captured EVENT's own timestamp plus its payload —
// never either ordinal namespace. eventconv.EventsFromNodes is what both the
// live capture path and GetHistory go through, and publishEvent only stamps the
// ordinal, so the two copies of one stored message share a key.
func TestClaudeDeliveryEvidenceKeyIgnoresOrdinalsAndUsesTimestamp(t *testing.T) {
	ck := assert.NewCollecting(t)
	at := time.Unix(1_700_000_000, 123_000_000)
	node := &conv.Node{
		Ref: conv.Ref{Ordinal: 4, CreatedAt: at},
		Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hello"}}},
	}
	live := eventconv.EventsFromNodes("c_1", []*conv.Node{node})
	history := eventconv.EventsFromNodes("c_1", []*conv.Node{node})
	ck.Require().Len(live, 1, "one node, one event")

	// publishEvent stamps the event-log ordinal onto the copy it publishes.
	live[0].Ordinal = proto32(91)

	liveKey, ok := evidenceOf(live[0])
	ck.Require().True(ok, "a stored user row carries a timestamp and a payload")
	historyKey, ok := evidenceOf(history[0])
	ck.Require().True(ok, "the history copy does too")
	ck.Eq(historyKey.key, liveKey.key, "the same stored message must have one identity across both paths")
	ck.Eq("hello", liveKey.norm, "the normalized text")

	other := eventconv.EventsFromNodes("c_1", []*conv.Node{{
		Ref: conv.Ref{Ordinal: 5, CreatedAt: at.Add(time.Second)},
		Msg: node.Msg,
	}})
	otherKey, ok := evidenceOf(other[0])
	ck.Require().True(ok, "a second row")
	ck.NotEq(liveKey.key, otherKey.key, "a different timestamp is a different message, whatever it says")

	// An event with no timestamp cannot be identified, so it is never evidence.
	_, ok = evidenceOf(&rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{}},
	})
	ck.False(ok, "an event with no timestamp cannot be identified, so it is never evidence")
}

// The overlay survives a focus change and a pane/session eviction, and is
// cleared when the child leaves the rail.
func TestClaudeDeliveryOverlaySurvivesFocusAndEviction(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	c.hop("c_2")

	// A history refresh of the same child is not a reason to drop it either.
	c.Update(historyMsg{childID: "c_1", after: 2, events: nil})
	ck.Require().Len(sends(c, "c_1"), 1, "a focus change and a history refresh must not clear the overlay")

	// Neither is losing the pane and the session to the LRU.
	c.evictPane("c_1")
	delete(c.sessions, "c_1")
	ck.Require().Len(sends(c, "c_1"), 1, "an evicted pane must not lose the send")

	feedRail(c, deliveredFor("c_1", "row-1", 1))
	ck.True(sends(c, "c_1")[0].sent, "and it can still be acknowledged")

	c.forgetChild("c_1")
	ck.Len(sends(c, "c_1"), 0, "a child that leaves the rail takes its overlay with it")
}

// Fundi is unchanged: its echo is the persisted user row, and matching text
// retires the ghost with no receipt involved.
func TestClaudeDeliveryFundiKeepsItsTextAcknowledgement(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{fundiSummary("c_1", "one", 0)}})

	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 1, "pending")

	feedFocus(c, capturedFor("c_1", "hello", 2, time.Unix(100, 0)))
	ck.Len(sends(c, "c_1"), 0, "a fundi child's echo retires its ghost, receipt or not")
}

// Both states are sanitized: an optimistic row is drawn from text a user
// typed, which can carry an escape sequence like any other transcript text.
func TestClaudeDeliveryBothStatesAreSanitized(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "red \x1b[2Jtext")
	ck.NotStrContains(c.View().Content, "\x1b[2J", "a pending row must not carry a raw escape sequence")

	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.NotStrContains(c.View().Content, "\x1b[2J", "a sent row must not either")
	ck.StrContains(ansi.Strip(c.View().Content), "red text", "the text itself is kept")
}

// A slash command and an abort get no optimistic row: the daemon consumes one
// without recording a message, and the other is not a message at all.
func TestClaudeDeliverySlashAndAbortLeaveNoOverlay(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{})

	for _, m := range runCmdDeep(t, c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "/clear", nil)) {
		if m != nil {
			c.Update(m)
		}
	}
	for _, m := range runCmdDeep(t, c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_ABORT, "", nil)) {
		if m != nil {
			c.Update(m)
		}
	}
	ck.Len(sends(c, "c_1"), 0, "neither a slash command nor an abort leaves a ghost")
}

// ── helpers ─────────────────────────────────────────────────────────────────

type fakeClock struct{ at time.Time }

func (f *fakeClock) now() time.Time          { return f.at }
func (f *fakeClock) advance(d time.Duration) { f.at = f.at.Add(d) }

func proto32(n int32) *int32 { return &n }

// streamMsg arms one of the cockpit's real stream waiters and returns what it
// produced, so a test can push an event down the actual channel and apply the
// message the cockpit's own plumbing would have delivered.
func streamMsg(t *testing.T, arm func() tea.Msg) <-chan tea.Msg {
	t.Helper()
	out := make(chan tea.Msg, 1)
	go func() { out <- arm() }()
	return out
}

func railCursor(c *Cockpit, child string) int32 {
	if n, ok := c.rail.Get(child); ok {
		return n.RailCursor
	}
	return -1
}
