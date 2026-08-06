package fundi

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/child"
)

// DBToPiFrames converts persisted conversation messages (from
// store.Messages.Load) into the pi AgentSessionEvent frames the attach TUI
// and ctrl_get_recent consumers expect. This is the read path for fundi
// children once the DB is the canonical store — the ring buffer is bypassed
// entirely.
//
// Each message produces a message_start / message_end pair. Streaming deltas
// (message_update) and tool-execution lifecycle frames (tool_execution_start/
// tool_execution_end) are not synthesised; completed messages carry all the
// content the TUI needs. An agent_start frame is prepended and an agent_end
// frame (carrying the full Anthropic messages array) is appended so the TUI's
// AgentSession can seed its message cache — see the attach TUI's primeHistory
// and updateCacheFromEvent, which populate _messages from agent_end's messages
// field.
func DBToPiFrames(msgs []anthropic.MessageParam) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(msgs)*2+2)

	// Start the session so the TUI knows a stream is beginning.
	if b, err := json.Marshal(child.PiAgentStart()); err == nil {
		out = append(out, b)
	}

	for _, m := range msgs {
		switch m.Role {
		case "user":
			frames := dbUserFrames(m)
			out = append(out, frames...)
		case "assistant":
			frames := dbAssistantFrames(m)
			out = append(out, frames...)
		}
	}

	// Terminal agent_end carrying the full Anthropic-formatted message list.
	// The TUI's AgentSession seeds its message cache from agent_end.messages
	// (see attach/src/session.ts updateCacheFromEvent), and the individual
	// message_start/message_end frames above populate user/assistant bubbles
	// during replay. Without agent_end, the TUI never gets the authoritative
	// Anthropic message list and shows no history.
	rawMsgs := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			// A malformed MessageParam (should never reach the DB) is logged
			// by the caller via its load warning; skip it here so the rest
			// of the agent_end payload is still valid.
			continue
		}
		rawMsgs[i] = b
	}
	if b, err := json.Marshal(child.PiAgentEnd(rawMsgs, nil)); err == nil {
		out = append(out, b)
	}

	return out
}

// dbUserFrames produces message_start / message_end for a user message.
// Content is extracted from text blocks into a plain string; tool_result
// blocks are preserved as structured content.
func dbUserFrames(m anthropic.MessageParam) []json.RawMessage {
	ts := time.Now().UnixMilli()
	id := fmt.Sprintf("user-%d", ts)

	// User messages in the DB may have text, tool_result, or both.
	// PiUserMessage.Content is `any`, so we can pass either a string or a
	// []any of blocks. The TUI handles both.
	hasToolResults := false
	var textParts []string
	for _, block := range m.Content {
		if block.OfText != nil && block.OfText.Text != "" {
			textParts = append(textParts, block.OfText.Text)
		}
		if block.OfToolResult != nil {
			hasToolResults = true
		}
		// Other block types (image, document, thinking) don't appear in
		// user messages from fundi's agent loop.
	}

	if hasToolResults {
		// Emit as a structured user message so tool_result blocks survive.
		// Convert Anthropic content blocks to generic maps — the TUI
		// uses the tool_use_id and content fields from tool_result blocks.
		blocks := make([]map[string]any, 0, len(m.Content))
		for _, block := range m.Content {
			switch {
			case block.OfText != nil:
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": block.OfText.Text,
				})
			case block.OfToolResult != nil:
				blocks = append(blocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": block.OfToolResult.ToolUseID,
					"content":     block.OfToolResult.Content,
					"is_error":    block.OfToolResult.IsError.Value,
				})
			}
		}
		msg := child.PiUserMessage{Role: "user", ID: id, Content: blocks, Timestamp: ts}
		start, _ := json.Marshal(child.PiUserMessageStart(msg))
		end, _ := json.Marshal(child.PiUserMessageEnd(msg))
		return []json.RawMessage{start, end}
	}

	// Plain text user message.
	text := ""
	if len(textParts) > 0 {
		text = textParts[0]
	}
	msg := child.PiUserMessage{Role: "user", ID: id, Content: text, Timestamp: ts}
	start, _ := json.Marshal(child.PiUserMessageStart(msg))
	end, _ := json.Marshal(child.PiUserMessageEnd(msg))
	return []json.RawMessage{start, end}
}

// dbAssistantFrames produces message_start / message_end for an assistant
// message stored in the DB. Anthropic content blocks are mapped to their
// PiContentBlock equivalents: text → PiTextBlock, thinking → PiThinkingBlock,
// tool_use → PiToolCallBlock.
func dbAssistantFrames(m anthropic.MessageParam) []json.RawMessage {
	blocks := make([]child.PiContentBlock, 0, len(m.Content))
	for _, block := range m.Content {
		switch {
		case block.OfText != nil && block.OfText.Text != "":
			blocks = append(blocks, child.PiTextBlock(block.OfText.Text))
		case block.OfThinking != nil && block.OfThinking.Thinking != "":
			blocks = append(blocks, child.PiThinkingBlock(block.OfThinking.Thinking))
		case block.OfToolUse != nil:
			args := toolUseArgs(block.OfToolUse.Input)
			blocks = append(blocks, child.PiToolCallBlock(block.OfToolUse.ID, block.OfToolUse.Name, args))
		}
	}

	msg := child.PiAssistantMessage{
		Role:       "assistant",
		Content:    blocks,
		API:        "anthropic-messages",
		Provider:   "", // unknown from DB messages alone; zero is acceptable
		Model:      "", // unknown from DB messages alone; zero is acceptable
		Usage:      child.PiUsage{},
		StopReason: "stop",
		Timestamp:  time.Now().UnixMilli(),
	}
	start, _ := json.Marshal(child.PiMessageStart(msg, ""))
	end, _ := json.Marshal(child.PiMessageEnd(msg, ""))
	return []json.RawMessage{start, end}
}

// toolUseArgs converts a tool_use block's input (json.RawMessage or map) into
// the map[string]any that PiToolCallBlock expects.
func toolUseArgs(input any) map[string]any {
	switch v := input.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			return map[string]any{"_raw": string(v)}
		}
		return m
	default:
		return map[string]any{}
	}
}
