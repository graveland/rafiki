// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// seedAccepted fills one child's link queue the way an echo does: each delivery
// is registered against a fresh launch and taken, in order. The newest launch's
// generation is returned so a test can retire it.
func seedAccepted(t *testing.T, r *claudeDeliveryRegistry, childID string, ds ...claudeDelivery) uint64 {
	t.Helper()
	gen := r.beginLaunch(childID)
	for i, d := range ds {
		u := fmt.Sprintf("u-%s-%d", childID, i)
		if _, ok := r.register(childID, u, d.rowID, d.text, d.images); !ok {
			t.Fatalf("register(%s, %s): no live launch", childID, d.rowID)
		}
		if _, _, ok := r.take(childID, gen, u); !ok {
			t.Fatalf("take(%s, %s): missed", childID, u)
		}
	}
	return gen
}

// acceptedCount is a test-only view of one child's link queue length, so a test
// can prove a link removed exactly what it named and left the rest.
func (r *claudeDeliveryRegistry) acceptedCount(childID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.accepted[childID])
}

// lockBuffer is a slog sink that is safe to write from any goroutine.
type lockBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// One request carried one message, and Claude Code rewrote it before reporting
// it: the counts agree, so position is the answer and the row keeps its name.
func TestClaudeLinkByOrderWhenTextDiffers(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	seedAccepted(t, r, "c", claudeDelivery{rowID: "row-1", text: "hello world"})

	ids, logs := r.link("c", time.Now(), []linkUnit{{text: "hello  wörld (edited)"}})

	ck.EqDeep([]string{"row-1"}, ids, "linked by order")
	ck.Require().Len(logs, 1, "logs")
	ck.Eq("claude delivery linked by order; text differs", logs[0].msg, "log message")
	ck.Eq("row-1", logs[0].rowID, "logged row")
	ck.Eq("hello world", logs[0].sent, "logged sent text")
	ck.Eq("hello  wörld (edited)", logs[0].captured, "logged captured text")
	ck.Eq(0, r.acceptedCount("c"), "the linked delivery left the queue")
}

// The capture is one message, the queue has two: the unit matches the second,
// so the first is a delivery whose capture never arrived and is dropped.
func TestClaudeLinkGreedyByTextWhenCountsDiffer(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	seedAccepted(t, r, "c",
		claudeDelivery{rowID: "row-A", text: "alpha"},
		claudeDelivery{rowID: "row-B", text: "beta"},
	)

	ids, logs := r.link("c", time.Now(), []linkUnit{{text: "beta"}})

	ck.EqDeep([]string{"row-B"}, ids, "linked to the matching delivery")
	ck.Require().Len(logs, 1, "logs")
	ck.Eq("claude delivery never linked", logs[0].msg, "log message")
	ck.Eq("row-A", logs[0].rowID, "logged row")
	ck.Eq("a later delivery linked", logs[0].reason, "log reason")
	ck.Eq(0, r.acceptedCount("c"), "both deliveries left the queue")
}

// A capture that matches nothing must not consume the deliveries: they may
// belong to a later request whose capture has not reported yet.
func TestClaudeLinkUnmatchedUnitStaysUnlinked(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	seedAccepted(t, r, "c", claudeDelivery{rowID: "row-A", text: "alpha"})

	ids, logs := r.link("c", time.Now(), []linkUnit{{text: "xx"}, {text: "yy"}})

	ck.EqDeep([]string{"", ""}, ids, "no delivery matched")
	ck.Require().Len(logs, 2, "logs")
	ck.Eq("captured user message matched no accepted delivery", logs[0].msg, "log message")
	ck.Eq(1, r.acceptedCount("c"), "queue after an unmatched capture")
}

