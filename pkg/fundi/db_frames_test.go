package fundi

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestDBToPiFrames_Empty(t *testing.T) {
	frames := DBToPiFrames(nil)
	// agent_start + agent_end (empty messages) = 2
	if len(frames) != 2 {
		t.Fatalf("got %d frames for empty input, want 2 (agent_start + agent_end)", len(frames))
	}
	var hdr struct{ Type string }
	if err := json.Unmarshal(frames[0], &hdr); err != nil || hdr.Type != "agent_start" {
		t.Fatalf("first frame = %s, want agent_start", frames[0])
	}
	if err := json.Unmarshal(frames[1], &hdr); err != nil || hdr.Type != "agent_end" {
		t.Fatalf("last frame = %s, want agent_end", frames[1])
	}
}

func TestDBToPiFrames_UserText(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(
			anthropic.NewTextBlock("hello world"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start(user) + message_end(user) + agent_end = 4
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 4", len(frames))
	}
	// Verify the user message frames.
	for i := 1; i < 3; i++ {
		var env struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(frames[i], &env); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if env.Message.Role != "user" {
			t.Errorf("frame %d role = %q, want user", i, env.Message.Role)
		}
		if env.Message.Content != "hello world" {
			t.Errorf("frame %d content = %q, want 'hello world'", i, env.Message.Content)
		}
	}
}

func TestDBToPiFrames_AssistantText(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewAssistantMessage(
			anthropic.NewTextBlock("pong"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start + message_update + message_end + agent_end = 5
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}
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
	if err := json.Unmarshal(frames[1], &env); err != nil {
		t.Fatal(err)
	}
	if env.Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", env.Message.Role)
	}
	if len(env.Message.Content) != 1 || env.Message.Content[0].Text != "pong" {
		t.Errorf("content = %+v, want [{text pong}]", env.Message.Content)
	}
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
	msgs := []anthropic.MessageParam{
		anthropic.NewAssistantMessage(
			anthropic.NewToolUseBlock("toolu_01", map[string]any{"file": "/tmp/x"}, "read"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start + message_update + message_end + agent_end = 5
	// (No tool_execution_start — there is no matching tool_result.)
	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5", len(frames))
	}
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
	if err := json.Unmarshal(frames[1], &env); err != nil {
		t.Fatal(err)
	}
	b := env.Message.Content[0]
	if b.Type != "toolCall" {
		t.Errorf("block type = %q, want toolCall", b.Type)
	}
	if b.ID != "toolu_01" {
		t.Errorf("id = %q, want toolu_01", b.ID)
	}
	if b.Name != "read" {
		t.Errorf("name = %q, want read", b.Name)
	}
	if b.Arguments["file"] != "/tmp/x" {
		t.Errorf("arguments = %v", b.Arguments)
	}
}

func TestDBToPiFrames_UserWithToolResult(t *testing.T) {
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
	if len(frames) != 7 {
		t.Fatalf("got %d frames, want 7", len(frames))
	}

	// Frame 3: tool_execution_start
	var start struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
	}
	if err := json.Unmarshal(frames[3], &start); err != nil {
		t.Fatalf("frame 3 (tool_execution_start): %v", err)
	}
	if start.Type != "tool_execution_start" {
		t.Errorf("frame 3 type = %q, want tool_execution_start", start.Type)
	}
	if start.ToolCallID != "toolu_01" {
		t.Errorf("tool_execution_start toolCallId = %q, want toolu_01", start.ToolCallID)
	}
	if start.ToolName != "bash" {
		t.Errorf("tool_execution_start toolName = %q, want bash", start.ToolName)
	}

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
	if err := json.Unmarshal(frames[5], &end); err != nil {
		t.Fatalf("frame 5 (tool_execution_end): %v", err)
	}
	if end.Type != "tool_execution_end" {
		t.Errorf("frame 5 type = %q, want tool_execution_end", end.Type)
	}
	if end.ToolCallID != "toolu_01" {
		t.Errorf("tool_execution_end toolCallId = %q, want toolu_01", end.ToolCallID)
	}
	if !end.IsError {
		t.Error("tool_execution_end isError = false, want true")
	}
	if len(end.Result.Content) != 1 || end.Result.Content[0].Text != "file1.txt\nfile2.txt" {
		t.Errorf("tool_execution_end result = %+v, want {text 'file1.txt\\nfile2.txt'}", end.Result.Content)
	}
}

func TestDBToPiFrames_Limit(t *testing.T) {
	// This test verifies that limit filtering is applied by the caller
	// (dbRecentForFundi), not by DBToPiFrames itself. DBToPiFrames returns
	// all frames; the caller slices.
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("msg1")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply1")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("msg2")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply2")),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + (2 user messages × 2 frames) + (2 assistant messages × 3 frames) + agent_end = 12
	if len(frames) != 12 {
		t.Fatalf("got %d frames, want 12 (agent_start + 2 user pairs + 2 assistant trios + agent_end)", len(frames))
	}
}

func TestDBToPiFrames_AgentEndCarriesMessages(t *testing.T) {
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
	if err := json.Unmarshal(last, &end); err != nil {
		t.Fatalf("unmarshal agent_end: %v", err)
	}
	if end.Type != "agent_end" {
		t.Fatalf("last frame type = %q, want agent_end", end.Type)
	}
	if len(end.Messages) != 2 {
		t.Fatalf("agent_end messages = %d, want 2 (user + assistant)", len(end.Messages))
	}
	// Each message must be a valid Pi-format message (PiUserMessage, PiAssistantMessage).
	for i, m := range end.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if msg.Role == "" {
			t.Errorf("message %d: role empty", i)
		}
	}
}
