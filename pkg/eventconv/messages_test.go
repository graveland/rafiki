package eventconv_test

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func TestEventsFromMessagesCarriesOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("hi")), StopReason: "end_turn"},
	}

	evs := eventconv.EventsFromMessages("c_test", msgs)

	c.Len(evs, 2, "got %d events, want 2", len(evs))
	c.Eq(0, evs[0].GetOrdinal(), "event 0 ordinal")
	c.Eq(1, evs[1].GetOrdinal(), "event 1 ordinal")
	c.Eq("c_test", evs[0].ChildId, "child id")
	c.NotNil(evs[0].GetUserMessage(), "event 0 is not a user message")
	c.NotNil(evs[1].GetAssistantMessage(), "event 1 is not an assistant message")
	c.Eq(rafikiv1.StopReason_STOP_REASON_END_TURN, evs[1].GetAssistantMessage().StopReason, "stop reason")
	c.Eq("end_turn", evs[1].GetAssistantMessage().RawStopReason, "raw stop reason")
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

// A kind='compaction_summary' row maps to a CompactionBoundary event, not to
// the ordinary UserMessage branch its role would otherwise land in. Only
// pre_tokens is ever set here — a reattach-synthesized boundary cannot know
// the post size — which is what makes the renderer pick the one-sided format.
func TestEventsFromMessagesMapsCompactionSummaryRow(t *testing.T) {
	c := assert.NewAborting(t)
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("old context"))},
		{Ordinal: 1, Kind: ptr("compaction_summary"), InputTokens: ptr(182000),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("summary"))},
	}

	evs := eventconv.EventsFromMessages("c_test", msgs)

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

// A summary row with no recorded input_tokens still becomes a boundary, with
// pre_tokens unset rather than a zero that would read as "compaction dropped
// everything".
func TestEventsFromMessagesCompactionSummaryWithoutTokens(t *testing.T) {
	msgs := []store.Message{
		{Ordinal: 4, Kind: ptr("compaction_summary"),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("summary"))},
	}

	ev := eventconv.EventsFromMessages("c_test", msgs)[0]

	cb := ev.GetCompactionBoundary()
	assert.NewAborting(t).NotNil(cb, "event is %T, want Event_CompactionBoundary", ev.Payload)
	if cb.PreTokens != nil || cb.PostTokens != nil {
		t.Fatalf("pre_tokens=%v post_tokens=%v, want both unset", cb.PreTokens, cb.PostTokens)
	}
}

// kind='compaction_tail' rows are stored duplicates of rows that already sit
// before the compaction horizon, so the full-history reader must drop them
// entirely — no event, no ordinal — leaving the summary's boundary and the
// pre-horizon messages behind.
func TestCompactionEventsFromMessagesSkipsTail(t *testing.T) {
	c := assert.NewAborting(t)
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("hi")), StopReason: "end_turn"},
		{Ordinal: 2, Kind: ptr("compaction_summary"), InputTokens: ptr(900),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("summary"))},
		{Ordinal: 3, Kind: ptr("compaction_tail"),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
		{Ordinal: 4, Kind: ptr("compaction_tail"),
			Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("hi")), StopReason: "end_turn"},
	}

	evs := eventconv.EventsFromMessages("c_test", msgs)

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