// A delivery accepted AFTER the request arrived cannot be one of its messages:
// the capture is written after the turn it belongs to, so an echo from that turn
// is later than the request and must stay out of reach.
func TestClaudeLinkRespectsEchoTiming(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	t0 := time.Now()
	seedAccepted(t, r, "c", claudeDelivery{rowID: "row-A", text: "hello"})

	r.mu.Lock()
	r.accepted["c"][0].echoAt = t0.Add(2 * time.Second)
	r.mu.Unlock()

	ids, _ := r.link("c", t0, []linkUnit{{text: "hello"}})
	ck.EqDeep([]string{""}, ids, "a delivery echoed after the request is not eligible")
	ck.Eq(1, r.acceptedCount("c"), "the ineligible delivery stays queued")

	ids, _ = r.link("c", time.Time{}, []linkUnit{{text: "hello"}})
	ck.EqDeep([]string{"row-A"}, ids, "an unknown request time makes every delivery eligible")
	ck.Eq(0, r.acceptedCount("c"), "queue after linking")
}

// An image-only send has no text to compare, so it matches on its image count —
// and a differing count is not a near miss, it is a different message.
func TestClaudeLinkImageOnlySend(t *testing.T) {
	ck := assert.NewCollecting(t)

	one := &claudeDeliveryRegistry{}
	seedAccepted(t, one, "c", claudeDelivery{rowID: "row-A", images: 2})
	ids, logs := one.link("c", time.Now(), []linkUnit{{images: 2}})
	ck.EqDeep([]string{"row-A"}, ids, "the image count matched")
	ck.Empty(logs, "an exact pairing logs nothing")

	two := &claudeDeliveryRegistry{}
	seedAccepted(t, two, "c",
		claudeDelivery{rowID: "row-B", images: 2},
		claudeDelivery{rowID: "row-C", images: 2},
	)
	ids, logs = two.link("c", time.Now(), []linkUnit{{images: 1}})
	ck.EqDeep([]string{""}, ids, "a differing image count links nothing")
	ck.Require().Len(logs, 1, "logs")
	ck.Eq("captured user message matched no accepted delivery", logs[0].msg, "log message")
	ck.Eq(2, two.acceptedCount("c"), "an image mismatch consumes nothing")
}

// A delivery is eligible when the echo landed no later than the request plus the
// grace: the echo and the arrival are stamped by different goroutines, so a small
// lag is the same request, and a longer one is a later request's message.
func TestClaudeLinkSkewWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		after  time.Duration
		linked bool
	}{
		{"inside the grace", claudeLinkSkew / 2, true},
		{"past the grace", claudeLinkSkew + time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewCollecting(t)
			r := &claudeDeliveryRegistry{}
			t0 := time.Now()
			seedAccepted(t, r, "c", claudeDelivery{rowID: "row-A", text: "hello"})

			r.mu.Lock()
			r.accepted["c"][0].echoAt = t0.Add(tc.after)
			r.mu.Unlock()

			ids, _ := r.link("c", t0, []linkUnit{{text: "hello"}})
			if tc.linked {
				ck.EqDeep([]string{"row-A"}, ids, "an echo inside the grace belongs to the request")
				ck.Eq(0, r.acceptedCount("c"), "queue after linking")
				return
			}
			ck.EqDeep([]string{""}, ids, "an echo past the grace belongs to a later request")
			ck.Eq(1, r.acceptedCount("c"), "the ineligible delivery stays queued")
		})
	}
}

// A queue that overflows means captures are not being reported: the oldest
// delivery is dropped (nothing will ever link it) and the drop is logged, so the
// live deliveries stay matchable and the stall is visible.
func TestClaudeLinkQueueCap(t *testing.T) {
	ck := assert.NewCollecting(t)
	logs := &lockBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newClaudeDeliveryHarness(t, "c_link")
	hook, _ := h.launch(t, "c_link")
	var written []string
	h.writeEcho(hook, &written, nil)

	oldest := h.send(t, "c_link", "row 0")
	for i := 1; i < claudeLinkQueueCap; i++ {
		h.send(t, "c_link", fmt.Sprintf("row %d", i))
	}
	ck.Eq(claudeLinkQueueCap, h.c.claudeDeliveries.acceptedCount("c_link"), "queue at the cap")

	h.send(t, "c_link", "overflow")
	ck.Eq(claudeLinkQueueCap, h.c.claudeDeliveries.acceptedCount("c_link"), "the cap held")
	ck.StrContains(logs.String(), "claude delivery never linked", "drop logged")
	ck.StrContains(logs.String(), oldest, "the dropped row is named")
	ck.StrContains(logs.String(), "accepted queue full", "drop reason logged")
}

