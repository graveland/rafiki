package claudecode_test

import (
	"strings"
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

// relayIn is relayAlone with the user's words swapped for text, so a fixture
// differs from a recorded one only in the rule under test.
func relayIn(text string) string {
	return strings.Replace(relayAlone, relayedText, text, 1)
}

// TestSplitRelaysRecordedToolResult pins the recorded shape: the tool output
// stays the tool result's text, the user's message comes out beside it, and the
// input is not written to.
func TestSplitRelaysRecordedToolResult(t *testing.T) {
	c := assert.NewAborting(t)

	in := relayToolResult(relayRecorded)
	want := relayToolResult(relayRecorded) // built again, sharing no slice with in

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

// TestSplitRelaysTakesTheLastOccurrence pins the position rules a relay is read
// by: the LAST relay in the text, the LAST trailer, the LAST content block of a
// tool result, and one trailing newline tolerated after the close tag.
func TestSplitRelaysTakesTheLastOccurrence(t *testing.T) {
	c := assert.NewAborting(t)

	// An earlier relay followed by the final one: only the final one is the
	// user's message, and everything before it — the earlier relay too — is
	// what is left in place.
	got, relays := claudecode.Harness{}.SplitRelays(relayToolResult(relayAlone + "\n\n" + relayIn("a second message")))
	tr, ok := got.Content[0].(msg.ToolResult)
	c.Require().True(ok, "two relays: block 0 is %T, want msg.ToolResult", got.Content[0])
	c.Require().Len(tr.Content, 1, "two relays: the text block stays")
	c.Eq(relayAlone, tr.Content[0].(msg.Text).Text, "two relays: everything before the last one is left")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: "a second message"}}, relays, "two relays: the last one is the user's")

	// A user message that quotes the trailer line earlier: the LAST trailer is
	// the one that ends it, so the quote stays part of the message.
	quoted := "here is what it says:\n\nThis is how Claude Code surfaces messages the user sends mid-turn — within the running turn, often alongside the next tool result, rather than as a separate conversation turn. Address the message above as you continue this turn.\n\nand now my message"
	got, relays = claudecode.Harness{}.SplitRelays(relayToolResult(relayIn(quoted)))
	tr, ok = got.Content[0].(msg.ToolResult)
	c.Require().True(ok, "quoted trailer: block 0 is %T, want msg.ToolResult", got.Content[0])
	c.Len(tr.Content, 0, "quoted trailer: nothing is left before the relay")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: quoted}}, relays, "quoted trailer: the quoted line stays in the message")

	// The relay sits in a tool result's LAST content block, not its first.
	got, relays = claudecode.Harness{}.SplitRelays(msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{
			msg.Text{Text: "earlier output"},
			msg.Text{Text: relayRecorded},
		}},
	}})
	tr, ok = got.Content[0].(msg.ToolResult)
	c.Require().True(ok, "last block: block 0 is %T, want msg.ToolResult", got.Content[0])
	c.Require().Len(tr.Content, 2, "last block: both content blocks stay")
	c.Eq("earlier output", tr.Content[0].(msg.Text).Text, "last block: the earlier block is untouched")
	c.Eq("(Bash completed with no output)", tr.Content[1].(msg.Text).Text, "last block: the later block is what is left")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: relayedText}}, relays, "last block: the last block is the one relayed")

	// One newline after the close tag is tolerated.
	got, relays = claudecode.Harness{}.SplitRelays(relayToolResult(relayAlone + "\n"))
	tr, ok = got.Content[0].(msg.ToolResult)
	c.Require().True(ok, "trailing newline: block 0 is %T, want msg.ToolResult", got.Content[0])
	c.Len(tr.Content, 0, "trailing newline: nothing is left before the relay")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: relayedText}}, relays, "trailing newline: the relay is found")
}

// TestSplitRelaysNearMissesUntouched pins what is still not a relay: no words of
// the user's, no reminder around the phrase, or not a user message. Framing is
// tolerated (TestSplitRelaysToleratesVariantFraming); a missing message is not.
func TestSplitRelaysNearMissesUntouched(t *testing.T) {
	c := assert.NewAborting(t)

	cases := map[string]func() msg.Message{
		"ordinary reminder": func() msg.Message {
			return msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: reminder}}}
		},
		"empty user text": func() msg.Message {
			return relayToolResult("<system-reminder>\nThe user sent a new message while you were working:\n  \n\nThis is how Claude Code surfaces messages the user sends mid-turn\n</system-reminder>")
		},
		"phrase outside a reminder": func() msg.Message {
			return relayToolResult("The user sent a new message while you were working:\nalso")
		},
		"relay not in the tool result's last block": func() msg.Message {
			return msg.Message{Role: msg.User, Content: []msg.Block{
				msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{
					msg.Text{Text: relayRecorded},
					msg.Image{MediaType: "image/png", Data: []byte{1, 2, 3}},
				}},
			}}
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

// TestSplitRelaysToleratesVariantFraming pins that the relay is found by its
// phrase, so a reworded trailer, a missing one, attributes on the tag or prose
// around it still surface the user's words.
func TestSplitRelaysToleratesVariantFraming(t *testing.T) {
	c := assert.NewAborting(t)

	cases := map[string]struct{ in, left string }{
		"missing trailer": {
			"<system-reminder>\nThe user sent a new message while you were working:\nalso\n</system-reminder>", ""},
		"missing close tag": {
			"<system-reminder>\nThe user sent a new message while you were working:\nalso\n\nThis is how Claude Code surfaces messages the user sends mid-turn, and more", ""},
		"tag attributes": {
			"<system-reminder kind=\"x\">\nThe user sent a new message while you were working:\nalso\n</system-reminder>", ""},
		"prose after": {
			"out\n\n" + relayIn("also") + "\n\nprose after the relay", "out\n\nprose after the relay"},
	}
	for name, tc := range cases {
		got, relays := claudecode.Harness{}.SplitRelays(relayToolResult(tc.in))
		c.EqDeep([]claudecode.Relayed{{Index: 0, Text: "also"}}, relays, "%s: the user's words come out", name)
		tr := got.Content[0].(msg.ToolResult)
		if tc.left == "" {
			c.Len(tr.Content, 0, "%s: nothing is left", name)
			continue
		}
		c.Require().Len(tr.Content, 1, "%s: the rest stays", name)
		c.Eq(tc.left, tr.Content[0].(msg.Text).Text, "%s: what is left", name)
	}

	// A trailer this code does not recognise is indistinguishable from the
	// user's own words, so it stays in the message rather than being guessed at.
	_, rw := claudecode.Harness{}.SplitRelays(relayToolResult("<system-reminder>\nThe user sent a new message while you were working:\nalso\n\nPlease address it.\n</system-reminder>"))
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: "also\n\nPlease address it."}}, rw, "reworded trailer: kept with the message")

	got, relays := claudecode.Harness{}.SplitRelays(msg.Message{Role: msg.User, Content: []msg.Block{
		msg.Text{Text: "earlier line\n\n" + relayAlone},
	}})
	c.Require().Len(got.Content, 1, "top-level prose before the relay stays")
	c.Eq("earlier line", got.Content[0].(msg.Text).Text, "top-level: the prose is left")
	c.EqDeep([]claudecode.Relayed{{Index: 0, Text: relayedText}}, relays, "top-level: the relay comes out beside it")
}
