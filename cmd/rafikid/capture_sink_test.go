package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// captureHarness records, in one slice, every event the controller publishes
// and every call the child's gate receives, so a test can assert their
// relative order. The bus delivers synchronously into a buffered channel, so
// draining it on each gate call yields everything published before that call.
type captureHarness struct {
	c      *Controller
	events <-chan *rafikiv1.Event
	log    []string
	got    []*rafikiv1.Event
	gate   map[string]heldEventReleaser
	looked []string
}

func (h *captureHarness) drain() {
	for {
		select {
		case ev := <-h.events:
			h.got = append(h.got, ev)
			h.log = append(h.log, eventLabel(ev))
		default:
			return
		}
	}
}

func eventLabel(ev *rafikiv1.Event) string {
	switch p := ev.GetPayload().(type) {
	case *rafikiv1.Event_UserMessage:
		return fmt.Sprintf("user:%d", ev.GetOrdinal())
	case *rafikiv1.Event_AssistantMessage:
		return fmt.Sprintf("assistant:%d", ev.GetOrdinal())
	case *rafikiv1.Event_Error:
		return "error:" + p.Error.GetCode()
	}
	return "other"
}

type recordingGate struct {
	h *captureHarness
}

func (g recordingGate) MessagePersisted(id string) {
	g.h.drain()
	g.h.log = append(g.h.log, "persisted:"+id)
}

func (g recordingGate) ReleaseHeldEvents() {
	g.h.drain()
	g.h.log = append(g.h.log, "released")
}

func newCaptureHarness(t *testing.T) *captureHarness {
	t.Helper()
	reg := nativebus.New()
	ch, cancel := reg.SubscribeAll()
	t.Cleanup(cancel)
	h := &captureHarness{
		c:      &Controller{st: childstore.New(), cm: newChildManager(), native: reg},
		events: ch,
		gate:   map[string]heldEventReleaser{},
	}
	h.c.gateLookup = func(id string) (heldEventReleaser, bool) {
		h.looked = append(h.looked, id)
		g, ok := h.gate[id]
		return g, ok
	}
	h.c.st.Insert(&childstore.Session{ChildID: "c_1", Kind: protocol.KindClaude, Status: protocol.StatusIdle})
	h.gate["c_1"] = recordingGate{h: h}
	return h
}

func capRow(ordinal int, role string, at time.Time, stop string) store.Message {
	m := store.Message{
		Ordinal:    ordinal,
		Param:      anthropic.MessageParam{Role: anthropic.MessageParamRole(role), Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(fmt.Sprintf("m%d", ordinal))}},
		StopReason: stop,
		CreatedAt:  at,
	}
	return m
}

func TestCaptureReportPublishesRowsWithRowTs(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	t1 := time.Date(2026, 10, 25, 1, 2, 3, 4000, time.UTC)
	t2 := t1.Add(3 * time.Second)

	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1",
		Rows: []store.Message{capRow(0, "user", t1, ""), capRow(1, "assistant", t2, "end_turn")},
	})
	h.drain()

	ck.Require().Eq(2, len(h.got), "events")
	ck.Eq("user:0", eventLabel(h.got[0]), "first")
	ck.Eq("assistant:1", eventLabel(h.got[1]), "second")
	ck.True(h.got[0].GetTs().AsTime().Equal(t1), "user ts %v want %v", h.got[0].GetTs().AsTime(), t1)
	ck.True(h.got[1].GetTs().AsTime().Equal(t2), "assistant ts %v want %v", h.got[1].GetTs().AsTime(), t2)
	ck.Eq("end_turn", h.got[1].GetAssistantMessage().GetRawStopReason(), "raw stop reason")
}

