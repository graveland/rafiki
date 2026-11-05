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

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ── Claude delivery acknowledgement ─────────────────────────────────────────
//
// A claude send is acknowledged by a durable message_delivered receipt naming
// the inbox row id SendResponse returned — never by matching the captured text.
// The receipt immediately removes the overlay, independently of capture.
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

func TestClaudeDeliveryReceiptBeforeTheResponseIsJoined(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	// Issue the RPC but hold its message back.
	cmd := c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "hello", nil)
	ck.Require().Len(sends(c, "c_1"), 1, "the row exists before the RPC is even run")

	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Len(sends(c, "c_1"), 1, "an unjoined receipt cannot remove a row")
	ck.Len(c.optimistic.get("c_1").early, 1, "the receipt must be buffered while an RPC is outstanding")

	for _, m := range runCmdDeep(t, cmd) {
		if m != nil {
			c.Update(m)
		}
	}
	ck.Len(sends(c, "c_1"), 0, "the joined receipt immediately retires the row")
	ck.Len(c.optimistic.get("c_1").early, 0, "the buffer is cleared once the RPC settles")
}

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
	ck.Require().Len(sends(c, "c_1"), 1, "only the acknowledged attempt retires")
	ck.Eq(firstID, sends(c, "c_1")[0].inboxID, "the first attempt remains")

	feedFocus(c, deliveredFor("c_1", firstID, 2))
	ck.Len(sends(c, "c_1"), 0, "each receipt retires its own attempt")
}

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
	ck.Len(l.get("c_1").sends, 1, "an expired receipt must never be joined")

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

func TestClaudeDeliveryUnclaimedReceiptsAreNotRetained(t *testing.T) {
	ck := assert.NewCollecting(t)
	l := newOptimisticLedger(time.Now)

	attempt := l.add("c_1", ackReceipt, "hello", nil)
	l.settle("c_1", attempt, "row-1")
	l.deliver("c_1", "somebody-elses-row")

	ck.Len(l.get("c_1").early, 0, "no RPC is outstanding, so nothing can be buffered")
	ck.Len(l.get("c_1").sends, 1, "a foreign id must not acknowledge this send")
}

func TestClaudeDeliveryOtherClientTextCannotAcknowledgePending(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	// Someone else's send, identical text, captured in the same conversation.
	feedFocus(c, capturedFor("c_1", "hello", 2, time.Unix(100, 0)))

	ck.Require().Len(sends(c, "c_1"), 1, "matching text must never acknowledge a pending claude row")
	ck.StrContains(c.View().Content, styleGhost.Width(c.convWidth()).Render("▌ hello"),
		"the ghost still stands for the message")
}

func TestClaudeDeliveryRailReceiptAdvancesWithoutMovingACursor(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	before := c.sessions["c_1"].Cursor

	// The rail stream is the real one: the event goes down railCh, through the
	// waiter the cockpit armed, and Update applies what it returns.
	c.railCh <- deliveredFor("c_1", "row-1", 7)
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForRailEvent(c.railCh)() }))

	ck.Len(sends(c, "c_1"), 0, "the receipt removes the overlay without capture")
	ck.Eq(before, c.sessions["c_1"].Cursor, "the rail feed must never advance a session cursor")
	ck.Eq(int32(7), railCursor(c, "c_1"), "the rail's own cursor does advance")

	// The focus stream's own copy arrives afterwards. It advances the session
	// cursor and changes nothing else — no transcript block, no duplicate row.
	blocks := len(c.sessions["c_1"].Blocks)
	feedFocus(c, deliveredFor("c_1", "row-1", 7))
	ck.Eq(int32(7), c.sessions["c_1"].Cursor, "the focus feed owns the session cursor")
	ck.Eq(blocks, len(c.sessions["c_1"].Blocks), "a receipt is not transcript content")
	ck.Len(sends(c, "c_1"), 0, "the duplicate delivery is idempotent")
}

func TestClaudeDeliveryFocusChannelDrivesTheWholeLifecycle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 1, "pending")

	c.focusCh <- deliveredFor("c_1", "row-1", 1)
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForEvent(c.focusCh)() }))
	ck.Require().Len(sends(c, "c_1"), 0, "receipt removes it before any capture")

	c.focusCh <- capturedFor("c_1", "hello", 2, time.Unix(100, 0))
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForEvent(c.focusCh)() }))
	ck.Len(sends(c, "c_1"), 0, "capture does not resurrect the overlay")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello"), "the real row is the one left on screen")
}

