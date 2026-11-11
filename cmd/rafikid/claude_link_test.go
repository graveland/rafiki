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

// seedBound fills one child's link queue the way a launch does: each delivery
// is registered against a fresh launch, echoed (take) in order, and then bound
// to responseID as the next main-thread assistant frame would. The launch's
// generation is returned so a test can retire it or keep driving it.
func seedBound(t *testing.T, r *claudeDeliveryRegistry, childID, responseID string, ds ...claudeDelivery) uint64 {
	t.Helper()
	gen := r.beginLaunch(childID)
	for _, d := range ds {
		echo(t, r, childID, gen, d)
	}
	r.bind(childID, gen, responseID)
	return gen
}

// echo registers one delivery on gen and takes it, as a write followed by
// Claude Code's acceptance echo would. It leaves the delivery unbound.
func echo(t *testing.T, r *claudeDeliveryRegistry, childID string, gen uint64, d claudeDelivery) {
	t.Helper()
	u := "u-" + childID + "-" + d.rowID
	if _, ok := r.register(childID, u, d.rowID, d.text, d.images); !ok {
		t.Fatalf("register(%s, %s): no live launch", childID, d.rowID)
	}
	if _, _, ok := r.take(childID, gen, u); !ok {
		t.Fatalf("take(%s, %s): missed", childID, u)
	}
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
	seedBound(t, r, "c", "msg_1", claudeDelivery{rowID: "row-1", text: "hello world"})

	ids, logs := r.link("c", "msg_1", []linkUnit{{text: "hello  wörld (edited)"}})

	ck.EqDeep([]string{"row-1"}, ids, "linked by order")
	ck.Require().Len(logs, 1, "logs")
	ck.Eq("claude delivery linked by order; text differs", logs[0].msg, "log message")
	ck.Eq("row-1", logs[0].rowID, "logged row")
	ck.Eq("hello world", logs[0].sent, "logged sent text")
	ck.Eq("hello  wörld (edited)", logs[0].captured, "logged captured text")
	ck.Eq(0, r.acceptedCount("c"), "the linked delivery left the queue")
}

// The capture is one message, two deliveries were bound to its response: the
// unit matches the second, and the first — bound to this response but named by
// none of its units — leaves the queue as never linked, because no other
// capture can name it.
func TestClaudeLinkGreedyByTextWhenCountsDiffer(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	seedBound(t, r, "c", "msg_1",
		claudeDelivery{rowID: "row-A", text: "alpha"},
		claudeDelivery{rowID: "row-B", text: "beta"},
	)

	ids, logs := r.link("c", "msg_1", []linkUnit{{text: "beta"}})

	ck.EqDeep([]string{"row-B"}, ids, "linked to the matching delivery")
	ck.Require().Len(logs, 1, "logs")
	ck.Eq("claude delivery never linked", logs[0].msg, "log message")
	ck.Eq("row-A", logs[0].rowID, "logged row")
	ck.Eq("not in its response's capture", logs[0].reason, "log reason")
	ck.Eq(0, r.acceptedCount("c"), "both deliveries left the queue")
}

// A unit that matches no bound delivery names nothing, and the bound delivery
// it failed to match is dropped with its response, never carried to a later one.
func TestClaudeLinkUnmatchedUnitNamesNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	seedBound(t, r, "c", "msg_1", claudeDelivery{rowID: "row-A", text: "alpha"})

	ids, logs := r.link("c", "msg_1", []linkUnit{{text: "xx"}, {text: "yy"}})

	ck.EqDeep([]string{"", ""}, ids, "no delivery matched")
	ck.Require().Len(logs, 3, "logs")
	ck.Eq("captured user message matched no accepted delivery", logs[0].msg, "log message")
	ck.Eq("captured user message matched no accepted delivery", logs[1].msg, "log message")
	ck.Eq("not in its response's capture", logs[2].reason, "the bound row is dropped")
	ck.Eq(0, r.acceptedCount("c"), "nothing is carried past its response")
}

