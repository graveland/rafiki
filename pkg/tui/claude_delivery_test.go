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
	"go.graveland.dev/rafiki/pkg/tui/session"
)

// ── Claude delivery acknowledgement ─────────────────────────────────────────
//
// A claude send is acknowledged by a durable message_delivered receipt naming
// the inbox row id SendResponse returned — never by matching the captured text.
// The receipt takes the row out of the pending overlay and puts it into the
// transcript as a provisional block, at the position the captured message will
// occupy; a linked capture or the turn's end finishes it.
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
// timestamp (the stored node's created_at) and the message's content. It
// carries no message id — the daemon could not link it to a send.
func capturedFor(child, text string, ord int32, at time.Time) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: child, Ordinal: &ord, Ts: timestamppb.New(at),
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Content: []*rafikiv1.ContentBlock{{
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
		}}}
}

// linkedFor is capturedFor with the inbox row id the daemon linked it to: the
// stored node, and the send it delivered.
func linkedFor(child, messageID, text string, ord int32, at time.Time) *rafikiv1.Event {
	ev := capturedFor(child, text, ord, at)
	ev.GetUserMessage().MessageId = messageID
	return ev
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

// pending is the rows still drawn in child's pending overlay.
func pending(c *Cockpit, child string) []optimisticSend {
	var out []optimisticSend
	for _, s := range sends(c, child) {
		if !s.accepted {
			out = append(out, s)
		}
	}
	return out
}

// provisionalBlocks is child's transcript blocks standing in for accepted
// sends.
func provisionalBlocks(c *Cockpit, child string) []session.Block {
	s := c.sessions[child]
	if s == nil {
		return nil
	}
	var out []session.Block
	for _, b := range s.Blocks {
		if b.Provisional {
			out = append(out, b)
		}
	}
	return out
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
	ck.Len(sends(c, "c_1"), 1, "the accepted row waits for its capture")
	ck.True(sends(c, "c_1")[0].accepted, "the joined receipt accepts the row")
	ck.Len(c.optimistic.get("c_1").early, 0, "the buffer is cleared once the RPC settles")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")
	ck.Eq("row-1", provisionalBlocks(c, "c_1")[0].MessageID, "the block names the inbox row")
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
	ck.Require().Len(pending(c, "c_1"), 1, "only the acknowledged attempt leaves the overlay")
	ck.Eq(firstID, pending(c, "c_1")[0].inboxID, "the first attempt remains")

	feedFocus(c, deliveredFor("c_1", firstID, 2))
	ck.Len(pending(c, "c_1"), 0, "each receipt takes its own attempt out of the overlay")
	ck.Len(sends(c, "c_1"), 2, "both accepted rows wait for their captures")
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

	ck.Len(pending(c, "c_1"), 0, "the receipt takes the row out of the overlay")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")
	ck.Eq(before, c.sessions["c_1"].Cursor, "the rail feed must never advance a session cursor")
	ck.Eq(int32(7), railCursor(c, "c_1"), "the rail's own cursor does advance")

	// The focus stream's own copy arrives afterwards. It advances the session
	// cursor and adds no second block for a send already in the transcript.
	blocks := len(c.sessions["c_1"].Blocks)
	feedFocus(c, deliveredFor("c_1", "row-1", 7))
	ck.Eq(int32(7), c.sessions["c_1"].Cursor, "the focus feed owns the session cursor")
	ck.Eq(blocks, len(c.sessions["c_1"].Blocks), "a duplicate receipt is not transcript content")
	ck.Len(sends(c, "c_1"), 1, "the duplicate delivery is idempotent")
}

func TestClaudeDeliveryFocusChannelDrivesTheWholeLifecycle(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	ck.Require().Len(sends(c, "c_1"), 1, "pending")

	c.focusCh <- deliveredFor("c_1", "row-1", 1)
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForEvent(c.focusCh)() }))
	ck.Require().Len(pending(c, "c_1"), 0, "the receipt takes the row out of the overlay before any capture")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	c.focusCh <- linkedFor("c_1", "row-1", "hello", 2, time.Unix(100, 0))
	c.Update(<-streamMsg(t, func() tea.Msg { return waitForEvent(c.focusCh)() }))
	ck.Len(sends(c, "c_1"), 0, "the linked capture finishes the send")
	ck.Len(provisionalBlocks(c, "c_1"), 0, "the canonical row took the block's place")
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

	ck.Len(pending(c, "c_1"), 0, "the receipt takes the row out of the overlay")
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
	ck.Len(pending(c, "c_1"), 0, "the receipt takes the row out of the overlay")

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