func TestClaudeDeliveryFocusSwitchKeepsCursorsApart(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1", "row-2"}})

	sendNow(t, c, "c_1", "hello")
	c.hop("c_2")
	c.focusChild = "c_2"
	c.sessions["c_2"].Cursor, c.sessions["c_2"].HasCursor = -1, true
	c1Before, c2Before := c.sessions["c_1"].Cursor, c.sessions["c_2"].Cursor

	feedRail(c, deliveredFor("c_1", "row-1", 9))

	ck.Len(sends(c, "c_1"), 0, "the receipt removes the overlay without capture")
	ck.Eq(c1Before, c.sessions["c_1"].Cursor, "c_1's cursor is untouched by the rail")
	ck.Eq(c2Before, c.sessions["c_2"].Cursor, "the focused child's cursor is untouched by c_1's receipt")
}

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
	ck.Len(sends(c, "c_1"), 0, "the receipt removes the overlay without capture")

	c.forgetChild("c_1")
	ck.Len(sends(c, "c_1"), 0, "a child that leaves the rail takes its overlay with it")
}

func TestClaudeDeliveryFundiKeepsItsTextAcknowledgement(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{fundiSummary("c_1", "one", 0)}})

	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 1, "pending")

	feedFocus(c, capturedFor("c_1", "hello", 2, time.Unix(100, 0)))
	ck.Len(sends(c, "c_1"), 0, "a fundi child's echo retires its ghost, receipt or not")
}

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

func TestClaudeDeliveryEchoRetiresTextAndAttachmentsWithoutCapture(t *testing.T) {
	for _, images := range [][]stagedAttachment{nil, {{mediaType: "image/png", data: []byte("png")}}} {
		for _, text := range []string{"hello there", ""} {
			if text == "" && len(images) == 0 {
				continue // An empty send is not a message.
			}
			t.Run(itoa(int64(len(images)))+text, func(t *testing.T) {
				ck := assert.NewCollecting(t)
				c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
				sendNow(t, c, "c_1", text, images...)
				ck.Require().Len(sends(c, "c_1"), 1, "pending before echo")
				_ = c.View() // warm the pane cache before the receipt
				feedFocus(c, deliveredFor("c_1", "row-1", 1))
				ck.Len(sends(c, "c_1"), 0, "echo hands custody to Claude, regardless of content")
				ck.Eq("", c.overlayFor("c_1").sig, "receipt invalidates the cached overlay")
				if text != "" {
					ck.NotStrContains(ansi.Strip(c.View().Content), text, "no bright or grey row remains before capture")
				}
			})
		}
	}
}

func TestClaudeDeliveryCaptureBeforeEchoDoesNotRetirePending(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	sendNow(t, c, "c_1", "hello")
	feedFocus(c, capturedFor("c_1", "hello", 1, time.Unix(100, 0)))
	ck.Len(sends(c, "c_1"), 1, "capture cannot substitute for an acceptance receipt")
	feedFocus(c, deliveredFor("c_1", "row-1", 2))
	ck.Len(sends(c, "c_1"), 0, "echo retires even when capture came first")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello"), "only the canonical transcript remains")
}

func TestClaudeDeliveryReceiptDoesNotAcknowledgeFundi(t *testing.T) {
	ck := assert.NewCollecting(t)
	l := newOptimisticLedger(time.Now)
	attempt := l.add("c_1", ackText, "hello", nil)
	l.deliver("c_1", "row-1")
	l.settle("c_1", attempt, "row-1")
	l.deliver("c_1", "row-1")
	ck.Len(l.get("c_1").sends, 1, "fundi still waits for its persisted user row")
}

type fakeClock struct{ at time.Time }

func (f *fakeClock) now() time.Time          { return f.at }
func (f *fakeClock) advance(d time.Duration) { f.at = f.at.Add(d) }

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

func TestClaudeDeliveryHistoryCannotRetirePendingAndForgetDropsBuffer(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{})
	c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "hello", nil)
	feedRail(c, deliveredFor("c_1", "row-1", 1))
	c.Update(historyMsg{childID: "c_1", after: 2, events: []*rafikiv1.Event{capturedFor("c_1", "hello", 2, time.Unix(100, 0))}})
	ck.Len(sends(c, "c_1"), 1, "history cannot substitute for a joined receipt")
	ck.Len(c.optimistic.get("c_1").early, 1, "early receipt is still buffered")
	c.forgetChild("c_1")
	ck.Nil(c.optimistic.get("c_1"), "forget drops pending rows and early receipts")
}

func TestClaudeDeliveryPendingTextIsSanitized(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	sendNow(t, c, "c_1", "red \x1b[2Jtext")
	ck.NotStrContains(c.View().Content, "\x1b[2J", "pending text cannot inject terminal escapes")
	ck.StrContains(ansi.Strip(c.View().Content), "red text", "safe text is visible")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.NotStrContains(ansi.Strip(c.View().Content), "red text", "echo removes the row")
}
