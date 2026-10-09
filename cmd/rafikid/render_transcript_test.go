package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/multigres/testkit/assert"
)

func ptr[T any](v T) *T { return &v }

// TestRenderTranscriptLimitCountsEntries pins that agent_view's limit counts
// rendered entries, not frames: DBToPiFrames ends every transcript with an
// agent_end frame that renders nothing, so a frame limit of 1 rendered
// "(no transcript yet)" for a child with a full transcript.
func TestRenderTranscriptLimitCountsEntries(t *testing.T) {
	ck := assert.NewCollecting(t)
	frames := fundi.DBToPiFrames([]anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("first prompt")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("first answer")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("second prompt")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("second answer")),
	})

	ck.Eq("[3 earlier entries omitted]\nassistant: second answer\n",
		renderTranscript(frames, 1, viewMaxBytes), "limit 1 must show the newest entry")
	ck.Eq("[2 earlier entries omitted]\nuser: second prompt\nassistant: second answer\n",
		renderTranscript(frames, 2, viewMaxBytes), "limit 2 must show the newest two entries")

	all := renderTranscript(frames, 0, viewMaxBytes)
	ck.False(strings.Contains(all, "omitted"), "limit 0 must render everything; got %q", all)
	ck.StrContains(all, "user: first prompt", "limit 0 must keep the oldest entry")
}

// TestRenderTranscriptRendersCompactionBoundary pins that the boundary divider
// pkg/fundi.DBToPiFramesMessages emits for a stored compaction_summary row
// renders as a readable divider line, not silently dropped.
func TestRenderTranscriptRendersCompactionBoundary(t *testing.T) {
	ck := assert.NewCollecting(t)
	frames := fundi.DBToPiFramesMessages([]store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("first prompt"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("first answer"))},
		{Ordinal: 2, Kind: ptr(store.KindCompactionSummary), InputTokens: ptr(150_000),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("summary text"))},
		{Ordinal: 3, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("second prompt"))},
	})
	out := renderTranscript(frames, 0, viewMaxBytes)
	ck.StrContains(out, "context compacted", "the compaction boundary must render as a divider")
	ck.StrContains(out, "150k tokens", "the divider must carry the replaced-token figure")
	ck.False(strings.Contains(out, "user: summary text"), "the summary row must not render as a plain user message; got %q", out)
}
