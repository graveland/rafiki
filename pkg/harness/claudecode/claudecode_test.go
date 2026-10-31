package claudecode_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/harness/claudecode"
	"go.graveland.dev/rafiki/pkg/msg"
)

const (
	// reminder is an exact reminder block: the wrapper, a one-line body, the
	// closing tag.
	reminder = "<system-reminder>\nx\n</system-reminder>"
	// modelReminder is the one reminder that is false outside Claude Code.
	modelReminder = "<system-reminder>\nYou are powered by the model named Haiku 4.5. The exact model ID is claude-haiku-4-5-20251001. Assistant knowledge cutoff is February 2025.\n</system-reminder>"
	// userText is a block the user actually wrote.
	userText = "Reply with exactly one word: ready"
)

func harness() claudecode.Harness { return claudecode.Harness{} }

// TestMarkExactForms pins the recognition rule: only the exact wrapper with a
// body that carries no tag of its own is a reminder. A near-miss is shown as
// user text, never lost.
func TestMarkExactForms(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{name: "exact", text: reminder, want: true},
		{name: "one trailing newline", text: reminder + "\n", want: true},
		{name: "two trailing newlines", text: reminder + "\n\n"},
		{name: "leading space", text: " " + reminder},
		{name: "missing open", text: "x\n</system-reminder>"},
		{name: "missing close", text: "<system-reminder>\nx"},
		{name: "nested open tag in body", text: "<system-reminder>\n<system-reminder>\nx\n</system-reminder>"},
		{name: "nested close tag in body", text: "<system-reminder>\nx</system-reminder>\n</system-reminder>"},
		{
			name: "reminder, user text, reminder",
			text: "<system-reminder>\nA\n</system-reminder>\nREAL\n<system-reminder>\nB\n</system-reminder>",
		},
		{name: "plain user text", text: userText},
	} {
		got := harness().Mark(msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: tc.text}}})
		block := got.Content[0].(msg.Text)
		c.Eq(tc.want, block.Injected, "%s: %q", tc.name, tc.text)
		c.Eq(tc.text, block.Text, "%s: Mark must keep the text verbatim", tc.name)
	}
}

// TestMarkKeepsOrderAndText pins that marking moves nothing: the same blocks
// come back in the same order with byte-identical text.
func TestMarkKeepsOrderAndText(t *testing.T) {
	c := assert.NewAborting(t)

	in := msg.Message{Role: msg.User, Content: []msg.Block{
		msg.Text{Text: reminder},
		msg.Text{Text: userText},
		msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: "result"}}},
		msg.Text{Text: reminder + "\n"},
	}}

	got := harness().Mark(in)

	c.Len(got.Content, 4, "nothing may be dropped")
	for i, want := range []string{reminder, userText, "", reminder + "\n"} {
		if want == "" {
			c.True(got.Content[i].(msg.ToolResult).ToolUseID == "toolu_1", "the tool result stays at %d", i)
			continue
		}
		c.Eq(want, got.Content[i].(msg.Text).Text, "block %d text", i)
	}
	for _, i := range []int{0, 3} {
		c.True(got.Content[i].(msg.Text).Injected, "block %d is a reminder", i)
	}
	c.False(got.Content[1].(msg.Text).Injected, "user text is not injected")
}

// TestMarkIgnoresToolResultContent pins that a tool result's own content is
// never inspected: the user wrote it as far as this harness knows.
func TestMarkIgnoresToolResultContent(t *testing.T) {
	c := assert.NewAborting(t)

	in := msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: "toolu_1", Content: []msg.Block{msg.Text{Text: reminder}}},
	}}

	got := harness().Mark(in)

	inner := got.Content[0].(msg.ToolResult).Content[0].(msg.Text)
	c.False(inner.Injected, "tool_result content is never inspected")
	c.Eq(reminder, inner.Text, "tool_result content is kept verbatim")
}

// TestMarkIgnoresAssistant pins that only a user message is scanned: an
// assistant message is model output, whatever it says.
func TestMarkIgnoresAssistant(t *testing.T) {
	c := assert.NewAborting(t)

	in := msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: reminder}}}

	got := harness().Mark(in)

	c.False(got.Content[0].(msg.Text).Injected, "an assistant message is never marked")
}