func TestClaudeDeliveryEchoMovesTheSendIntoTheTranscript(t *testing.T) {
	for _, images := range [][]stagedAttachment{nil, {{mediaType: "image/png", data: []byte("png")}}} {
		for _, text := range []string{"hello there", ""} {
			if text == "" && len(images) == 0 {
				continue // An empty send is not a message.
			}
			t.Run(itoa(int64(len(images)))+text, func(t *testing.T) {
				ck := assert.NewCollecting(t)
				c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
				sendNow(t, c, "c_1", text, images...)
				sent := pending(c, "c_1")
				ck.Require().Len(sent, 1, "pending before echo")
				_ = c.View() // warm the pane cache before the receipt
				feedFocus(c, deliveredFor("c_1", "row-1", 1))
				ck.Len(pending(c, "c_1"), 0, "the echo takes the row out of the pending overlay")
				ck.Eq("", c.overlayFor("c_1").sig, "receipt invalidates the cached overlay")
				ck.NotStrContains(c.View().Content,
					styleGhost.Width(c.convWidth()).Render("▌ "+sent[0].displayText()),
					"the grey pending row is gone")
				blocks := provisionalBlocks(c, "c_1")
				ck.Require().Len(blocks, 1, "the send is in the transcript, independently of capture")
				ck.Eq(session.KindUser, blocks[0].Kind, "the send is drawn as the user's own message")
				ck.Eq(sent[0].displayText(), blocks[0].Text, "the block carries the send, text or attachments")
				ck.StrContains(ansi.Strip(c.View().Content), "✓ delivered", "the block says it is delivered")
			})
		}
	}
}

func TestClaudeDeliveryCaptureBeforeEchoDoesNotRetirePending(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	sendNow(t, c, "c_1", "hello")
	feedFocus(c, capturedFor("c_1", "hello", 1, time.Unix(100, 0)))
	ck.Require().Len(sends(c, "c_1"), 1, "capture cannot substitute for an acceptance receipt")
	ck.False(sends(c, "c_1")[0].accepted, "an unlinked capture accepts nothing")

	feedFocus(c, deliveredFor("c_1", "row-1", 2))
	ck.Len(pending(c, "c_1"), 0, "echo takes the row out of the overlay even when capture came first")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the unlinked capture left the send standing")

	feedFocus(c, turnEndFor("c_1", 3))
	ck.Len(sends(c, "c_1"), 0, "the turn's end ends the send no capture linked")
	ck.Len(provisionalBlocks(c, "c_1"), 0, "and the block that stood for it")
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
	ck.NotStrContains(c.View().Content, "\x1b[2J", "the transcript block cannot inject terminal escapes either")
	ck.StrContains(ansi.Strip(c.View().Content), "red text ✓ delivered",
		"the accepted send is drawn as the user's own message")
}

// A capture the daemon linked to its send finishes that send: the canonical row
// takes the place the provisional block already held, so nothing moves and no
// second row appears.
func TestClaudeDeliveryLinkedCaptureRetiresTheAcceptedSend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")
	ck.StrContains(ansi.Strip(c.View().Content), "✓ delivered", "the provisional block is on screen")

	feedFocus(c, linkedFor("c_1", "row-1", "hello", 2, time.Unix(100, 0)))

	ck.Len(sends(c, "c_1"), 0, "the linked capture finishes the send")
	blocks := c.sessions["c_1"].Blocks
	ck.Require().Len(blocks, 1, "one row, in the place the send already held")
	ck.Eq(session.KindUser, blocks[0].Kind, "kind")
	ck.False(blocks[0].Provisional, "the canonical row is not provisional")
	ck.Eq("row-1", blocks[0].MessageID, "the row names the send it delivered")
	ck.Eq("hello", blocks[0].Text, "text")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello"), "the transcript holds the message once")
	ck.NotStrContains(ansi.Strip(c.View().Content), "✓ delivered", "the canonical row carries no decoration")
}

// The rail and focus feeds are not ordered against each other, so a rail
// turn_end can land before the focus stream's linked message for the child the
// focus stream owns. Ending its accepted send there would take a block away
// from a capture still on its way.
func TestClaudeDeliveryRailTurnEndSparesTheFocusedChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(sends(c, "c_1"), 1, "accepted")

	feedRail(c, turnEndFor("c_1", 9))

	ck.Len(sends(c, "c_1"), 1, "the focused child's ending belongs to its own feed")
	ck.Len(provisionalBlocks(c, "c_1"), 1, "the block still stands for the send")
}

