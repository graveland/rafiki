package fundi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func TestDBToPiFrames_Empty(t *testing.T) {
	frames := DBToPiFrames(nil)
	// agent_start + agent_end (empty messages) = 2
	assert.NewAborting(t).Len(frames, 2, "got %d frames for empty input, want 2 (agent_start + agent_end)", len(frames))
	var hdr struct{ Type string }
	if err := json.Unmarshal(frames[0], &hdr); err != nil || hdr.Type != "agent_start" {
		t.Fatalf("first frame = %s, want agent_start", frames[0])
	}
	if err := json.Unmarshal(frames[1], &hdr); err != nil || hdr.Type != "agent_end" {
		t.Fatalf("last frame = %s, want agent_end", frames[1])
	}
}

func TestDBToPiFrames_UserText(t *testing.T) {
	c := assert.NewCollecting(t)
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(
			anthropic.NewTextBlock("hello world"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start(user) + message_end(user) + agent_end = 4
	c.Require().Len(frames, 4, "got %d frames, want 4", len(frames))
	// Verify the user message frames.
	for i := 1; i < 3; i++ {
		var env struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		c.Require().NoError(json.Unmarshal(frames[i], &env), "frame %d", i)
		c.Eq("user", env.Message.Role, "frame %d role = %q, want user", i, env.Message.Role)
		c.Eq("hello world", env.Message.Content, "frame %d content = %q, want 'hello world'", i, env.Message.Content)
	}
}

func TestDBToPiFrames_AssistantText(t *testing.T) {
	c := assert.NewCollecting(t)
	msgs := []anthropic.MessageParam{
		anthropic.NewAssistantMessage(
			anthropic.NewTextBlock("pong"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start + message_update + message_end + agent_end = 5
	c.Require().Len(frames, 5, "got %d frames, want 5", len(frames))
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	// frame 1: message_start
	c.Require().NoError(json.Unmarshal(frames[1], &env))
	c.Eq("assistant", env.Message.Role, "role")
	c.False(len(env.Message.Content) != 1 || env.Message.Content[0].Text != "pong", "content = %+v, want [{text pong}]", env.Message.Content)
	// frame 2: message_update
	var upd struct{ Type string }
	if err := json.Unmarshal(frames[2], &upd); err != nil || upd.Type != "message_update" {
		t.Errorf("frame 2 type = %s, want message_update", frames[2])
	}
	// frame 3: message_end
	var end struct{ Type string }
	if err := json.Unmarshal(frames[3], &end); err != nil || end.Type != "message_end" {
		t.Errorf("frame 3 type = %s, want message_end", frames[3])
	}
}

func TestDBToPiFrames_ToolUse(t *testing.T) {
	c := assert.NewCollecting(t)
	msgs := []anthropic.MessageParam{
		anthropic.NewAssistantMessage(
			anthropic.NewToolUseBlock("toolu_01", map[string]any{"file": "/tmp/x"}, "read"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start + message_update + message_end + agent_end = 5
	// (No tool_execution_start — there is no matching tool_result.)
	c.Require().Len(frames, 5, "got %d frames, want 5", len(frames))
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type      string         `json:"type"`
				ID        string         `json:"id"`
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"content"`
		} `json:"message"`
	}
	// frame 1: message_start carries the tool_use block
	c.Require().NoError(json.Unmarshal(frames[1], &env))
	b := env.Message.Content[0]
	c.Eq("toolCall", b.Type, "block type")
	c.Eq("toolu_01", b.ID, "id")
	c.Eq("read", b.Name, "name")
	c.False(b.Arguments["file"] != "/tmp/x", "arguments = %v", b.Arguments)
}

func TestDBToPiFrames_UserWithToolResult(t *testing.T) {
	c := assert.NewCollecting(t)
	isError := true
	msgs := []anthropic.MessageParam{
		anthropic.NewAssistantMessage(
			anthropic.NewToolUseBlock("toolu_01", map[string]any{"cmd": "ls"}, "bash"),
		),
		anthropic.NewUserMessage(
			anthropic.NewToolResultBlock("toolu_01", "file1.txt\nfile2.txt", isError),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start
	//   + message_start(assistant) + message_update + tool_execution_start + message_end
	//   + tool_execution_end
	//   + agent_end
	// = 7
	c.Require().Len(frames, 7, "got %d frames, want 7", len(frames))

	// Frame 3: tool_execution_start
	var start struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
	}
	c.Require().NoError(json.Unmarshal(frames[3], &start), "frame 3 (tool_execution_start)")
	c.Eq("tool_execution_start", start.Type, "frame 3 type")
	c.Eq("toolu_01", start.ToolCallID, "tool_execution_start toolCallId")
	c.Eq("bash", start.ToolName, "tool_execution_start toolName")

	// Frame 5: tool_execution_end
	var end struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    bool   `json:"isError"`
		Result     struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	c.Require().NoError(json.Unmarshal(frames[5], &end), "frame 5 (tool_execution_end)")
	c.Eq("tool_execution_end", end.Type, "frame 5 type")
	c.Eq("toolu_01", end.ToolCallID, "tool_execution_end toolCallId")
	c.True(end.IsError, "tool_execution_end isError = false, want true")
	c.False(len(end.Result.Content) != 1 || end.Result.Content[0].Text != "file1.txt\nfile2.txt", "tool_execution_end result = %+v, want {text 'file1.txt\\nfile2.txt'}", end.Result.Content)
}

func TestDBToPiFrames_Limit(t *testing.T) {
	// This test verifies that limit filtering is applied by the caller
	// (dbRecent), not by DBToPiFrames itself. DBToPiFrames returns
	// all frames; the caller slices.
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("msg1")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply1")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("msg2")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply2")),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + (2 user messages × 2 frames) + (2 assistant messages × 3 frames) + agent_end = 12
	assert.NewAborting(t).Len(frames, 12, "got %d frames, want 12 (agent_start + 2 user pairs + 2 assistant trios + agent_end)", len(frames))
}

func TestDBToPiFrames_AgentEndCarriesMessages(t *testing.T) {
	c := assert.NewCollecting(t)
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("hi there")),
	}
	frames := DBToPiFrames(msgs)
	// Last frame must be agent_end.
	var end struct {
		Type     string            `json:"type"`
		Messages []json.RawMessage `json:"messages"`
	}
	last := frames[len(frames)-1]
	c.Require().NoError(json.Unmarshal(last, &end), "unmarshal agent_end")
	c.Require().Eq("agent_end", end.Type, "last frame type")
	c.Require().Len(end.Messages, 2, "agent_end messages = %d, want 2 (user + assistant)", len(end.Messages))
	// Each message must be a valid Pi-format message (PiUserMessage, PiAssistantMessage).
	for i, m := range end.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		c.Require().NoError(json.Unmarshal(m, &msg), "message %d", i)
		c.NotEq("", msg.Role, "message %d: role empty", i)
	}
}

// TestDBToPiFramesMessages_SkipsTailAndRendersBoundary pins the full-history
// reader contract for the agent_view path: a compaction_tail copy must be
// dropped (the tail shows once, through its original row), and the
// compaction_summary row must render as a compaction_boundary divider rather
// than as a plain user prompt.
func TestDBToPiFramesMessages_SkipsTailAndRendersBoundary(t *testing.T) {
	c := assert.NewCollecting(t)
	kind := func(k string) *string { return &k }
	pre := 123
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("p1"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("a1"))},
		{Ordinal: 2, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("p2"))},
		{Ordinal: 3, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("a2"))},
		{Ordinal: 4, Kind: kind(store.KindCompactionSummary), InputTokens: &pre,
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("SUMMARY-TEXT"))},
		{Ordinal: 5, Kind: kind(store.KindCompactionTail),
			Param: anthropic.NewUserMessage(anthropic.NewTextBlock("p2"))},
		{Ordinal: 6, Kind: kind(store.KindCompactionTail),
			Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("a2"))},
	}

	var boundary, p2, summary int
	for _, f := range DBToPiFramesMessages(msgs) {
		var env struct {
			Type      string `json:"type"`
			PreTokens *int   `json:"preTokens"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		c.Require().NoError(json.Unmarshal(f, &env), "frame %s", f)
		switch env.Type {
		case "compaction_boundary":
			boundary++
			c.Require().NotNil(env.PreTokens, "boundary frame carries no preTokens")
			c.Eq(123, *env.PreTokens, "boundary preTokens")
		case "message_end":
			var text string
			if json.Unmarshal(env.Message.Content, &text) != nil {
				continue
			}
			switch text {
			case "p2":
				p2++
			case "SUMMARY-TEXT":
				summary++
			}
		}
	}
	c.Eq(1, boundary, "compaction_boundary frames")
	c.Eq(1, p2, "the tail marker must appear once (tail copy skipped, original kept)")
	c.Eq(0, summary, "the summary row must not render as a plain user message")
}

// TestAppendPiMsg_SkipsNilRawMessage guards the fix for appendPiMsg's dead
// nil check: a nil json.RawMessage (what mustFrame returns when marshaling
// fails) must be skipped, not appended as a literal JSON `null`.
// json.Marshal(json.RawMessage(nil)) returns the 4 bytes `null` with a nil
// error, which is why the old `b == nil` check after marshaling never fired.
func TestAppendPiMsg_SkipsNilRawMessage(t *testing.T) {
	c := assert.NewAborting(t)
	out := appendPiMsg(nil, json.RawMessage(nil))
	c.Empty(out, "got %d entries, want 0 (nil json.RawMessage must be skipped)", len(out))

	out = appendPiMsg(nil, json.RawMessage{})
	c.Empty(out, "got %d entries, want 0 (empty json.RawMessage must be skipped)", len(out))
}

// TestAppendPiMsg_AppendsNonNilValues is the positive-side discriminator for
// TestAppendPiMsg_SkipsNilRawMessage: both an already-marshaled
// json.RawMessage and a raw struct must still be appended, so the nil guard
// above cannot be satisfied by a change that skips everything.
func TestAppendPiMsg_AppendsNonNilValues(t *testing.T) {
	c := assert.NewCollecting(t)
	out := appendPiMsg(nil, json.RawMessage(`{"role":"user"}`))
	c.Require().Len(out, 1, "got %d entries, want 1 for a non-nil json.RawMessage", len(out))
	c.Eq(`{"role":"user"}`, string(out[0]), "entry = %s, want the original raw message unchanged", out[0])

	out = appendPiMsg(nil, struct {
		Role string `json:"role"`
	}{Role: "assistant"})
	c.Require().Len(out, 1, "got %d entries, want 1 for a plain struct", len(out))
	c.Eq(`{"role":"assistant"}`, string(out[0]), "entry = %s, want marshaled struct", out[0])
}

// A fundi /clear's synthetic boundary row renders as a divider, not as a user
// prompt carrying the boundary text.
func TestDBToPiFramesMessages_RendersClearAsBoundary(t *testing.T) {
	c := assert.NewCollecting(t)
	clear := store.KindClear
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("p1"))},
		{Ordinal: 1, Param: anthropic.NewAssistantMessage(anthropic.NewTextBlock("a1"))},
		{Ordinal: 2, Kind: &clear, Param: anthropic.NewUserMessage(anthropic.NewTextBlock(store.ClearBoundaryText))},
	}
	var boundary, leaked int
	for _, f := range DBToPiFramesMessages(msgs) {
		if strings.Contains(string(f), store.ClearBoundaryText) {
			leaked++
		}
		var env struct {
			Type string `json:"type"`
		}
		c.Require().NoError(json.Unmarshal(f, &env), "frame %s", f)
		if env.Type == "compaction_boundary" {
			boundary++
		}
	}
	c.Eq(1, boundary, "compaction_boundary frames")
	c.Eq(0, leaked, "the boundary text must not render as a prompt")
}

func TestDBToPiFramesMessages_ClearBoundaryFrameCarriesTrigger(t *testing.T) {
	clear := store.KindClear
	frames := DBToPiFramesMessages([]store.Message{
		{Ordinal: 0, Kind: &clear, Param: anthropic.NewUserMessage(anthropic.NewTextBlock(store.ClearBoundaryText))},
	})
	var trigger string
	for _, f := range frames {
		var env struct {
			Type    string `json:"type"`
			Trigger string `json:"trigger"`
		}
		assert.NewAborting(t).NoError(json.Unmarshal(f, &env), "frame %s", f)
		if env.Type == "compaction_boundary" {
			trigger = env.Trigger
		}
	}
	assert.NewCollecting(t).Eq("clear", trigger, "the divider frame's trigger")
}
