package main

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/multigres/testkit/assert"
)

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