// TestMarkDoesNotMutateInput pins that Mark writes nothing through its
// argument: a node's Msg is shared by the cache.
func TestMarkDoesNotMutateInput(t *testing.T) {
	c := assert.NewAborting(t)

	in := msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: reminder}, msg.Text{Text: userText}}}

	got := harness().Mark(in)

	c.False(in.Content[0].(msg.Text).Injected, "Mark must not mark the caller's block")
	c.Eq(userText, in.Content[1].(msg.Text).Text, "Mark must not touch the caller's blocks")
	c.True(&in.Content[0] != &got.Content[0], "Mark must return a new Content slice")
	c.True(got.Content[0].(msg.Text).Injected, "the returned block is marked")

	got.Content[0] = msg.Text{Text: "clobbered"}
	c.Eq(reminder, in.Content[0].(msg.Text).Text, "writing the result must not reach the input")
}

// fixtureMessage returns message ordinal 0 of the recorded capture, so the
// fixture-shape tests run against the real reminder texts.
func fixtureMessage(t *testing.T) msg.Message {
	t.Helper()
	c := assert.NewAborting(t)

	raw, err := os.ReadFile("../../capture/testdata/interim-capture-rows.json")
	c.NoError(err, "read the capture fixture")

	var doc struct {
		Messages []struct {
			Ordinal int             `json:"ordinal"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(raw, &doc), "decode the capture fixture")

	for _, m := range doc.Messages {
		if m.Ordinal != 0 {
			continue
		}
		c.Eq("user", m.Role, "message ordinal 0 is a user message")
		var blocks []struct {
			Text string `json:"text"`
			Type string `json:"type"`
		}
		c.NoError(json.Unmarshal(m.Content, &blocks), "decode ordinal 0's content")
		out := msg.Message{Role: msg.User}
		for _, b := range blocks {
			c.Eq("text", b.Type, "ordinal 0 is all text blocks")
			out.Content = append(out.Content, msg.Text{Text: b.Text})
		}
		return out
	}
	c.Fail("the capture fixture has no message ordinal 0")
	return msg.Message{}
}

// TestForeignReplayDropsModelReminder pins the one reminder that is false
// outside Claude Code, on the recorded fixture shape: env, model, email, date,
// user text.
func TestForeignReplayDropsModelReminder(t *testing.T) {
	c := assert.NewAborting(t)

	in := fixtureMessage(t)
	c.Len(in.Content, 5, "the fixture is five blocks")

	got := harness().ForeignReplay(in)

	c.Len(got.Content, 4, "the model reminder is dropped")
	for i, want := range []int{0, 2, 3} {
		block := got.Content[i].(msg.Text)
		c.Eq(in.Content[want].(msg.Text).Text, block.Text, "block %d is fixture block %d", i, want)
		c.True(block.Injected, "block %d is a reminder", i)
	}
	last := got.Content[3].(msg.Text)
	c.Eq(userText, last.Text, "the user's own text is last")
	c.False(last.Injected, "the user's own text is not injected")
}

// TestForeignReplayNeverEmptiesAMessage pins that dropping is bounded: a
// message that is only the model reminder keeps it, marked.
func TestForeignReplayNeverEmptiesAMessage(t *testing.T) {
	c := assert.NewAborting(t)

	got := harness().ForeignReplay(msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: modelReminder}}})

	c.Len(got.Content, 1, "a message is never emptied")
	c.Eq(modelReminder, got.Content[0].(msg.Text).Text, "the block is kept verbatim")
	c.True(got.Content[0].(msg.Text).Injected, "the kept block is marked")
}

// TestUnwrap pins display: a marked block shows its body, anything else its
// text.
func TestUnwrap(t *testing.T) {
	c := assert.NewAborting(t)

	c.Eq("x", harness().Unwrap(msg.Text{Text: reminder, Injected: true}), "exact form")
	c.Eq("x", harness().Unwrap(msg.Text{Text: reminder + "\n", Injected: true}), "trailing newline form")
	c.Eq(userText, harness().Unwrap(msg.Text{Text: userText}), "non-reminder text is unchanged")
	c.Eq(" "+reminder, harness().Unwrap(msg.Text{Text: " " + reminder}), "a near-miss is unchanged")
}