func TestCaptureReportDedupesByOrdinal(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	now := time.Now()

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv",
		Rows: []store.Message{capRow(3, "user", now, ""), capRow(4, "assistant", now, "end_turn")}})
	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv",
		Rows: []store.Message{capRow(4, "assistant", now, "end_turn"), capRow(5, "user", now, "")}})
	h.drain()

	var labels []string
	for _, ev := range h.got {
		labels = append(labels, eventLabel(ev))
	}
	ck.EqDeep([]string{"user:3", "assistant:4", "user:5"}, labels, "published once each")
}

func TestCaptureReportPublishesNoBoundaryForCompactionSummary(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	now := time.Now()
	summary := capRow(1, "user", now, "")
	kind := store.KindCompactionSummary
	summary.Kind = &kind

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv",
		Rows: []store.Message{capRow(0, "user", now, ""), summary, capRow(2, "assistant", now, "end_turn")}})
	h.drain()

	var labels []string
	for _, ev := range h.got {
		labels = append(labels, eventLabel(ev))
	}
	ck.EqDeep([]string{"user:0", "assistant:2"}, labels, "no boundary from the summary row")
}

func TestCaptureReportErrPublishesErrorAfterRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	now := time.Now()

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1",
		Err:  "insert failed",
		Rows: []store.Message{capRow(0, "user", now, "")}})
	h.drain()

	ck.EqDeep([]string{"user:0", "error:capture_not_persisted", "released"}, h.log, "order")
	ck.Eq("turn not persisted: insert failed", h.got[1].GetError().GetMessage(), "message")
}

func TestCaptureReportEmptyResponseIDReleasesGate(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv"})
	h.drain()

	ck.EqDeep([]string{"released"}, h.log, "gate calls")
}

func TestCaptureReportMarksResponsePersisted(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	now := time.Now()

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1",
		Rows: []store.Message{capRow(0, "user", now, ""), capRow(1, "assistant", now, "end_turn")}})
	h.drain()

	ck.EqDeep([]string{"user:0", "assistant:1", "persisted:msg_1"}, h.log, "row events precede the gate call")
}

func TestCaptureReportThreadRoutesToThreadChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	ck.Require().NoError(h.c.EnsureThreadChild("c_1", "th", "conv-th"), "EnsureThreadChild")
	tid := threadChildID("c_1", "th")
	h.drain()
	h.log, h.got = nil, nil

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ThreadID: "th", ConversationID: "conv-th", ResponseMessageID: "msg_9",
		Rows: []store.Message{capRow(0, "user", time.Now(), "")}})
	h.drain()

	ck.Require().Eq(1, len(h.got), "events")
	ck.Eq(tid, h.got[0].GetChildId(), "child id")
	ck.Eq(0, len(h.looked), "gateLookup calls")
}

func TestCaptureReportUnknownChildDropped(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)

	h.c.CaptureReported(server.CaptureReport{Session: "launcher", ConversationID: "conv", ResponseMessageID: "msg_1",
		Rows: []store.Message{capRow(0, "user", time.Now(), "")}})
	h.drain()

	ck.Eq(0, len(h.got), "events")
	ck.Eq(0, len(h.looked), "gateLookup calls")
}

func TestCaptureReportTypedNilChildIsNotAGate(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: nativebus.New()}
	c.st.Insert(&childstore.Session{ChildID: "c_1", Kind: protocol.KindClaude, Status: protocol.StatusIdle})

	c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1"})

	_, ok := c.gateFor("c_1")
	ck.False(ok, "no live child means no gate")
}

// The contract: a claude message event's ts is the stored row's created_at,
// the same value GetHistory reports.
func TestClaudeAssistantMessageTsIsRowCreatedAt(t *testing.T) {
	assertPublishedTsMatchesHistory(t, "assistant")
}

func TestClaudeUserMessageTsIsRowCreatedAt(t *testing.T) {
	assertPublishedTsMatchesHistory(t, "user")
}

