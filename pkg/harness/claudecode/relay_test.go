package claudecode_test

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/harness/claudecode"
	"go.graveland.dev/rafiki/pkg/msg"
)

// relayRecorded is the exact text Claude Code 2.1.290 put in a tool result when
// the user sent a message mid-turn (testdata/claude-capture-delivery,
// 000002-request.http).
const relayRecorded = "(Bash completed with no output)\n\n<system-reminder>\nThe user sent a new message while you were working:\nReply with exactly one word: also\n\nThis is how Claude Code surfaces messages the user sends mid-turn — within the running turn, often alongside the next tool result, rather than as a separate conversation turn. Address the message above as you continue this turn.\n</system-reminder>"

// relayAlone is the same relay with no tool output before it.
const relayAlone = "<system-reminder>\nThe user sent a new message while you were working:\nReply with exactly one word: also\n\nThis is how Claude Code surfaces messages the user sends mid-turn — within the running turn, often alongside the next tool result, rather than as a separate conversation turn. Address the message above as you continue this turn.\n</system-reminder>"

// relayedText is what the user actually sent.
const relayedText = "Reply with exactly one word: also"

// relayToolResult builds a user message whose one block is a tool result
// carrying text as its single content block.
func relayToolResult(text string) msg.Message {
	return msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: text}}},
	}}
}

// TestSplitRelaysRecordedToolResult pins the recorded shape: the tool output
// stays the tool result's text, the user's message comes out beside it, and the
// input is not written to.
func TestSplitRelaysRecordedToolResult(t *testing.T) {
	c := assert.NewAborting(t)

	in := relayToolResult(relayRecorded)
	want := in // an independent copy built the same way: the input must survive

	got, relays := claudecode.Harness{}.SplitRelays(in)

	c.EqDeep(want, in, "SplitRelays writes nothing to its input")
	c.Require().Len(got.Content, 1, "the tool result block stays")
	tr, ok := got.Content[0].(msg.ToolResult)
	c.Require().True(ok, "block 0 is %T, want msg.ToolResult", got.Content[0])
	c.Require().Len(tr.Content, 1, "the tool result keeps one text block")
	c.Eq("(Bash completed with no output)", tr.Content[0].(msg.Text).Text, "the tool output is what is left")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: relayedText}}, relays, "the user's message is relayed")
}

// TestSplitRelaysWholeBlockRelayDropsTheBlock pins a relay that is all a tool
// result carried: the user's message comes out and no empty text is left.
func TestSplitRelaysWholeBlockRelayDropsTheBlock(t *testing.T) {
	c := assert.NewAborting(t)

	got, relays := claudecode.Harness{}.SplitRelays(relayToolResult(relayAlone))

	c.Require().Len(got.Content, 1, "the tool result block stays")
	tr, ok := got.Content[0].(msg.ToolResult)
	c.Require().True(ok, "block 0 is %T, want msg.ToolResult", got.Content[0])
	c.Len(tr.Content, 0, "a tool result that was only the relay leaves nothing behind")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: relayedText}}, relays, "the user's message is relayed")
}

// TestSplitRelaysTopLevelBlock pins the same relay arriving as a top-level
// block: the block becomes the user's own text in place, and is not injected.
func TestSplitRelaysTopLevelBlock(t *testing.T) {
	c := assert.NewAborting(t)

	got, relays := claudecode.Harness{}.SplitRelays(msg.Message{
		Role: msg.User, Content: []msg.Block{msg.Text{Text: relayAlone}},
	})

	c.Require().Len(got.Content, 1, "the block keeps its place")
	txt, ok := got.Content[0].(msg.Text)
	c.Require().True(ok, "block 0 is %T, want msg.Text", got.Content[0])
	c.Eq(relayedText, txt.Text, "the block is the user's own message")
	c.False(txt.Injected, "the user's own message is not injected")
	c.Len(relays, 0, "a top-level relay needs no block of its own")
}

// TestSplitRelaysNearMissesUntouched pins that only the exact shape is a relay:
// every near-miss is left for the ordinary reminder handling, and is neither
// guessed at nor truncated.
func TestSplitRelaysNearMissesUntouched(t *testing.T) {
	c := assert.NewAborting(t)

	cases := map[string]func() msg.Message{
		"missing trailer": func() msg.Message {
			return relayToolResult("<system-reminder>\nThe user sent a new message while you were working:\nalso\n</system-reminder>")
		},
		"missing close tag": func() msg.Message {
			return relayToolResult("<system-reminder>\nThe user sent a new message while you were working:\nalso\n\nThis is how Claude Code surfaces messages the user sends mid-turn, and then some trailing prose with no closing reminder tag at all")
		},
		"ordinary reminder": func() msg.Message {
			return msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: reminder}}}
		},
		"nested tag in the body": func() msg.Message {
			return relayToolResult("<system-reminder>\nThe user sent a new message while you were working:\nalso\n<system-reminder>nested</system-reminder>\n\nThis is how Claude Code surfaces messages the user sends mid-turn\n</system-reminder>")
		},
		"empty user text": func() msg.Message {
			return relayToolResult("<system-reminder>\nThe user sent a new message while you were working:\n  \n\nThis is how Claude Code surfaces messages the user sends mid-turn\n</system-reminder>")
		},
		"relay not at the end": func() msg.Message {
			return relayToolResult(relayAlone + "\n\nprose after the relay")
		},
		"assistant role": func() msg.Message {
			return msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: relayAlone}}}
		},
	}

	for name, build := range cases {
		want := build()
		got, relays := claudecode.Harness{}.SplitRelays(build())
		c.EqDeep(want, got, "%s: the message is left untouched", name)
		c.Len(relays, 0, "%s: nothing is relayed", name)
	}
}
