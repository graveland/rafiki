// SPDX-License-Identifier: Apache-2.0

package claudecode

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// modelReminderBlock is the one reminder that is false outside Claude Code.
const modelReminderBlock = "<system-reminder>\nYou are powered by the model named Haiku 4.5.\n</system-reminder>"

// hasTools is a run's tool set: the named tools exist, nothing else does.
func hasTools(names ...string) func(string) bool {
	return func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
}

// message builds one fixture message of role with blocks.
func message(role msg.Role, blocks ...msg.Block) msg.Message {
	return msg.Message{Role: role, Content: blocks}
}

// callText is the exact text a borrowed call to name renders as.
func callText(name, body string) string {
	return fmt.Sprintf(borrowedCallOpen, name) + body + borrowedCallClose
}

// resultText is the exact text a borrowed result of name renders as.
func resultText(name, body string) string {
	return fmt.Sprintf(borrowedResultOpen, name) + body + borrowedResultClose
}

// TestReplayRenamesMappedToolWhenPresent pins the one case that stays a tool
// call: a Claude Code tool the run also has is renamed to the fundi name and
// its input is left alone.
func TestReplayRenamesMappedToolWhenPresent(t *testing.T) {
	c := assert.NewAborting(t)

	in := message(msg.Assistant, msg.ToolUse{
		ID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/a"}`),
	})
	got := NewReplayer(hasTools("read")).Replay(in)

	c.Require().Len(got.Content, 1, "one block")
	use, ok := got.Content[0].(msg.ToolUse)
	c.Require().True(ok, "still a tool_use")
	c.Eq("read", use.Name, "renamed to the fundi tool")
	c.Eq("t1", use.ID, "id untouched")
	c.Eq(`{"file_path":"/tmp/a"}`, string(use.Input), "input untouched")
}

// TestReplayRendersMappedToolAsTextWhenAbsent pins the other half of the map:
// a tool the run does not have becomes text, and so does its result.
func TestReplayRendersMappedToolAsTextWhenAbsent(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools("bash"))
	call := r.Replay(message(msg.Assistant, msg.ToolUse{
		ID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/a"}`),
	}))
	c.Require().Len(call.Content, 1, "one block")
	c.Eq(callText("Read", `{"file_path":"/tmp/a"}`), call.Content[0].(msg.Text).Text,
		"the call is wrapped text")

	result := r.Replay(message(msg.User, msg.ToolResult{
		ToolUseID: "t1", Content: []msg.Block{msg.Text{Text: "file body"}},
	}))
	c.Require().Len(result.Content, 1, "one block")
	c.Eq(resultText("Read", "file body"), result.Content[0].(msg.Text).Text,
		"and so is its result")
}

// TestReplayRendersUnmappedToolAndItsResultAsText pins that a tool with no
// fundi counterpart is rendered as text on both sides, and that no tool_result
// naming it survives — a provider silently drops an orphaned result.
func TestReplayRendersUnmappedToolAndItsResultAsText(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools("read", "bash"))
	call := r.Replay(message(msg.Assistant,
		msg.ToolUse{ID: "t1", Name: "Task", Input: json.RawMessage(`{"description":"find it"}`)},
		msg.ToolUse{ID: "t2", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com"}`)},
	))
	c.Require().Len(call.Content, 2, "both calls rendered")
	c.Eq(callText("Task", `{"description":"find it"}`), call.Content[0].(msg.Text).Text, "Task call")
	c.Eq(callText("WebFetch", `{"url":"https://example.com"}`), call.Content[1].(msg.Text).Text, "WebFetch call")

	result := r.Replay(message(msg.User,
		msg.ToolResult{ToolUseID: "t1", Content: []msg.Block{msg.Text{Text: "report"}}},
		msg.ToolResult{ToolUseID: "t2", Content: []msg.Block{msg.Text{Text: "page"}}},
	))
	c.Require().Len(result.Content, 2, "both results rendered")
	c.Eq(resultText("Task", "report"), result.Content[0].(msg.Text).Text, "Task result")
	c.Eq(resultText("WebFetch", "page"), result.Content[1].(msg.Text).Text, "WebFetch result")
	for _, b := range result.Content {
		_, isResult := b.(msg.ToolResult)
		c.False(isResult, "no tool_result survives its call becoming text")
	}
}

