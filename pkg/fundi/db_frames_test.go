package fundi

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestDBToPiFrames_Empty(t *testing.T) {
	frames := DBToPiFrames(nil)
	// At minimum: agent_start frame.
	if len(frames) == 0 {
		t.Fatal("empty messages should still produce at least agent_start")
	}
	var hdr struct{ Type string }
	if err := json.Unmarshal(frames[0], &hdr); err != nil || hdr.Type != "agent_start" {
		t.Fatalf("first frame = %s, want agent_start", frames[0])
	}
	// Only agent_start frame.
	if len(frames) != 1 {
		t.Fatalf("got %d frames for empty input, want 1 (agent_start only)", len(frames))
	}
}

func TestDBToPiFrames_UserText(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(
			anthropic.NewTextBlock("hello world"),
		),
	}
	frames := DBToPiFrames(msgs)
	// agent_start + message_start(user) + message_end(user) = 3
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
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
	// agent_start + message_start(assistant) + message_end(assistant) = 3
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
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
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
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
	// Should have user message_start + message_end.
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
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
	// agent_start + 4 message pairs = 9 frames
	if len(frames) != 9 {
		t.Fatalf("got %d frames, want 9 (agent_start + 4 message pairs)", len(frames))
	}
}
