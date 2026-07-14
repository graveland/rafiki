package child

import "encoding/json"

// pi_events.go defines the Go shapes for the subset of pi's AgentSessionEvent
// vocabulary that the daemon synthesizes when translating a non-pi child's
// native stream into pi-vocabulary frames on the bus. The JSON tags match the
// TypeScript types exactly so the attach layer (RemoteAgentSession.translate /
// updateCacheFromEvent) and pi's TUI render the frames without per-kind code.
//
// Schema source of truth:
//   - pi/packages/ai/dist/types.d.ts        (AssistantMessage / UserMessage /
//     ToolResultMessage / TextContent / ThinkingContent / ToolCall / Usage)
//   - pi/packages/agent/dist/types.d.ts     (AgentEvent union)
//   - pi/packages/coding-agent/dist/core/agent-session.d.ts (AgentSessionEvent,
//     the agent_end{messages,willRetry} shape)

// ─── Content blocks (AssistantMessage.content[] / ToolResultMessage.content[]) ──

// PiContentBlock is one content block. Its concrete shape (TextContent /
// ThinkingContent / ToolCall) is distinguished by the "type" discriminator. We
// model all fields in one struct and rely on omitempty so each constructor emits
// only the keys its block type requires. arguments uses a pointer so an empty
// map still marshals as {} (a toolCall always carries arguments) while text /
// thinking blocks omit it entirely.
type PiContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments *map[string]any `json:"arguments,omitempty"`
}

// PiTextBlock builds a TextContent block: {type:"text",text}.
func PiTextBlock(text string) PiContentBlock {
	return PiContentBlock{Type: "text", Text: text}
}

// PiThinkingBlock builds a ThinkingContent block: {type:"thinking",thinking}.
func PiThinkingBlock(thinking string) PiContentBlock {
	return PiContentBlock{Type: "thinking", Thinking: thinking}
}

// PiToolCallBlock builds a ToolCall block: {type:"toolCall",id,name,arguments}.
// Note the pi vocabulary: type is "toolCall" (not claude's "tool_use") and the
// args field is "arguments" (not claude's "input"). A nil args map is coerced to
// an empty map so the key is always present and non-null.
func PiToolCallBlock(id, name string, args map[string]any) PiContentBlock {
	if args == nil {
		args = map[string]any{}
	}
	return PiContentBlock{Type: "toolCall", ID: id, Name: name, Arguments: &args}
}

// ─── Usage ──────────────────────────────────────────────────────────────────

// PiCost mirrors AssistantMessage.usage.cost. All fields are required by the TS
// type, so they always serialize (no omitempty).
type PiCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// PiUsage mirrors the pi-ai Usage type. claude's stream-json result carries token
// counts we don't currently thread through, so a zero PiUsage is emitted; the
// type is non-optional on AssistantMessage, so it must be present.
type PiUsage struct {
	Input       int    `json:"input"`
	Output      int    `json:"output"`
	CacheRead   int    `json:"cacheRead"`
	CacheWrite  int    `json:"cacheWrite"`
	TotalTokens int    `json:"totalTokens"`
	Cost        PiCost `json:"cost"`
}

// ─── Messages ───────────────────────────────────────────────────────────────

// PiAssistantMessage mirrors the pi-ai AssistantMessage. Required fields (role,
// content, api, provider, model, usage, stopReason, timestamp) always serialize;
// optional ones (responseModel, responseId, diagnostics, errorMessage) are
// omitted.
type PiAssistantMessage struct {
	Role       string           `json:"role"`
	Content    []PiContentBlock `json:"content"`
	API        string           `json:"api"`
	Provider   string           `json:"provider"`
	Model      string           `json:"model"`
	Usage      PiUsage          `json:"usage"`
	StopReason string           `json:"stopReason"`
	Timestamp  int64            `json:"timestamp"`
}

// PiUserMessage mirrors the pi-ai UserMessage. content may be a plain string or
// a content-block array; the daemon synthesizes the string form.
type PiUserMessage struct {
	Role      string `json:"role"`
	ID        string `json:"id,omitempty"`
	Content   any    `json:"content"`
	Timestamp int64  `json:"timestamp"`
}

// PiToolResultMessage mirrors the pi-ai ToolResultMessage.
type PiToolResultMessage struct {
	Role       string           `json:"role"`
	ToolCallID string           `json:"toolCallId"`
	ToolName   string           `json:"toolName"`
	Content    []PiContentBlock `json:"content"`
	IsError    bool             `json:"isError"`
	Timestamp  int64            `json:"timestamp"`
}

// ─── AgentSessionEvent constructors ─────────────────────────────────────────
//
// Each returns a value whose JSON marshalling is one AgentSessionEvent frame.
// The daemon marshals these and publishes the bytes on the bus.

type piAgentStart struct {
	Type string `json:"type"`
}

// PiAgentStart builds {"type":"agent_start"}.
func PiAgentStart() any { return piAgentStart{Type: "agent_start"} }

// piMessageEnvelope carries an assistant message_start/message_end frame.
// parentToolUseId is set (to the spawning Task tool call's id) for frames
// produced by a sub-agent, so consumers can attribute and indent them; it is a
// pic extension to the pi vocabulary (omitted, and ignored by pi's TUI, when
// the frame is from the top-level agent).
type piMessageEnvelope struct {
	Type            string             `json:"type"`
	Message         PiAssistantMessage `json:"message"`
	ParentToolUseID string             `json:"parentToolUseId,omitempty"`
}

