// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
)

func turnStartEvent(childID string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: childID, Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}}
}

func deltaEvent(childID, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: childID, Payload: &rafikiv1.Event_ContentBlockDelta{
		ContentBlockDelta: &rafikiv1.ContentBlockDelta{
			Delta: &rafikiv1.ContentBlockDelta_Text{Text: text},
		},
	}}
}

func assistantEvent(childID, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: childID, Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{
			Content: []*rafikiv1.ContentBlock{{
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
			}},
			RawStopReason: "end_turn",
		},
	}}
}

func turnEndEvent(childID string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: childID, Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}}
}

// The streamed fragments and the committed message are the SAME content, and
// they must not both survive: the deltas build the block a viewer watches grow,
// and the assistant message is that block's final, authoritative form.
func TestStreamedDeltasAreReplacedByTheCommittedMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c1")

	s.Apply(turnStartEvent("c1"))
	s.Apply(deltaEvent("c1", "Hel"))
	s.Apply(deltaEvent("c1", "lo"))
	c.Eq("Hello", s.Blocks[0].Text, "text while streaming")

	s.Apply(assistantEvent("c1", "Hello, world"))
	s.Apply(turnEndEvent("c1"))

	c.Len(s.Blocks, 1, "blocks (deltas + the committed message must not double up): %+v", s.Blocks)
	b := s.Blocks[0]
	c.False(b.Kind != session.KindAssistant || b.Text != "Hello, world" || !b.Final,
		"block = %+v, want KindAssistant text=%q final=true", b, "Hello, world")
}

// A turn that never streams (a claude child, whose message arrives from its
// captured row) still opens a placeholder at TurnStart. The committed message
// replaces it rather than leaving an empty block ahead of the real one.
func TestCommittedMessageReplacesTheTurnStartPlaceholder(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c1")

	s.Apply(turnStartEvent("c1"))
	s.Apply(assistantEvent("c1", "the answer"))

	c.Len(s.Blocks, 1, "blocks = %+v, want the placeholder replaced, not appended to", s.Blocks)
	c.Eq("the answer", s.Blocks[0].Text, "text")
}

// An agentic turn makes one LLM call per tool round, and each call's message
// commits before the next one streams. The second call's fragments therefore
// belong to a block of their OWN: appending them to the finalized first block
// would splice the calls together.
func TestASecondCallInTheSameTurnStreamsIntoItsOwnBlock(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c1")

	s.Apply(turnStartEvent("c1"))
	s.Apply(deltaEvent("c1", "first"))
	s.Apply(assistantEvent("c1", "first"))
	s.Apply(deltaEvent("c1", "second"))
	s.Apply(assistantEvent("c1", "second"))
	s.Apply(turnEndEvent("c1"))

	c.Len(s.Blocks, 2, "blocks = %+v, want one per LLM call", s.Blocks)
	c.Eq("first", s.Blocks[0].Text, "first call")
	c.Eq("second", s.Blocks[1].Text, "second call")
}

// A steer's echo is a user message published MID-turn, so it lands in the
// transcript while the assistant message it interrupted is still streaming.
// The committed message still COMPLETES that assistant block: keying the
// replace on "the very last block" would append a second assistant block and
// draw the reply twice.
func TestASteerEchoMidStreamDoesNotDuplicateTheReply(t *testing.T) {
	c := assert.NewCollecting(t)
	s := session.New("c1")

	s.Apply(turnStartEvent("c1"))
	s.Apply(deltaEvent("c1", "before the steer"))
	s.Apply(textEvent("c1", "hold on")) // the steer's own echo
	s.Apply(assistantEvent("c1", "before the steer"))
	s.Apply(turnEndEvent("c1"))

	c.Len(s.Blocks, 2, "blocks = %+v, want the streamed block completed in place, not a second one", s.Blocks)
	b := s.Blocks[0]
	c.False(b.Kind != session.KindAssistant || b.Text != "before the steer" || !b.Final,
		"block = %+v, want KindAssistant text=%q final=true", b, "before the steer")
	c.False(s.Blocks[1].Kind != session.KindUser || s.Blocks[1].Text != "hold on",
		"the steer echo must keep its place after the assistant block: %+v", s.Blocks[1])
}

// TurnStart opens its placeholder when the prompt is delivered, but the user
// message is published afterwards (claude: from the captured row, once the
// response is in; fundi: once the prompt's row is inserted). The prompt must
// still sit ABOVE the reply it started, so the empty placeholder has to stay
// the last block rather than be left behind by the user's block.
func TestUserMessageLandsAheadOfTheTurnStartPlaceholder(t *testing.T) {
	c := assert.NewCollecting(t)
	for name, reply := range map[string]func(*session.Session){
		"committed message only": func(s *session.Session) {
			s.Apply(assistantEvent("c1", "the answer"))
		},
		"streamed then committed": func(s *session.Session) {
			s.Apply(deltaEvent("c1", "the ans"))
			s.Apply(assistantEvent("c1", "the answer"))
		},
	} {
		s := session.New("c1")
		s.Apply(turnStartEvent("c1"))
		s.Apply(textEvent("c1", "the question"))
		reply(s)
		s.Apply(turnEndEvent("c1"))

		c.Require().Len(s.Blocks, 2, "%s: blocks = %+v", name, s.Blocks)
		c.Eq(session.KindUser, s.Blocks[0].Kind, "%s: first block", name)
		c.Eq("the question", s.Blocks[0].Text, "%s: prompt", name)
		c.Eq(session.KindAssistant, s.Blocks[1].Kind, "%s: second block", name)
		c.Eq("the answer", s.Blocks[1].Text, "%s: reply", name)
	}
}