// A captured message is linked only to the launch that accepted it: a resumed
// process replays the old conversation, and a replayed user frame is
// indistinguishable from a fresh acceptance, so the retired launch's queue goes.
func TestClaudeLinkRetiredWithTheLaunch(t *testing.T) {
	ck := assert.NewCollecting(t)
	units := []linkUnit{{text: "hello"}}

	exited := &claudeDeliveryRegistry{}
	gen := seedAccepted(t, exited, "c", claudeDelivery{rowID: "row-A", text: "hello"})
	inst := &child.Child{}
	exited.attach(inst, gen)
	exited.endLaunch("c", inst)
	ids, _ := exited.link("c", time.Now(), units)
	ck.EqDeep([]string{""}, ids, "endLaunch retired the accepted delivery")
	ck.Eq(0, exited.acceptedCount("c"), "queue after endLaunch")

	resumed := &claudeDeliveryRegistry{}
	seedAccepted(t, resumed, "c", claudeDelivery{rowID: "row-A", text: "hello"})
	resumed.beginLaunch("c")
	ids, _ = resumed.link("c", time.Now(), units)
	ck.EqDeep([]string{""}, ids, "beginLaunch retired the accepted delivery")
	ck.Eq(0, resumed.acceptedCount("c"), "queue after beginLaunch")
}

// An injected block is the daemon's own word and never part of the message a
// human sent, and a tool result is not a send at all: neither is a unit.
func TestClaudeLinkInjectedNeverAUnit(t *testing.T) {
	ck := assert.NewCollecting(t)

	injectedOnly := &rafikiv1.UserMessage{
		Injected: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "job done"}}}},
		Content:  []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{}}}},
	}
	_, ok := linkUnitOf(injectedOnly)
	ck.False(ok, "injected text and a tool result are not a send")

	sent := &rafikiv1.UserMessage{
		Injected: []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "job done"}}}},
		Content:  []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "the real ask"}}}},
	}
	u, ok := linkUnitOf(sent)
	ck.Require().True(ok, "a text block is a unit")
	ck.Eq("the real ask", u.text, "injected text is not part of the unit")
	ck.Eq(0, u.images, "images")
}

