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
// Every message produces frames that mirror what the live fundi Emitter would
// have produced: user prompts get message_start/message_end, assistant turns
// get message_start/message_update/message_end plus tool_execution_start for
// each tool_use, and tool results get tool_execution_end. An agent_start frame
// is prepended and an agent_end frame carrying the Pi-format accumulated
// messages is appended so the TUI's AgentSession can seed its message cache.
func DBToPiFrames(msgs []anthropic.MessageParam) []json.RawMessage {
	// Phase 1: collect tool_use blocks and resolved tool_use ids (those with
	// a matching tool_result) so tool_execution_start is only emitted when a
	// result follows and tool_execution_end can recover the tool name.
	uses, resolved := toolUseMap(msgs)

	// Phase 2: build frames and accumulate Pi-format messages for agent_end.
	out := make([]json.RawMessage, 0, len(msgs)*3+2)
	piMsgs := make([]json.RawMessage, 0, len(msgs))

	out = appendFrame(out, child.PiAgentStart())

	for _, m := range msgs {
		switch m.Role {
		case "user":
			frames, piMsgsFor := dbUserFramesWithTools(m, uses)
			out = append(out, frames...)
			piMsgs = append(piMsgs, piMsgsFor...)
		case "assistant":
			frames, piMsg := dbAssistantFramesWithTools(m, uses, resolved)
			out = append(out, frames...)
			piMsgs = appendPiMsg(piMsgs, piMsg)
		}
	}

	out = appendFrame(out, child.PiAgentEnd(piMsgs, nil))

	return out
}

// toolUseInfo carries the fields a tool_execution_end frame needs to pair a
// tool_result block with its originating tool_use.
type toolUseInfo struct {
	name  string
	input json.RawMessage
}

// toolUseMap scans messages for tool_use blocks and tool_result blocks,
// returning (1) a map from tool_use id → {name, input} for name recovery in
// tool_execution_end, and (2) the set of tool_use ids that have a matching
// tool_result — only those ids get tool_execution_start frames.
func toolUseMap(msgs []anthropic.MessageParam) (map[string]toolUseInfo, map[string]bool) {
	m := make(map[string]toolUseInfo)
	resolved := make(map[string]bool)
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if tu := block.OfToolUse; tu != nil && tu.ID != "" {
				input, _ := json.Marshal(tu.Input)
				m[tu.ID] = toolUseInfo{name: tu.Name, input: input}
			}
			if tr := block.OfToolResult; tr != nil && tr.ToolUseID != "" {
				resolved[tr.ToolUseID] = true
			}
		}
	}
	return m, resolved
}

// dbUserFramesWithTools produces frames and Pi-format messages for a user
// message. Tool_result blocks emit tool_execution_end (the TUI renders tool
// output from tool_execution_start/end, not from message content blocks).
// Pure tool_result messages do NOT get a user message_start/message_end —
// the live Emitter accumulates PiToolResultMessage but does not echo user
// message frames for tool results.
func dbUserFramesWithTools(m anthropic.MessageParam, uses map[string]toolUseInfo) ([]json.RawMessage, []json.RawMessage) {
	ts := time.Now().UnixMilli()
	id := fmt.Sprintf("user-%d", ts)

	var (
		frames []json.RawMessage
		piMsgs []json.RawMessage
		texts  []string
	)

	for _, block := range m.Content {
		switch {
		case block.OfText != nil && block.OfText.Text != "":
			texts = append(texts, block.OfText.Text)
		case block.OfToolResult != nil:
			tr := block.OfToolResult
			resultText := toolResultText(tr.Content)
			isErr := tr.IsError.Value
			name := ""
			if u, ok := uses[tr.ToolUseID]; ok {
				name = u.name
			}
			frames = appendFrame(frames, child.PiToolExecutionEnd(tr.ToolUseID, name, resultText, isErr, ""))
			piMsgs = appendPiMsg(piMsgs, child.PiToolResultMessage{
				Role:       "toolResult",
				ToolCallID: tr.ToolUseID,
				ToolName:   name,
				Content:    []child.PiContentBlock{child.PiTextBlock(resultText)},
				IsError:    isErr,
				Timestamp:  ts,
			})
		}
	}

	if len(texts) > 0 {
		// This is a user prompt or steer — emit message_start/message_end.
		text := texts[0]
		msg := child.PiUserMessage{Role: "user", ID: id, Content: text, Timestamp: ts}
		frames = appendFrame(frames, child.PiUserMessageStart(msg))
		frames = appendFrame(frames, child.PiUserMessageEnd(msg))
		piMsgs = appendPiMsg(piMsgs, msg)
	}

	return frames, piMsgs
}

