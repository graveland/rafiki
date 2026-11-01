// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func turnStartFor(id string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}}
}

func deltaFor(id, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_ContentBlockDelta{
		ContentBlockDelta: &rafikiv1.ContentBlockDelta{
			Delta: &rafikiv1.ContentBlockDelta_Text{Text: text},
		},
	}}
}

func assistantMessageFor(id, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{
			Content: []*rafikiv1.ContentBlock{{
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
			RawStopReason: "end_turn",
		},
	}}
}

// A streamed reply must appear in the pane AS IT ARRIVES, not once its message
// commits. The deltas are ephemeral (never logged, never replayed), so nothing
// but the live focus stream delivers them — and until fundi published them, the
// pane showed nothing for the whole reply and then the finished paragraph at
// once.
func TestStreamedDeltasRenderBeforeTheMessageCommits(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("c_1")
	defer c.shutdown()
	c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	c.Update(seedMsg{children: []*rafikiv1.ChildSummary{summaryFor("c_1", "one", 0)}})
	c.Update(historyMsg{childID: "c_1", after: 0, events: nil}) // opens c_1's focus stream

	c.Update(eventMsg{evs: []*rafikiv1.Event{turnStartFor("c_1")}})
	c.Update(eventMsg{evs: []*rafikiv1.Event{deltaFor("c_1", "first half ")}})
	ck.StrContains(ansi.Strip(c.View().Content), "first half", "the first fragment did not render while it was still streaming:\n")

	c.Update(eventMsg{evs: []*rafikiv1.Event{deltaFor("c_1", "second half")}})
	ck.StrContains(ansi.Strip(c.View().Content), "first half second half", "fragments do not accumulate in the pane:\n")

	// The committed message is the same content in its final form. It replaces
	// the streamed block; drawing both would show the reply twice.
	c.Update(eventMsg{evs: []*rafikiv1.Event{assistantMessageFor("c_1", "first half second half")}})
	ck.Eq(1, strings.Count(ansi.Strip(c.View().Content), "first half second half"),
		"the reply is drawn once, from the committed message:\n%s", c.View().Content)
}
