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
	// agent_start + message_start(assistant) + message_end(assistant) + agent_end = 4
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 4", len(frames))
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
	if err := json.Unmarshal(frames[1], &env); err != nil {
		t.Fatal(err)
	}
	if env.Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", env.Message.Role)
	}
	if len(env.Message.Content) != 1 || env.Message.Content[0].Text != "pong" {
		t.Errorf("content = %+v, want [{text pong}]", env.Message.Content)
	}
}

func TestDBToPiFrames_ToolUse(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewAssistantMessage(
			anthropic.NewToolUseBlock("toolu_01", map[string]any{"file": "/tmp/x"}, "read"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start(tool_use) + message_end(tool_use) + agent_end = 4
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 4", len(frames))
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
		anthropic.NewUserMessage(
			anthropic.NewToolResultBlock("toolu_01", "[]", isError),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start(user tool_result) + message_end(user tool_result) + agent_end = 4
	if len(frames) != 4 {
		t.Fatalf("got %d frames, want 4", len(frames))
	}
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				IsError   bool   `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(frames[1], &env); err != nil {
		t.Fatal(err)
	}
	if env.Message.Role != "user" {
		t.Errorf("role = %q, want user", env.Message.Role)
	}
	b := env.Message.Content[0]
	if b.Type != "tool_result" {
		t.Errorf("block type = %q, want tool_result", b.Type)
	}
	if b.ToolUseID != "toolu_01" {
		t.Errorf("tool_use_id = %q", b.ToolUseID)
	}
	if !b.IsError {
		t.Error("is_error = false, want true")
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
	// agent_start + 4 message pairs + agent_end = 10 frames
	if len(frames) != 10 {
		t.Fatalf("got %d frames, want 10 (agent_start + 4 message pairs + agent_end)", len(frames))
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
	// Each message must be a valid Anthropic MessageParam.
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