// dbAssistantFramesWithTools produces frames and a Pi-format message for an
// assistant message. In addition to message_start/message_update/message_end,
// it emits tool_execution_start for each tool_use block that has a matching
// tool_result later in the conversation.
func dbAssistantFramesWithTools(m anthropic.MessageParam, uses map[string]toolUseInfo, resolved map[string]bool) ([]json.RawMessage, json.RawMessage) {
	blocks := make([]child.PiContentBlock, 0, len(m.Content))
	hasToolUse := false
	for _, block := range m.Content {
		switch {
		case block.OfText != nil && block.OfText.Text != "":
			blocks = append(blocks, child.PiTextBlock(block.OfText.Text))
		case block.OfThinking != nil && block.OfThinking.Thinking != "":
			blocks = append(blocks, child.PiThinkingBlock(block.OfThinking.Thinking))
		case block.OfToolUse != nil:
			hasToolUse = true
			args := toolUseArgs(block.OfToolUse.Input)
			blocks = append(blocks, child.PiToolCallBlock(block.OfToolUse.ID, block.OfToolUse.Name, args))
		}
	}

	stopReason := "stop"
	if hasToolUse {
		stopReason = "toolUse"
	}
	msg := child.PiAssistantMessage{
		Role:       "assistant",
		Content:    blocks,
		API:        "anthropic-messages",
		Provider:   "", // unknown from DB messages alone; zero is acceptable
		Model:      "", // unknown from DB messages alone; zero is acceptable
		Usage:      child.PiUsage{},
		StopReason: stopReason,
		Timestamp:  time.Now().UnixMilli(),
	}

	var frames []json.RawMessage
	frames = appendFrame(frames, child.PiMessageStart(msg, ""))
	frames = appendFrame(frames, child.PiMessageUpdate(msg, ""))
	// Emit tool_execution_start for each tool_use that has a matching
	// tool_result, mirroring what the live Emitter does (ToolStart before
	// message_end). Without these, the TUI shows the tool_use block but not
	// the tool-execution lifecycle that renders the output.
	for _, block := range m.Content {
		if tu := block.OfToolUse; tu != nil && resolved[tu.ID] {
			args := toolUseArgs(tu.Input)
			frames = appendFrame(frames, child.PiToolExecutionStart(tu.ID, tu.Name, args, ""))
		}
	}
	frames = appendFrame(frames, child.PiMessageEnd(msg, ""))

	piMsg := mustFrame(msg)
	return frames, piMsg
}

// toolResultText extracts a plain-text representation from the SDK's
// ToolResultBlockParamContentUnion array. Each element is typically OfText.
func toolResultText(content []anthropic.ToolResultBlockParamContentUnion) string {
	for _, c := range content {
		if c.OfText != nil {
			return c.OfText.Text
		}
	}
	// Fallback: marshal the whole thing. This shouldn't be reached for
	// normal tool_result content (which always has OfText blocks).
	if b, err := json.Marshal(content); err == nil && len(b) > 0 {
		return string(b)
	}
	return ""
}

// appendFrame marshals v and appends the result to out. Marshal failures are
// silently dropped (v is always a well-formed child.Pi* type).
func appendFrame(out []json.RawMessage, v any) []json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	return append(out, b)
}

// mustFrame marshals v and returns the result, or nil on error.
func mustFrame(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// appendPiMsg marshals v and appends the result to out. Nil entries are
// skipped, not appended.
//
// Callers sometimes pass an already-marshaled json.RawMessage (e.g. the
// return of mustFrame, which is nil when marshaling failed upstream). A nil
// or empty json.RawMessage must be special-cased BEFORE the marshal below:
// json.RawMessage.MarshalJSON returns the literal `null` for an empty
// receiver, never a nil slice or an error, so checking json.Marshal's result
// for b == nil never catches this case — it would instead append a literal
// JSON `null` into agent_end.messages.
func appendPiMsg(out []json.RawMessage, v any) []json.RawMessage {
	if raw, ok := v.(json.RawMessage); ok && len(raw) == 0 {
		return out
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	return append(out, b)
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
