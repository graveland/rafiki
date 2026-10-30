package eventconv_test

import (
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// node is one transcript node carrying a single text block.
func node(ordinal int, role msg.Role, text string) *conv.Node {
	return &conv.Node{
		Ref: conv.Ref{Ordinal: ordinal},
		Msg: msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: text}}},
	}
}

func TestEventsFromNodesCarriesOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	nodes := []*conv.Node{
		node(0, msg.User, "hello"),
		{Ref: conv.Ref{Ordinal: 1}, Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "hi"}}}, StopReason: "end_turn"},
	}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
	c.Eq("c_test", evs[0].ChildId, "child id")
	c.NotNil(evs[0].GetUserMessage(), "event 0 is not a user message")
	c.NotNil(evs[1].GetAssistantMessage(), "event 1 is not an assistant message")
	c.Eq(rafikiv1.StopReason_STOP_REASON_END_TURN, evs[1].GetAssistantMessage().StopReason, "stop reason")
	c.Eq("end_turn", evs[1].GetAssistantMessage().RawStopReason, "raw stop reason")
}

// A stored system row is a message the harness injected, not something the user
// typed, so it gets its own event rather than a user turn's. A user row still
// lands on UserMessage.
func TestEventsFromNodesRendersSystemRole(t *testing.T) {
	c := assert.NewAborting(t)
	nodes := []*conv.Node{
		node(0, msg.System, "Environment update: /tmp is now writable"),
		node(1, msg.User, "carry on"),
	}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	sys := evs[0].GetSystemMessage()
	c.NotNil(sys, "event 0 is %T, want Event_SystemMessage", evs[0].Payload)
	c.Nil(evs[0].GetUserMessage(), "event 0 must not be a user message")
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Len(sys.GetContent(), 1, "system content blocks")
	c.Eq("Environment update: /tmp is now writable", sys.GetContent()[0].GetText().GetText(), "system text")
	user := evs[1].GetUserMessage()
	c.NotNil(user, "event 1 is %T, want Event_UserMessage", evs[1].Payload)
	c.Nil(evs[1].GetSystemMessage(), "event 1 must not be a system message")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
}

// Each event carries its node's created_at as ts. A node with no recorded
// created_at leaves ts unset rather than stamping the zero time, which would
// read as "year 1" and sort before every real event.
func TestEventsFromNodesCarriesTimestamp(t *testing.T) {
	c := assert.NewAborting(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := t0.Add(90 * time.Second)
	first := node(0, msg.User, "first")
	first.CreatedAt = t0
	second := node(1, msg.Assistant, "second")
	second.CreatedAt = t1
	nodes := []*conv.Node{first, second, node(2, msg.User, "undated")}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 3, "got %d events, want 3", len(evs))
	c.NotNil(evs[0].GetTs(), "event 0 ts is nil")
	c.NotNil(evs[1].GetTs(), "event 1 ts is nil")
	c.Eq(t0, evs[0].GetTs().AsTime().UTC(), "event 0 ts")
	c.Eq(t1, evs[1].GetTs().AsTime().UTC(), "event 1 ts")
	c.Nil(evs[2].GetTs(), "event 2 has a zero CreatedAt, want ts nil")
}

func TestBlocksFromParamPreservesToolUseInputAsRawJSON(t *testing.T) {
	c := assert.NewAborting(t)
	p := anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock("tu_1", map[string]any{"path": "/tmp/x"}, "read"),
	)

	blocks := eventconv.BlocksFromParam(p)

	c.Len(blocks, 1, "got %d blocks, want 1", len(blocks))
	tu := blocks[0].GetToolUse()
	c.NotNil(tu, "block is not a tool_use")
	c.Eq("tu_1", tu.Id, "id")
	c.Eq("read", tu.Name, "name")
	c.NotEq("", tu.InputJson, "input_json is empty; it must carry the raw arguments JSON")
}

func TestBlocksAreIndexedMonotonically(t *testing.T) {
	p := anthropic.NewAssistantMessage(
		anthropic.NewTextBlock("one"),
		anthropic.NewTextBlock("two"),
		anthropic.NewTextBlock("three"),
	)

	blocks := eventconv.BlocksFromParam(p)

	for i, b := range blocks {
		assert.NewAborting(t).Eq(int32(i), b.Index, "block %d has index %d, want %d", i, b.Index, i)
	}
}

func TestStopReasonNormalizes(t *testing.T) {
	cases := map[string]rafikiv1.StopReason{
		"end_turn":   rafikiv1.StopReason_STOP_REASON_END_TURN,
		"max_tokens": rafikiv1.StopReason_STOP_REASON_MAX_TOKENS,
		"tool_use":   rafikiv1.StopReason_STOP_REASON_TOOL_USE,
		"stop":       rafikiv1.StopReason_STOP_REASON_END_TURN,
		"length":     rafikiv1.StopReason_STOP_REASON_MAX_TOKENS,
		"tool_calls": rafikiv1.StopReason_STOP_REASON_TOOL_USE,
		"":           rafikiv1.StopReason_STOP_REASON_UNSPECIFIED,
		"who_knows":  rafikiv1.StopReason_STOP_REASON_UNSPECIFIED,
	}
	for in, want := range cases {
		got := eventconv.StopReasonFromString(in)
		assert.NewCollecting(t).Eq(want, got, "StopReasonFromString(%q) = %v, want", in, got)
	}
}