// Nothing else will ever end an unfocused child's accepted send: its session is
// not the one the focus stream advances, so the rail's turn_end is the one that
// finishes it.
func TestClaudeDeliveryRailTurnEndDropsAnUnfocusedChildsAcceptedSend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	c.hop("c_2")
	c.focusChild = "c_2"

	sendNow(t, c, "c_1", "hello")
	feedRail(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(sends(c, "c_1"), 1, "accepted")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	feedRail(c, turnEndFor("c_1", 2))

	ck.Len(sends(c, "c_1"), 0, "an unfocused child's ending ends its accepted send")
	ck.Len(provisionalBlocks(c, "c_1"), 0, "and the block that stood for it")
}

// A child's exit is an ending too. A claude child's only turn_end comes from
// its result frame, so a mid-turn kill or crash publishes none; without this,
// the provisional block and its ledger row stand forever and every history
// refetch puts the block back.
func TestClaudeDeliveryChildExitEndsTheFocusedChildsAcceptedSend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(sends(c, "c_1"), 1, "accepted")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	feedFocus(c, exitedFor("c_1", 2))

	ck.Len(sends(c, "c_1"), 0, "the child's death ends the send no turn end will")
	ck.Len(provisionalBlocks(c, "c_1"), 0, "and the block that stood for it")
	ck.Len(c.optimistic.accepted("c_1"), 0, "a history refetch has nothing left to re-insert")
}

// Nothing else will ever end an unfocused child's accepted send: its session is
// not the one the focus stream advances, so the rail's child_exited is what
// finishes it.
func TestClaudeDeliveryRailChildExitDropsAnUnfocusedChildsAcceptedSend(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})
	c.hop("c_2")
	c.focusChild = "c_2"

	sendNow(t, c, "c_1", "hello")
	feedRail(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(sends(c, "c_1"), 1, "accepted")
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	feedRail(c, exitedFor("c_1", 2))

	ck.Len(sends(c, "c_1"), 0, "an unfocused child's death ends its accepted send")
	ck.Len(provisionalBlocks(c, "c_1"), 0, "and the block that stood for it")
}

// The rail's ending is subject to the same per-feed rule as its turn_end: the
// focus stream owns the focused child, and its exit arrives on that feed.
func TestClaudeDeliveryRailChildExitSparesTheFocusedChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(sends(c, "c_1"), 1, "accepted")

	feedRail(c, exitedFor("c_1", 2))

	ck.Len(sends(c, "c_1"), 1, "the focused child's ending belongs to its own feed")
	ck.Len(provisionalBlocks(c, "c_1"), 1, "the block still stands for the send")
}

// The rail and focus subscriptions overlap on the durable tier, so the same
// receipt arrives twice, and a reconnect replays it. The second delivery must
// return nothing: the caller would insert a provisional block whose linked
// capture has already replaced it.
func TestClaudeDeliveryLedgerDeliverIsIdempotent(t *testing.T) {
	ck := assert.NewCollecting(t)
	l := newOptimisticLedger(time.Now)
	attempt := l.add("c_1", ackReceipt, "hello", nil)
	l.settle("c_1", attempt, "row-1")

	ck.Require().Len(l.deliver("c_1", "row-1"), 1, "the first receipt accepts the send")
	ck.Len(l.deliver("c_1", "row-1"), 0, "a replayed receipt returns no send")
	ck.Len(l.get("c_1").sends, 1, "and leaves the row in place for its capture")
}

// A history fetch rebuilds the transcript from the database, which holds no
// provisional block. The ledger is what remembers the send the receipt accepted,
// so the rebuilt transcript gets it back in the place its capture will land.
func TestClaudeDeliveryHistoryRefetchReinsertsAcceptedSends(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	c.hop("c_2")
	c.sessions["c_1"] = session.New("c_1")
	c.Update(historyMsg{childID: "c_1", after: 2, events: nil})

	blocks := provisionalBlocks(c, "c_1")
	ck.Require().Len(blocks, 1, "a rebuilt transcript gets the accepted send back")
	ck.Eq("hello", blocks[0].Text, "text")
	ck.Eq("row-1", blocks[0].MessageID, "the block names the inbox row")
}

// errorFor is the durable error the daemon logs, which settles every block.
func errorFor(child string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: child, Ordinal: &ord,
		Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
			Code: "upstream", Message: "died"}}}
}

// linkedInjectedOnlyFor is a captured row the daemon linked to its send whose
// only content is what the harness injected alongside it: the user's own words
// are nowhere, so the row leaves no block of its own behind.
func linkedInjectedOnlyFor(child, messageID, injected string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: child, Ordinal: &ord,
		Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{
			Injected: []*rafikiv1.ContentBlock{{
				Index: 0,
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: injected}},
			}},
			MessageId: messageID,
		}}}
}