// The production off-by-one: Claude Code echoes an idle send only after the
// request carrying it has reached the proxy, so a link keyed on the echo's
// arrival missed every message's own capture and named it on the NEXT turn's.
// Bound by stdout order instead, each capture names its own row, and a capture
// of a response printed BEFORE the echo leaves the delivery for its own.
func TestClaudeLinkEachTurnNamesItsOwnRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	gen := r.beginLaunch("c")

	turns := []struct{ row, text, resp string }{
		{"row-0", "bee is down again", "msg_1"},
		{"row-1", "can we bump z2m's replica?", "msg_2"},
		{"row-2", "registry: the data is in s3?", "msg_3"},
	}
	for _, tc := range turns {
		echo(t, r, "c", gen, claudeDelivery{rowID: tc.row, text: tc.text})

		ids, logs := r.link("c", "msg_prev_"+tc.row, []linkUnit{{text: "Tool loaded."}})
		ck.EqDeep([]string{""}, ids, "%s: an earlier response's capture names nothing", tc.row)
		ck.Empty(logs, "%s: an unbound delivery is not that capture's to judge", tc.row)

		r.bind("c", gen, tc.resp)
		ids, logs = r.link("c", tc.resp, []linkUnit{{text: tc.text}})
		ck.EqDeep([]string{tc.row}, ids, "%s names its own row", tc.row)
		ck.Empty(logs, "%s logs nothing", tc.row)
	}
	ck.Eq(0, r.acceptedCount("c"), "nothing left queued")
}

// Every content block of one response prints its own frame with the same id.
// An echo printed between two of them belongs to the NEXT request, so a repeat
// of the latest id binds nothing and the delivery waits for a new one.
func TestClaudeLinkRepeatedFrameBindsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	gen := r.beginLaunch("c")

	echo(t, r, "c", gen, claudeDelivery{rowID: "row-A", text: "alpha"})
	r.bind("c", gen, "msg_1")
	echo(t, r, "c", gen, claudeDelivery{rowID: "row-B", text: "beta"})
	r.bind("c", gen, "msg_1")

	ids, _ := r.link("c", "msg_1", []linkUnit{{text: "alpha"}})
	ck.EqDeep([]string{"row-A"}, ids, "msg_1 carried only the first")
	ck.Eq(1, r.acceptedCount("c"), "the second waits for its own response")

	r.bind("c", gen, "msg_2")
	ids, logs := r.link("c", "msg_2", []linkUnit{{text: "beta"}})
	ck.EqDeep([]string{"row-B"}, ids, "the next response names it")
	ck.Empty(logs, "an exact pairing logs nothing")
}

// A capture can report before the child's stdout reaches its response's first
// frame. It publishes unlinked, and a delivery bound to that id afterwards is
// dropped with a warning instead of waiting for a report that will not come.
func TestClaudeLinkCaptureFirstDropsTheLateBinding(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	gen := r.beginLaunch("c")
	echo(t, r, "c", gen, claudeDelivery{rowID: "row-A", text: "alpha"})

	ids, logs := r.link("c", "msg_1", []linkUnit{{text: "alpha"}})
	ck.EqDeep([]string{""}, ids, "nothing was bound yet")
	ck.Empty(logs, "an unbound delivery is not judged")

	logs = r.bind("c", gen, "msg_1")
	ck.Require().Len(logs, 1, "logs")
	ck.Eq("claude delivery never linked", logs[0].msg, "log message")
	ck.Eq("row-A", logs[0].rowID, "logged row")
	ck.Eq("capture published first", logs[0].reason, "log reason")
	ck.Eq(0, r.acceptedCount("c"), "the late delivery left the queue")
}

// Claude Code's synthetic API-error reply has no id the proxy reports, so the
// child never fires the hook for it (pkg/child: GatedMessageID) and an empty id
// binds nothing: the delivery rides to the retry, which carries it again.
func TestClaudeLinkEmptyIDCarriesToTheRetry(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	gen := r.beginLaunch("c")
	echo(t, r, "c", gen, claudeDelivery{rowID: "row-A", text: "alpha"})

	r.bind("c", gen, "")
	r.bind("c", gen, "msg_retry")
	ids, _ := r.link("c", "msg_retry", []linkUnit{{text: "alpha"}})
	ck.EqDeep([]string{"row-A"}, ids, "the retry's capture names it")
}