func ptr[T any](v T) *T { return &v }

// A kind='compaction_summary' node maps to a CompactionBoundary event, not to
// the ordinary UserMessage branch its role would otherwise land in. Only
// pre_tokens is ever set here — a reattach-synthesized boundary cannot know
// the post size — which is what makes the renderer pick the one-sided format.
func TestEventsFromNodesMapsCompactionSummaryNode(t *testing.T) {
	c := assert.NewAborting(t)
	boundary := node(1, msg.User, "summary")
	boundary.Kind = conv.KindCompactionSummary
	boundary.InputTokens = ptr(int64(182000))
	nodes := []*conv.Node{node(0, msg.User, "old context"), boundary}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	c.NotNil(evs[0].GetUserMessage(), "event 0 is %T, want an ordinary user message", evs[0].Payload)
	ev := evs[1]
	c.Eq(1, ev.GetOrdinal(), "event 1 ordinal")
	cb := ev.GetCompactionBoundary()
	c.NotNil(cb, "event 1 is %T, want Event_CompactionBoundary", ev.Payload)
	if cb.GetPreTokens() != 182000 || cb.PreTokens == nil {
		t.Fatalf("pre_tokens = %v, want 182000", cb.PreTokens)
	}
	c.Nil(cb.PostTokens, "post_tokens")
	c.Eq("", cb.Trigger, "trigger")
}

// A summary node with no recorded input_tokens still becomes a boundary, with
// pre_tokens unset rather than a zero that would read as "compaction dropped
// everything".
func TestEventsFromNodesCompactionSummaryWithoutTokens(t *testing.T) {
	boundary := node(4, msg.User, "summary")
	boundary.Kind = conv.KindCompactionSummary

	ev := eventconv.EventsFromNodes("c_test", []*conv.Node{boundary})[0]

	cb := ev.GetCompactionBoundary()
	assert.NewAborting(t).NotNil(cb, "event is %T, want Event_CompactionBoundary", ev.Payload)
	if cb.PreTokens != nil || cb.PostTokens != nil {
		t.Fatalf("pre_tokens=%v post_tokens=%v, want both unset", cb.PreTokens, cb.PostTokens)
	}
}

// kind='compaction_tail' nodes are copies of nodes that already sit before the
// compaction horizon, so the full-history reader must drop them entirely — no
// event, no ordinal — leaving the summary's boundary and the pre-horizon
// messages behind. conv.Store.Page already drops them; this pins the renderer's
// own rule so a caller handing it a raw walk cannot leak the copies.
func TestCompactionEventsFromNodesSkipsTail(t *testing.T) {
	c := assert.NewAborting(t)
	summary := node(2, msg.User, "summary")
	summary.Kind = conv.KindCompactionSummary
	summary.InputTokens = ptr(int64(900))
	tailOne := node(3, msg.User, "hello")
	tailOne.Kind = conv.KindCompactionTail
	tailTwo := node(4, msg.Assistant, "hi")
	tailTwo.Kind = conv.KindCompactionTail
	nodes := []*conv.Node{
		node(0, msg.User, "hello"),
		{Ref: conv.Ref{Ordinal: 1}, Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "hi"}}}, StopReason: "end_turn"},
		summary,
		tailOne,
		tailTwo,
	}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Len(evs, 3, "got %d events, want 3", len(evs))
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
	c.Eq(2, evs[2].GetOrdinal(), "event 2 ordinal")
	c.NotNil(evs[0].GetUserMessage(), "event 0 is %T, want an ordinary user message", evs[0].Payload)
	c.NotNil(evs[1].GetAssistantMessage(), "event 1 is %T, want an ordinary assistant message", evs[1].Payload)
	cb := evs[2].GetCompactionBoundary()
	c.NotNil(cb, "event 2 is %T, want Event_CompactionBoundary", evs[2].Payload)
	if cb.PreTokens == nil || cb.GetPreTokens() != 900 {
		t.Fatalf("pre_tokens = %v, want 900", cb.PreTokens)
	}
}

func TestEventsFromNodesRendersFundiClearAsBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	clearBoundary := node(1, msg.User, store.ClearBoundaryText)
	clearBoundary.Kind = conv.KindClear
	// A claude clear row is the real first message of the new head.
	claudeClear := node(2, msg.User, "real head")
	claudeClear.Kind = conv.KindClear
	nodes := []*conv.Node{node(0, msg.User, "before"), clearBoundary, claudeClear}

	evs := eventconv.EventsFromNodes("c_test", nodes)

	c.Require().Len(evs, 3, "events")
	c.NotNil(evs[0].GetUserMessage(), "ordinary row")
	c.Eq("clear", evs[1].GetCompactionBoundary().GetTrigger(), "synthetic clear row renders as a divider")
	c.NotNil(evs[2].GetUserMessage(), "a claude clear row stays a message")
}