// PiMessageStart builds {"type":"message_start","message":<assistant>}.
func PiMessageStart(msg PiAssistantMessage, parentToolUseID string) any {
	return piMessageEnvelope{Type: "message_start", Message: msg, ParentToolUseID: parentToolUseID}
}

// PiMessageEnd builds {"type":"message_end","message":<assistant>}.
func PiMessageEnd(msg PiAssistantMessage, parentToolUseID string) any {
	return piMessageEnvelope{Type: "message_end", Message: msg, ParentToolUseID: parentToolUseID}
}

type piUserMessageEnvelope struct {
	Type    string        `json:"type"`
	Message PiUserMessage `json:"message"`
}

// PiUserMessageStart builds {"type":"message_start","message":<user>}. The TUI
// renders a user bubble only on a message_start whose message.role is "user".
func PiUserMessageStart(msg PiUserMessage) any {
	return piUserMessageEnvelope{Type: "message_start", Message: msg}
}

// PiUserMessageEnd builds {"type":"message_end","message":<user>}. The consumer
// appends the message to its cache on message_end (deduping by id).
func PiUserMessageEnd(msg PiUserMessage) any {
	return piUserMessageEnvelope{Type: "message_end", Message: msg}
}

type piMessageUpdate struct {
	Type                  string             `json:"type"`
	Message               PiAssistantMessage `json:"message"`
	AssistantMessageEvent piDoneEvent        `json:"assistantMessageEvent"`
	ParentToolUseID       string             `json:"parentToolUseId,omitempty"`
}

// piDoneEvent is the AssistantMessageEvent carried by message_update. claude has
// no token deltas, so the synthesized event is the terminal "done" event with
// the full message — back-to-back with message_start, which renders a complete
// message with no typewriter (intended; see plan streaming note).
type piDoneEvent struct {
	Type    string             `json:"type"`
	Reason  string             `json:"reason"`
	Message PiAssistantMessage `json:"message"`
}

// PiMessageUpdate builds a message_update carrying the full mapped content and a
// terminal "done" assistantMessageEvent. reason mirrors the message stopReason
// (toolUse / stop / length); other StopReason values aren't producible here.
func PiMessageUpdate(msg PiAssistantMessage, parentToolUseID string) any {
	reason := msg.StopReason
	switch reason {
	case "toolUse", "length":
		// keep
	default:
		reason = "stop"
	}
	return piMessageUpdate{
		Type:                  "message_update",
		Message:               msg,
		AssistantMessageEvent: piDoneEvent{Type: "done", Reason: reason, Message: msg},
		ParentToolUseID:       parentToolUseID,
	}
}

type piToolExecutionStart struct {
	Type            string `json:"type"`
	ToolCallID      string `json:"toolCallId"`
	ToolName        string `json:"toolName"`
	Args            any    `json:"args"`
	ParentToolUseID string `json:"parentToolUseId,omitempty"`
}

// PiToolExecutionStart builds a tool_execution_start frame. parentToolUseID is
// the spawning Task call's id when this tool runs inside a sub-agent (empty
// otherwise).
func PiToolExecutionStart(toolCallID, toolName string, args any, parentToolUseID string) any {
	if args == nil {
		args = map[string]any{}
	}
	return piToolExecutionStart{Type: "tool_execution_start", ToolCallID: toolCallID, ToolName: toolName, Args: args, ParentToolUseID: parentToolUseID}
}

type piToolExecutionEnd struct {
	Type            string `json:"type"`
	ToolCallID      string `json:"toolCallId"`
	ToolName        string `json:"toolName"`
	Result          any    `json:"result"`
	IsError         bool   `json:"isError"`
	ParentToolUseID string `json:"parentToolUseId,omitempty"`
}

// PiToolExecutionEnd builds a tool_execution_end frame. parentToolUseID is the
// spawning Task call's id when this tool runs inside a sub-agent.
func PiToolExecutionEnd(toolCallID, toolName string, result any, isError bool, parentToolUseID string) any {
	return piToolExecutionEnd{Type: "tool_execution_end", ToolCallID: toolCallID, ToolName: toolName, Result: result, IsError: isError, ParentToolUseID: parentToolUseID}
}

type piAgentEnd struct {
	Type      string            `json:"type"`
	Messages  []json.RawMessage `json:"messages"`
	WillRetry bool              `json:"willRetry"`
	Usage     *PiUsage          `json:"usage,omitempty"`
}

// PiAgentEnd builds the terminal agent_end frame carrying the FULL accumulated
// messages[] and willRetry:false. messages is pre-marshalled (each entry is a
// user / assistant / toolResult message) so a heterogeneous slice round-trips
// without an interface tag. usage (a pic extension to the pi vocabulary,
// ignored by pi's TUI) carries the turn's token/cost totals when known.
func PiAgentEnd(messages []json.RawMessage, usage *PiUsage) any {
	if messages == nil {
		messages = []json.RawMessage{}
	}
	return piAgentEnd{Type: "agent_end", Messages: messages, WillRetry: false, Usage: usage}
}

type piAgentSettled struct {
	Type string `json:"type"`
}

// PiAgentSettled builds {"type":"agent_settled"} — pi's true-idle event
// (since v0.80.x agent_end only ends one low-level run and may be followed
// by an auto-retry or compaction; consumers like pi's InteractiveMode key
// post-run actions on agent_settled). The claude backend has no retry
// continuation, so the synthesized stream settles immediately after
// agent_end.
func PiAgentSettled() any { return piAgentSettled{Type: "agent_settled"} }
