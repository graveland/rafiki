package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/fundi/internal/child"
	"git.graveland.dev/brent/rafiki/routing"
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
	pricer   Pricer

	messages []json.RawMessage
	usage    child.PiUsage

	// started guards StreamStart so it is idempotent within a turn. The
	// caller invokes StreamStart on the first CONTENT event of a streamed
	// response, not on the API's own message_start, so that a retry (e.g.
	// rafiki's sendWithTrim, which resends up to 3 times on a
	// prompt-too-large error) that fails before any content arrives cannot
	// leave an orphaned message_start — and hence abandoned text — in an
	// attached TUI. Reset by StreamEnd so the next turn starts again.
	started bool
}

// Pricer resolves a model id to its per-token list price, mirroring rafiki's
// insights.Pricer. routing.ModelCatalog.Pricing has this exact signature and is
// assignable directly, so the catalog's resolution rules (including its
// fallback from a dated Anthropic snapshot id to the base model, which
// OpenRouter doesn't list) apply for free. A nil Pricer, or one returning
// ok=false, leaves cost zero.
type Pricer func(model string) (routing.ModelPricing, bool)

// NewEmitter constructs an Emitter that writes pi frames through fe, tagging
// assistant messages with provider and pricing each turn's usage via pricer
// (nil = report tokens with zero cost).
func NewEmitter(fe *Frontend, provider string, pricer Pricer) *Emitter {
	return &Emitter{fe: fe, provider: provider, pricer: pricer}
}

// AgentStart emits {"type":"agent_start"}.
func (e *Emitter) AgentStart() {
	e.fe.Emit(child.PiAgentStart())
}

// UserMessage emits the message_start/message_end pair for an accepted
// prompt or steer, and accumulates the echoed user message for the eventual
// agent_end frame.
func (e *Emitter) UserMessage(text string) {
	ts := time.Now().UnixMilli()
	msg := child.PiUserMessage{Role: "user", ID: fmt.Sprintf("user-%d", ts), Content: text, Timestamp: ts}
	e.fe.Emit(child.PiUserMessageStart(msg))
	e.fe.Emit(child.PiUserMessageEnd(msg))
	e.accumulate(msg)
}

// AssistantTurn maps resp into a PiAssistantMessage and emits
// message_start/message_update/message_end, accumulating the mapped message
// and folding its usage into the turn total.
func (e *Emitter) AssistantTurn(resp *anthropic.Message) {
	msg := MapAssistantMessage(resp, e.provider, e.pricer)
	e.fe.Emit(child.PiMessageStart(msg, ""))
	e.fe.Emit(child.PiMessageUpdate(msg, ""))
	e.fe.Emit(child.PiMessageEnd(msg, ""))
	e.accumulate(msg)
	e.addUsage(msg.Usage)
}

// StreamStart emits message_start for a streaming assistant turn. It is
// idempotent within the turn: a second call before StreamEnd is a no-op, so
// the caller can invoke it unconditionally on the first content event of a
// streamed response (rather than on the API's own message_start) without
// risking a duplicate frame. See the started field doc for why that timing
// matters.
func (e *Emitter) StreamStart(msg child.PiAssistantMessage) {
	if e.started {
		return
	}
	e.started = true
	e.fe.Emit(child.PiMessageStart(msg, ""))
}

// StreamDelta emits one message_update carrying the message accumulated so
// far. Unlike StreamEnd, it does not accumulate or fold usage — only the
// final message represents the turn.
func (e *Emitter) StreamDelta(msg child.PiAssistantMessage) {
	e.fe.Emit(child.PiMessageUpdate(msg, ""))
}

// StreamEnd emits message_end for the finished message, accumulates it for
// the eventual agent_end frame, and folds its usage into the turn total —
// the same bookkeeping AssistantTurn does, so per-turn cost cannot silently
// diverge depending on whether the turn was streamed. It also resets the
// started guard so the next turn's StreamStart fires again.
func (e *Emitter) StreamEnd(msg child.PiAssistantMessage) {
	e.fe.Emit(child.PiMessageEnd(msg, ""))
	e.accumulate(msg)
	e.addUsage(msg.Usage)
	e.started = false
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

// addUsage folds an assistant turn's usage — tokens and cost — into the turn
// total reported on agent_end. Cost is summed per component rather than
// recomputed, so the total stays consistent with the per-message costs even
// across a turn whose messages were served by different models.
func (e *Emitter) addUsage(u child.PiUsage) {
	e.usage.Input += u.Input
	e.usage.Output += u.Output
	e.usage.CacheRead += u.CacheRead
	e.usage.CacheWrite += u.CacheWrite
	e.usage.TotalTokens += u.TotalTokens
	e.usage.Cost.Input += u.Cost.Input
	e.usage.Cost.Output += u.Cost.Output
	e.usage.Cost.CacheRead += u.Cost.CacheRead
	e.usage.Cost.CacheWrite += u.Cost.CacheWrite
	e.usage.Cost.Total += u.Cost.Total
}

// costOf prices usage for the model that actually served the response, using
// rafiki's shared per-component formula (routing.ModelPricing.Cost) so this
// runtime and rafiki's analyze pipeline can never drift on the arithmetic.
//
// Zero is returned when there is no pricer or the model is unpriced — both are
// normal (a fake sender in tests, a catalog that hasn't loaded, a model
// OpenRouter doesn't list), which is why an unpriced turn reports cost 0 rather
// than failing the turn.
func costOf(pricer Pricer, model string, usage anthropic.Usage) child.PiCost {
	if pricer == nil {
		return child.PiCost{}
	}
	price, ok := pricer(model)
	if !ok {
		return child.PiCost{}
	}
	c := price.Cost(usage)
	return child.PiCost{
		Input:      c.Input,
		Output:     c.Output,
		CacheRead:  c.CacheRead,
		CacheWrite: c.CacheWrite,
		Total:      c.Total,
	}
}

// MapAssistantMessage maps an Anthropic SDK response message onto the pi
// AssistantMessage wire shape, tagging it with provider and this layer's
// fixed API identifier ("anthropic-messages"). Timestamp is captured at map
// time (time.Now().UnixMilli()).
func MapAssistantMessage(resp *anthropic.Message, provider string, pricer Pricer) child.PiAssistantMessage {
	// Non-nil (rather than a nil slice growing via append) so a response with
	// no mappable blocks still marshals content as [] and not JSON null — the
	// pi TUI expects an array here, matching the nil-coercion precedent
	// elsewhere in child.Pi* (PiToolExecutionStart args, PiAgentEnd messages).
	blocks := make([]child.PiContentBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			// Skip empty text. PiContentBlock.Text is `omitempty`, so an empty
			// string serializes to {type:"text"} with no `text` field, and pi's
			// TUI does c.text.trim() → crashes on the undefined field. Mirrors
			// the same guard in provider_claude_state.go's emitAssistant.
			if v.Text != "" {
				blocks = append(blocks, child.PiTextBlock(v.Text))
			}
		case anthropic.ThinkingBlock:
			// Same hazard for thinking: an empty string would serialize to
			// {type:"thinking"} (no `thinking` field) and crash pi's TUI at
			// c.thinking.trim(). Skip empty thinking blocks entirely.
			if v.Thinking != "" {
				blocks = append(blocks, child.PiThinkingBlock(v.Thinking))
			}
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
	usage.Cost = costOf(pricer, string(resp.Model), u)

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