// A bind from a retired launch's hook binds nothing in the current one.
func TestClaudeLinkStaleLaunchBindsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	r := &claudeDeliveryRegistry{}
	old := r.beginLaunch("c")
	gen := r.beginLaunch("c")
	echo(t, r, "c", gen, claudeDelivery{rowID: "row-A", text: "alpha"})

	r.bind("c", old, "msg_1")
	ids, _ := r.link("c", "msg_1", []linkUnit{{text: "alpha"}})
	ck.EqDeep([]string{""}, ids, "a stale launch's frame bound nothing")
	ck.Eq(1, r.acceptedCount("c"), "the delivery is still waiting")
}

// An image-only send has no text to compare, so it matches on its image count —
// and a differing count is not a near miss, it is a different message.
func TestClaudeLinkImageOnlySend(t *testing.T) {
	ck := assert.NewCollecting(t)

	one := &claudeDeliveryRegistry{}
	seedBound(t, one, "c", "msg_1", claudeDelivery{rowID: "row-A", images: 2})
	ids, logs := one.link("c", "msg_1", []linkUnit{{images: 2}})
	ck.EqDeep([]string{"row-A"}, ids, "the image count matched")
	ck.Empty(logs, "an exact pairing logs nothing")

	two := &claudeDeliveryRegistry{}
	seedBound(t, two, "c", "msg_1",
		claudeDelivery{rowID: "row-B", images: 2},
		claudeDelivery{rowID: "row-C", images: 2},
	)
	ids, logs = two.link("c", "msg_1", []linkUnit{{images: 1}})
	ck.EqDeep([]string{""}, ids, "a differing image count links nothing")
	ck.Require().Len(logs, 3, "logs")
	ck.Eq("captured user message matched no accepted delivery", logs[0].msg, "log message")
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
	gen := seedBound(t, exited, "c", "msg_1", claudeDelivery{rowID: "row-A", text: "hello"})
	inst := &child.Child{}
	exited.attach(inst, gen)
	exited.endLaunch("c", inst)
	ids, _ := exited.link("c", "msg_1", units)
	ck.EqDeep([]string{""}, ids, "endLaunch retired the accepted delivery")
	ck.Eq(0, exited.acceptedCount("c"), "queue after endLaunch")

	resumed := &claudeDeliveryRegistry{}
	seedBound(t, resumed, "c", "msg_1", claudeDelivery{rowID: "row-A", text: "hello"})
	resumed.beginLaunch("c")
	ids, _ = resumed.link("c", "msg_1", units)
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
		seedBound(t, &h.c.claudeDeliveries, "c_1", "msg_1", claudeDelivery{rowID: rowID, text: text})
	}

	accept("row-1", "m0")
	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1",
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
		Session: "c_1", ThreadID: "th", ConversationID: "conv-th", ResponseMessageID: "msg_9",
		Rows: []*conv.Node{capRow(0, "user", now, "")},
	})
	h.drain()

	ck.Require().Len(h.got, 1, "a subagent thread still publishes its row")
	ck.Eq("", h.got[0].GetUserMessage().GetMessageId(), "a subagent thread never links")
	ck.Eq(1, h.c.claudeDeliveries.acceptedCount("c_1"), "the subagent report consumed nothing")

	h.log, h.got = nil, nil
	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1", ReleaseOnly: true,
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
	seedBound(t, r, "c", "msg_2", claudeDelivery{rowID: "row-also", text: "Reply with exactly one word: also"})
	ids, logs := r.link("c", "msg_2", []linkUnit{unit})

	ck.EqDeep([]string{"row-also"}, ids, "the recorded relay links to the row that carried it")
	ck.Len(logs, 0, "an exact match warns about nothing")
	ck.Eq(0, r.acceptedCount("c"), "the linked delivery left the queue")
}
