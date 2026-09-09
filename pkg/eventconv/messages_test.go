package eventconv_test

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"
)

func TestEventsFromMessagesCarriesOrdinal(t *testing.T) {
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("hi")), StopReason: "end_turn"},
	}

	evs := eventconv.EventsFromMessages("c_test", msgs)

	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].GetOrdinal() != 0 {
		t.Fatalf("event 0 ordinal = %d, want 0", evs[0].GetOrdinal())
	}
	if evs[1].GetOrdinal() != 1 {
		t.Fatalf("event 1 ordinal = %d, want 1", evs[1].GetOrdinal())
	}
	if evs[0].ChildId != "c_test" {
		t.Fatalf("child id = %q, want c_test", evs[0].ChildId)
	}
	if evs[0].GetUserMessage() == nil {
		t.Fatal("event 0 is not a user message")
	}
	if evs[1].GetAssistantMessage() == nil {
		t.Fatal("event 1 is not an assistant message")
	}
	if evs[1].GetAssistantMessage().StopReason != rafikiv1.StopReason_STOP_REASON_END_TURN {
		t.Fatalf("stop reason = %v, want STOP_REASON_END_TURN", evs[1].GetAssistantMessage().StopReason)
	}
	if evs[1].GetAssistantMessage().RawStopReason != "end_turn" {
		t.Fatalf("raw stop reason = %q, want %q", evs[1].GetAssistantMessage().RawStopReason, "end_turn")
	}
}

func TestBlocksFromParamPreservesToolUseInputAsRawJSON(t *testing.T) {
	p := anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock("tu_1", map[string]any{"path": "/tmp/x"}, "read"),
	)

	blocks := eventconv.BlocksFromParam(p)

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	tu := blocks[0].GetToolUse()
	if tu == nil {
		t.Fatal("block is not a tool_use")
	}
	if tu.Id != "tu_1" {
		t.Fatalf("id = %q, want tu_1", tu.Id)
	}
	if tu.Name != "read" {
		t.Fatalf("name = %q, want read", tu.Name)
	}
	if tu.InputJson == "" {
		t.Fatal("input_json is empty; it must carry the raw arguments JSON")
	}
}

func TestBlocksAreIndexedMonotonically(t *testing.T) {
	p := anthropic.NewAssistantMessage(
		anthropic.NewTextBlock("one"),
		anthropic.NewTextBlock("two"),
		anthropic.NewTextBlock("three"),
	)

	blocks := eventconv.BlocksFromParam(p)

	for i, b := range blocks {
		if b.Index != int32(i) {
			t.Fatalf("block %d has index %d, want %d", i, b.Index, i)
		}
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
		if got := eventconv.StopReasonFromString(in); got != want {
			t.Errorf("StopReasonFromString(%q) = %v, want %v", in, got, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A kind='compaction_summary' row maps to a CompactionBoundary event, not to
// the ordinary UserMessage branch its role would otherwise land in. Only
// pre_tokens is ever set here — a reattach-synthesized boundary cannot know
// the post size — which is what makes the renderer pick the one-sided format.
func TestEventsFromMessagesMapsCompactionSummaryRow(t *testing.T) {
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("old context"))},
		{Ordinal: 1, Kind: ptr("compaction_summary"), InputTokens: ptr(182000),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("summary"))},
	}

	evs := eventconv.EventsFromMessages("c_test", msgs)

	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if evs[0].GetUserMessage() == nil {
		t.Fatalf("event 0 is %T, want an ordinary user message", evs[0].Payload)
	}
	ev := evs[1]
	if ev.GetOrdinal() != 1 {
		t.Fatalf("event 1 ordinal = %d, want 1", ev.GetOrdinal())
	}
	cb := ev.GetCompactionBoundary()
	if cb == nil {
		t.Fatalf("event 1 is %T, want Event_CompactionBoundary", ev.Payload)
	}
	if cb.GetPreTokens() != 182000 || cb.PreTokens == nil {
		t.Fatalf("pre_tokens = %v, want 182000", cb.PreTokens)
	}
	if cb.PostTokens != nil {
		t.Fatalf("post_tokens = %v, want unset (the renderer's one-sided format relies on it)", cb.PostTokens)
	}
	if cb.Trigger != "" {
		t.Fatalf("trigger = %q, want empty (the stored row carries no trigger)", cb.Trigger)
	}
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
	if cb == nil {
		t.Fatalf("event is %T, want Event_CompactionBoundary", ev.Payload)
	}
	if cb.PreTokens != nil || cb.PostTokens != nil {
		t.Fatalf("pre_tokens=%v post_tokens=%v, want both unset", cb.PreTokens, cb.PostTokens)
	}
}