func assertPublishedTsMatchesHistory(t *testing.T, role string) {
	t.Helper()
	ck := assert.NewCollecting(t)
	pool := openTestPool(t)
	ctx := t.Context()
	cs := capture.NewCaptureStore(pool)

	convID, err := cs.EnsureConversation(ctx, capture.ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	ck.Require().NoError(err, "EnsureConversation")
	req := []byte(`{"messages":[{"role":"user","content":"hello contract"}]}`)
	turnID, createdAt, err := cs.InsertTurnIntent(ctx, capture.TurnIntent{ConversationID: convID, Model: "m", Request: req})
	ck.Require().NoError(err, "InsertTurnIntent")
	next, err := cs.DecomposeRequest(ctx, convID, turnID, createdAt, req, "")
	ck.Require().NoError(err, "DecomposeRequest")
	canonical := []byte(`{"role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn"}`)
	ck.Require().NoError(cs.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 10, 5, "end_turn"), "AppendResponseMessage")

	rows, err := cs.MessagesSince(ctx, convID, createdAt)
	ck.Require().NoError(err, "MessagesSince")

	h := newCaptureHarness(t)
	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: convID, ResponseMessageID: "msg_1", Rows: rows})
	h.drain()

	page, err := store.NewMessages(pool).LoadPage(ctx, convID, store.HistoryPage{})
	ck.Require().NoError(err, "LoadPage")
	history := eventconv.EventsFromMessages("c_1", page)

	want := func(evs []*rafikiv1.Event) *timestamppb.Timestamp {
		for _, ev := range evs {
			if (ev.GetAssistantMessage() != nil) == (role == "assistant") && (ev.GetAssistantMessage() != nil || ev.GetUserMessage() != nil) {
				return ev.GetTs()
			}
		}
		return nil
	}
	pub, hist := want(h.got), want(history)
	ck.Require().NotNil(pub, "no published %s event", role)
	ck.Require().NotNil(hist, "no history %s event", role)
	ck.True(pub.AsTime().Equal(hist.AsTime()), "published ts %v != history ts %v", pub.AsTime(), hist.AsTime())
}

// A release-only report — a session request that declares no client tools, so
// it is not the agent's own conversation — publishes NOTHING, but it must still
// release the gate: the response it names is what a frame on the child's stdout
// armed the gate behind, and dropping this report is what left that child's
// event stream silent (TurnEnd included) until it exited.
func TestCaptureReportReleaseOnlyReleasesTheGateAndPublishesNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)
	now := time.Now()

	h.c.CaptureReported(server.CaptureReport{
		Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1", ReleaseOnly: true,
		Rows: []store.Message{capRow(0, "user", now, ""), capRow(1, "assistant", now, "end_turn")},
	})
	h.drain()

	ck.Eq(0, len(h.got), "a release-only report publishes no rows")
	ck.EqDeep([]string{"persisted:msg_1"}, h.log, "gate calls")

	// Its rows must not advance the ordinal mark either: a release-only report
	// says nothing about what this conversation has published.
	h.log, h.got = nil, nil
	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv", ResponseMessageID: "msg_1",
		Rows: []store.Message{capRow(0, "user", now, ""), capRow(1, "assistant", now, "end_turn")}})
	h.drain()

	ck.EqDeep([]string{"user:0", "assistant:1", "persisted:msg_1"}, h.log, "the mark was not advanced")
}

// A release-only report that stored no response names nothing to release, and
// flushing the whole gate would publish the agent's own held turn events out of
// order behind a helper's failure. It leaves the gate alone; the gate's stall
// release is the backstop.
func TestCaptureReportReleaseOnlyWithoutResponseNeverFlushesHeldEvents(t *testing.T) {
	ck := assert.NewCollecting(t)
	h := newCaptureHarness(t)

	h.c.CaptureReported(server.CaptureReport{Session: "c_1", ConversationID: "conv",
		ReleaseOnly: true, Err: "insert failed"})
	h.drain()

	ck.Eq(0, len(h.got), "events: a helper's failure is not the agent's turn")
	ck.Eq(0, len(h.log), "gate calls")
}