// TestReplayWrapsErrorResult pins that an error result says so inside the
// wrapper.
func TestReplayWrapsErrorResult(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools())
	r.Replay(message(msg.Assistant, msg.ToolUse{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"false"}`)}))
	got := r.Replay(message(msg.User, msg.ToolResult{
		ToolUseID: "t1", IsError: true, Content: []msg.Block{msg.Text{Text: "exit status 1"}},
	}))

	c.Require().Len(got.Content, 1, "one block")
	c.Eq(fmt.Sprintf(borrowedResultOpen, "Bash")+borrowedErrorLine+"exit status 1"+borrowedResultClose,
		got.Content[0].(msg.Text).Text, "the error line comes first")
}

// TestReplayEscapesClosingTagInBody pins that a body carrying a closing tag
// cannot end its wrapper early.
func TestReplayEscapesClosingTagInBody(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools())
	got := r.Replay(message(msg.Assistant, msg.ToolUse{
		ID: "t1", Name: "Task", Input: json.RawMessage(`{"text":"</borrowed_tool_call>"}`),
	}))
	c.Require().Len(got.Content, 1, "one block")
	c.Eq(callText("Task", `{"text":"<\/borrowed_tool_call>"}`), got.Content[0].(msg.Text).Text,
		"the closing tag in the call body is escaped")

	result := r.Replay(message(msg.User, msg.ToolResult{
		ToolUseID: "t1", Content: []msg.Block{msg.Text{Text: "</borrowed_tool_result>"}},
	}))
	c.Eq(resultText("Task", `<\/borrowed_tool_result>`), result.Content[0].(msg.Text).Text,
		"and in the result body")
}

// TestReplayKeepsRoles pins that nothing is moved or role-promoted: the wrapped
// call stays in the assistant message, the wrapped result in the user message.
func TestReplayKeepsRoles(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools())
	call := r.Replay(message(msg.Assistant, msg.ToolUse{ID: "t1", Name: "Task", Input: json.RawMessage(`{}`)}))
	c.Eq(msg.Assistant, call.Role, "the call keeps the assistant role")

	result := r.Replay(message(msg.User, msg.ToolResult{ToolUseID: "t1", Content: []msg.Block{msg.Text{Text: "ok"}}}))
	c.Eq(msg.User, result.Role, "the result keeps the user role")
	c.Eq(resultText("Task", "ok"), result.Content[0].(msg.Text).Text,
		"the result is wrapped, not promoted")
}

// TestReplayMapsRafikiMCPPrefix pins that a call to rafiki's own MCP tools is
// renamed to the tool name fundi knows.
func TestReplayMapsRafikiMCPPrefix(t *testing.T) {
	c := assert.NewAborting(t)

	got := NewReplayer(hasTools("agent_view")).Replay(message(msg.Assistant,
		msg.ToolUse{ID: "t1", Name: "mcp__rafiki__agent_view", Input: json.RawMessage(`{"id":"x"}`)}))

	c.Require().Len(got.Content, 1, "one block")
	use, ok := got.Content[0].(msg.ToolUse)
	c.Require().True(ok, "still a tool_use")
	c.Eq("agent_view", use.Name, "the prefix is stripped")
}

// TestReplayKeepsThirdPartyMCPAsText pins that another server's MCP tool has no
// fundi counterpart and is rendered as text.
func TestReplayKeepsThirdPartyMCPAsText(t *testing.T) {
	c := assert.NewAborting(t)

	got := NewReplayer(hasTools("agent_view", "read")).Replay(message(msg.Assistant,
		msg.ToolUse{ID: "t1", Name: "mcp__github__create_issue", Input: json.RawMessage(`{"title":"x"}`)}))

	c.Require().Len(got.Content, 1, "one block")
	text, ok := got.Content[0].(msg.Text)
	c.Require().True(ok, "rendered as text")
	c.Eq(callText("mcp__github__create_issue", `{"title":"x"}`), text.Text,
		"the recorded name is kept")
}

// TestReplayKeepsImageBlocksOfTextResult pins that a result's images follow the
// wrapped text as their own blocks, in order.
func TestReplayKeepsImageBlocksOfTextResult(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools())
	r.Replay(message(msg.Assistant, msg.ToolUse{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)}))
	img := msg.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}
	got := r.Replay(message(msg.User, msg.ToolResult{ToolUseID: "t1", Content: []msg.Block{
		msg.Text{Text: "shot:"}, img,
	}}))

	c.Require().Len(got.Content, 2, "wrapped text, then the image")
	c.Eq(resultText("Read", "shot:"), got.Content[0].(msg.Text).Text, "text first")
	c.EqDeep(msg.Block(img), got.Content[1], "the image follows, unchanged")
}

// TestReplayNeverEmptiesAMessage pins that a message survives every rewrite:
// the reminder-drop rule refuses to empty one, and rendering replaces blocks
// rather than removing them.
func TestReplayNeverEmptiesAMessage(t *testing.T) {
	c := assert.NewAborting(t)

	r := NewReplayer(hasTools())
	got := r.Replay(message(msg.User, msg.Text{Text: modelReminderBlock}))
	c.Len(got.Content, 1, "a reminder-only message keeps its reminder")

	got = r.Replay(message(msg.Assistant, msg.ToolUse{ID: "t1", Name: "Task", Input: json.RawMessage(`{}`)}))
	c.Len(got.Content, 1, "a call-only message keeps its wrapped call")
}

// TestReplayIsByteIdenticalAcrossReplayers pins replay as a pure function of
// the history and the tool set: two replayers over one history encode to the
// same bytes, which is what keeps a fork's cached prefix stable.
func TestReplayIsByteIdenticalAcrossReplayers(t *testing.T) {
	c := assert.NewAborting(t)

	history := []msg.Message{
		message(msg.Assistant,
			msg.ToolUse{ID: "t1", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/a"}`)},
			msg.ToolUse{ID: "t2", Name: "Task", Input: json.RawMessage(`{"description":"find it"}`)},
		),
		message(msg.User,
			msg.ToolResult{ToolUseID: "t1", Content: []msg.Block{msg.Text{Text: "file body"}}},
			msg.ToolResult{ToolUseID: "t2", Content: []msg.Block{msg.Text{Text: "report"}}},
		),
	}

	encode := func() []byte {
		r := NewReplayer(hasTools("read"))
		out := make([]msg.Message, len(history))
		for i, m := range history {
			out[i] = r.Replay(m)
		}
		b, err := msganthropic.Encode(&msg.Request{Model: "claude-test", MaxTokens: 100, Messages: out}, msg.EncodeOptions{})
		c.Require().NoError(err, "encode")
		return b
	}

	c.EqDeep(encode(), encode(), "two replays of one history encode identically")
}