// The link reaches exactly one report shape: the child's own main-thread
// capture. A subagent thread's request is another conversation this child's
// stream never carried, and a release-only report has no rows to publish at all,
// so neither may consume a queued delivery.
func TestCaptureReportedLinksOnlyTheMainThread(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	now := time.Now()

	accept := func(rowID, text string) {
		t.Helper()
		gen := h.c.claudeDeliveries.beginLaunch("c_1")
		if _, ok := h.c.claudeDeliveries.register("c_1", "u-"+rowID, rowID, text, 0); !ok {
			t.Fatalf("register(%s): no live launch", rowID)
		}
		if _, _, ok := h.c.claudeDeliveries.take("c_1", gen, "u-"+rowID); !ok {
			t.Fatalf("take(%s): missed", rowID)
		}
	}

	accept("row-1", "m0")
	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1", RequestReceived: time.Now(),
		Rows: []*conv.Node{capRow(0, "user", now, ""), capRow(1, "assistant", now, "end_turn")},
	})
	h.drain()

	ck.Require().Len(h.got, 2, "events")
	ck.Require().NotNil(h.got[0].GetUserMessage(), "the first event is a user message")
	ck.Eq("row-1", h.got[0].GetUserMessage().GetMessageId(), "the captured message names its inbox row")
	ck.Eq(0, h.c.claudeDeliveries.acceptedCount("c_1"), "the linked delivery left the queue")

	ck.Require().NoError(h.c.EnsureThreadChild("c_1", "th", "conv-th"), "EnsureThreadChild")
	h.drain()
	h.log, h.got = nil, nil

	accept("row-2", "m0")
	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ThreadID: "th", ConversationID: "conv-th", ResponseMessageID: "msg_9", RequestReceived: time.Now(),
		Rows: []*conv.Node{capRow(0, "user", now, "")},
	})
	h.drain()

	ck.Require().Len(h.got, 1, "a subagent thread still publishes its row")
	ck.Eq("", h.got[0].GetUserMessage().GetMessageId(), "a subagent thread never links")
	ck.Eq(1, h.c.claudeDeliveries.acceptedCount("c_1"), "the subagent report consumed nothing")

	h.log, h.got = nil, nil
	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1", ReleaseOnly: true, RequestReceived: time.Now(),
		Rows: []*conv.Node{capRow(0, "user", now, "")},
	})
	h.drain()

	ck.Eq(0, len(h.got), "a release-only report publishes nothing")
	ck.Eq(1, h.c.claudeDeliveries.acceptedCount("c_1"), "a release-only report consumes nothing")
}

// recordedMidTurnRequest is the corpus's mid-turn request. It is the evidence
// that the relay split and the link agree on what Claude Code actually wrote,
// so the test reads the recorded bytes rather than retyping them.
const recordedMidTurnRequest = "../../testdata/claude-capture-delivery/000002-request.http"

// recordedMidTurnRelay decodes the one recorded message that carries a relayed
// mid-turn message, through the same codec the capture uses.
func recordedMidTurnRelay(t *testing.T) msg.Message {
	t.Helper()
	c := assert.NewAborting(t)
	raw, err := os.ReadFile(recordedMidTurnRequest)
	c.NoError(err, "read the recorded mid-turn request")
	_, body, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("%s is not an HTTP fixture", recordedMidTurnRequest)
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(body, &req), "parse the recorded request")
	var found []msg.Message
	for _, m := range req.Messages {
		if !bytes.Contains(m.Content, []byte("The user sent a new message while you were working")) {
			continue
		}
		decoded, err := anthropic.Decode(m.Role, m.Content)
		c.NoError(err, "decode the recorded message")
		found = append(found, decoded)
	}
	c.Require().Len(found, 1, "the recording carries exactly one mid-turn relay")
	return found[0]
}

// The relay split and the link are unit-tested apart, in different packages,
// so this drives the seam between them with the RECORDED bytes: Claude Code
// folded the mid-turn message into a tool result, the split recovers exactly
// the words the user sent, and the linker names the row that delivered them.
func TestClaudeLinkWorksOnTheRecordedMidTurnRelay(t *testing.T) {
	ck := assert.NewCollecting(t)
	m := recordedMidTurnRelay(t)

	um := eventconv.UserMessageFrom(conv.OriginClaudeCode, m)
	unit, ok := linkUnitOf(um)
	ck.Require().True(ok, "the split leaves a linkable unit")
	ck.Eq("Reply with exactly one word: also", unit.text, "the unit is exactly the relayed user words")
	ck.Eq(0, unit.images, "the recorded relay carries no image")

	r := &claudeDeliveryRegistry{}
	seedAccepted(t, r, "c", claudeDelivery{rowID: "row-also", text: "Reply with exactly one word: also"})
	ids, logs := r.link("c", time.Now(), []linkUnit{unit})

	ck.EqDeep([]string{"row-also"}, ids, "the recorded relay links to the row that carried it")
	ck.Len(logs, 0, "an exact match warns about nothing")
	ck.Eq(0, r.acceptedCount("c"), "the linked delivery left the queue")
}