// An error settles every block, so the linked capture replaces and inserts
// BELOW the renderer's cached prefix. Unless the session reopens the watermark
// there, the pane is served the row it cached before the capture arrived.
func TestClaudeDeliveryLinkedCaptureAfterASettleRebuildsTheStalePane(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().StrContains(ansi.Strip(c.View().Content), "✓ delivered", "the provisional block is on screen")

	feedFocus(c, errorFor("c_1", 2))
	ck.Require().StrContains(ansi.Strip(c.View().Content), "Error: died", "the settled transcript is cached above the send")

	feedFocus(c, linkedFor("c_1", "row-1", "hello", 3, time.Unix(100, 0)))

	ck.NotStrContains(ansi.Strip(c.View().Content), "✓ delivered", "the cached provisional row is not what is drawn")
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "hello"), "the canonical row took its place")
}

// A focus-channel event for a child the subscription no longer owns is a stale
// frame from the previous hop. Moving the ledger on it takes the id away from a
// block the session still draws, and nothing is left to end that block.
func TestClaudeDeliveryStaleFocusTurnEndSparesAPreviousChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	c.hop("c_2")
	c.focusChild = "c_2"
	feedFocus(c, turnEndFor("c_1", 2))

	ck.Len(sends(c, "c_1"), 1, "a stale frame must not take the send away")
	ck.Len(provisionalBlocks(c, "c_1"), 1, "the block still stands for it")
}

// A linked capture carrying nothing of the user's own leaves the session with
// only the id. A receipt that joins afterwards must not put a block back that
// nothing will ever replace.
func TestClaudeDeliveryLateReceiptAfterALinkedCaptureInsertsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	cmd := c.sendTo("c_1", rafikiv1.SendMode_SEND_MODE_PROMPT, "hello", nil)
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	feedFocus(c, linkedInjectedOnlyFor("c_1", "row-1", "A", 2))
	ck.Require().Len(provisionalBlocks(c, "c_1"), 0, "the linked row left no block of the user's own")

	for _, m := range runCmdDeep(t, cmd) {
		if m != nil {
			c.Update(m)
		}
	}

	ck.Len(sends(c, "c_1"), 1, "the buffered receipt still accepts the send")
	ck.Len(provisionalBlocks(c, "c_1"), 0, "a send already linked is never inserted again")
}

// The ledger follows the SESSION's decision, not the event's arrival. A focus
// event the session refuses as an ordinal it has already folded in has had its
// say once; moving the ledger on it takes the id away from a block the session
// still draws, and the two never agree again.
func TestClaudeDeliveryFocusEventTheSessionRefusedDoesNotMoveTheLedger(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, deliveredFor("c_1", "row-1", 1))
	ck.Require().Len(provisionalBlocks(c, "c_1"), 1, "the send holds its place in the transcript")

	// The session's cursor is past both of these, as it is once a replay
	// overlaps the point the subscription resumed from.
	c.sessions["c_1"].Cursor, c.sessions["c_1"].HasCursor = 9, true
	feedFocus(c, linkedFor("c_1", "row-1", "hello", 9, time.Unix(100, 0)))
	ck.Len(sends(c, "c_1"), 1, "the send is not retired by an event the session refused")

	feedFocus(c, turnEndFor("c_1", 9))
	ck.Len(sends(c, "c_1"), 1, "nor dropped by a turn end it refused")
	ck.Len(provisionalBlocks(c, "c_1"), 1, "the block the ledger still stands for is still there")
}

// exitedFor is the durable child_exited the daemon logs. It settles every
// block and appends none, so an open placeholder stays trailing and settled.
func exitedFor(child string, ord int32) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: child, Ordinal: &ord,
		Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{}}}
}

// A send answered at an ordinal the session has already folded in is still a
// send: the ledger accepts it, and the block it stands for has to be DRAWN. The
// exit above settled the placeholder, so the insert lands below the watermark
// and the pane rebuilds over a cache that already covers that index — unless
// the insert reopens the watermark, the new row is never rendered.
func TestClaudeDeliveryProvisionalAfterASettleRebuildsTheStalePane(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := claudeCockpit(t, &fakeSend{ids: []string{"row-1"}})

	sendNow(t, c, "c_1", "hello")
	feedFocus(c, turnStartFor("c_1"))
	feedFocus(c, exitedFor("c_1", 1))
	ck.Require().Eq(len(c.sessions["c_1"].Blocks), c.sessions["c_1"].Finalized,
		"the exit settled every block, the placeholder included")
	_ = c.View() // the settled transcript is what the renderer caches

	feedFocus(c, deliveredFor("c_1", "row-1", 2))

	ck.Len(pending(c, "c_1"), 0, "the receipt takes the row out of the overlay")
	ck.StrContains(ansi.Strip(c.View().Content), "hello ✓ delivered",
		"the send stands in the transcript, ahead of the placeholder it answered")
}
