package agent

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/fundi/internal/child"
)

// Emitter converts Anthropic SDK messages and tool-execution events into pi
// AgentSessionEvent frames and writes them through a Frontend, so the daemon
// (identity PiProvider) and pi's TUI render fundi's native agent turns
// indistinguishably from a real pi child.
//
// A pi child echoes user messages itself (PiProvider.OutboundEcho returns
// nil), so Emitter.UserMessage must be called for every accepted
// prompt/steer or the TUI never renders the user bubble.
//
// Emitter also accumulates the turn's messages (user echo, assistant turns,
// tool results) and summed token usage so AgentEnd can emit the full
// messages[] the daemon/TUI expect. Not safe for concurrent use; the Engine
// (Task 7) is expected to drive it from a single goroutine per turn.
type Emitter struct {
	fe       *Frontend
	provider string
	modelID  string

	messages []json.RawMessage
	usage    child.PiUsage
}

// NewEmitter constructs an Emitter that writes pi frames through fe, tagging
// assistant messages with provider/modelID.
func NewEmitter(fe *Frontend, provider, modelID string) *Emitter {
	return &Emitter{fe: fe, provider: provider, modelID: modelID}
}

// AgentStart emits {"type":"agent_start"}.
func (e *Emitter) AgentStart() {
	e.fe.Emit(child.PiAgentStart())
}

// UserMessage emits the message_start/message_end pair for an accepted
// prompt or steer, and accumulates the echoed user message for the eventual
// agent_end frame.
func (e *Emitter) UserMessage(text string) {
	msg := child.PiUserMessage{Role: "user", Content: text, Timestamp: time.Now().UnixMilli()}
	e.fe.Emit(child.PiUserMessageStart(msg))
	e.fe.Emit(child.PiUserMessageEnd(msg))
	e.accumulate(msg)
}

// AssistantTurn maps resp into a PiAssistantMessage and emits
// message_start/message_update/message_end, accumulating the mapped message
// and folding its usage into the turn total.
func (e *Emitter) AssistantTurn(resp *anthropic.Message) {
	msg := MapAssistantMessage(resp, e.provider)
	e.fe.Emit(child.PiMessageStart(msg, ""))
	e.fe.Emit(child.PiMessageUpdate(msg, ""))
	e.fe.Emit(child.PiMessageEnd(msg, ""))
	e.accumulate(msg)
	e.addUsage(msg.Usage)
}

// ToolStart emits tool_execution_start for a tool call about to run.
func (e *Emitter) ToolStart(id, name string, input json.RawMessage) {
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		slog.Warn("emit: tool_execution_start input unmarshal failed", "tool", name, "id", id, "error", err)
		args = map[string]any{"_raw": string(input)}
	}
	e.fe.Emit(child.PiToolExecutionStart(id, name, args, ""))
}

// ToolEnd emits tool_execution_end for a completed tool call and accumulates
// the corresponding toolResult message for the eventual agent_end frame.
func (e *Emitter) ToolEnd(id, name, result string, isErr bool) {
	e.fe.Emit(child.PiToolExecutionEnd(id, name, result, isErr, ""))
	msg := child.PiToolResultMessage{
		Role:       "toolResult",
		ToolCallID: id,
		ToolName:   name,
		Content:    []child.PiContentBlock{child.PiTextBlock(result)},
		IsError:    isErr,
		Timestamp:  time.Now().UnixMilli(),
	}
	e.accumulate(msg)
}

// AgentEnd emits the terminal agent_end frame (carrying every accumulated
// message in order plus the turn's summed usage) followed by agent_settled,
// then resets accumulated state for the next turn.
func (e *Emitter) AgentEnd() {
	usage := e.usage
	e.fe.Emit(child.PiAgentEnd(e.messages, &usage))
	e.fe.Emit(child.PiAgentSettled())
	e.messages = nil
	e.usage = child.PiUsage{}
}

// accumulate marshals msg and appends it to the turn's message log. A
// marshal failure here means Emit's own marshal will fail identically (msg
// is always one of the well-formed child.Pi*Message types), so it is logged
// and the message is dropped from agent_end rather than corrupting the
// accumulated slice.
func (e *Emitter) accumulate(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		slog.Warn("emit: failed to accumulate message for agent_end", "error", err)
		return
	}
	e.messages = append(e.messages, b)
}

// addUsage folds an assistant turn's usage into the turn total reported on
// agent_end. Cost is left zero (unknown at this layer).
func (e *Emitter) addUsage(u child.PiUsage) {
	e.usage.Input += u.Input
	e.usage.Output += u.Output
	e.usage.CacheRead += u.CacheRead
	e.usage.CacheWrite += u.CacheWrite
	e.usage.TotalTokens += u.TotalTokens
}

// MapAssistantMessage maps an Anthropic SDK response message onto the pi
// AssistantMessage wire shape, tagging it with provider and this layer's
// fixed API identifier ("anthropic-messages"). Timestamp is captured at map
// time (time.Now().UnixMilli()).
func MapAssistantMessage(resp *anthropic.Message, provider string) child.PiAssistantMessage {
	// Non-nil (rather than a nil slice growing via append) so a response with
	// no mappable blocks still marshals content as [] and not JSON null — the
	// pi TUI expects an array here, matching the nil-coercion precedent
	// elsewhere in child.Pi* (PiToolExecutionStart args, PiAgentEnd messages).
	blocks := make([]child.PiContentBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			blocks = append(blocks, child.PiTextBlock(v.Text))
		case anthropic.ThinkingBlock:
			blocks = append(blocks, child.PiThinkingBlock(v.Thinking))
		case anthropic.ToolUseBlock:
			var args map[string]any
			if err := json.Unmarshal(v.Input, &args); err != nil {
				slog.Warn("emit: tool_use input unmarshal failed", "tool", v.Name, "id", v.ID, "error", err)
				args = map[string]any{"_raw": string(v.Input)}
			}
			blocks = append(blocks, child.PiToolCallBlock(v.ID, v.Name, args))
		}
	}

	stop := "stop"
	switch resp.StopReason {
	case anthropic.StopReasonToolUse:
		stop = "toolUse"
	case anthropic.StopReasonMaxTokens:
		stop = "length"
	}

	u := resp.Usage
	usage := child.PiUsage{
		Input:      int(u.InputTokens),
		Output:     int(u.OutputTokens),
		CacheRead:  int(u.CacheReadInputTokens),
		CacheWrite: int(u.CacheCreationInputTokens),
	}
	usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite

	return child.PiAssistantMessage{
		Role:       "assistant",
		Content:    blocks,
		API:        "anthropic-messages",
		Provider:   provider,
		Model:      string(resp.Model),
		Usage:      usage,
		StopReason: stop,
		Timestamp:  time.Now().UnixMilli(),
	}
}
