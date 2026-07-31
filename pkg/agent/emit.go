package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/routing"
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

// mapMessage maps resp through this emitter's own provider/pricer. It exists
// so the engine's streaming handler (package agent, but a different type) can
// build the child.PiAssistantMessage that StreamStart/StreamDelta/StreamEnd
// take without reaching into Emitter's provider/pricer fields directly.
func (e *Emitter) mapMessage(resp *anthropic.Message) child.PiAssistantMessage {
	return MapAssistantMessage(resp, e.provider, e.pricer)
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
	// Also reset the streaming guard: a stream that failed or was aborted
	// after content arrived never reaches StreamEnd, and a surviving
	// `started` would suppress the NEXT turn's message_start entirely.
	e.started = false
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

// streamingToolArgs decodes a tool_use block's Input for MapAssistantMessage,
// distinguishing "still streaming" from "genuinely malformed" so only the
// latter warns.
//
// Per anthropic.Message.Accumulate (messageutil.go), a tool_use block's Input
// starts as the literal "{}" (content_block_start) and is then either
// REPLACED wholesale by the first non-empty input_json_delta or APPENDED to
// by every one after that. So while the block is still open, Input is a JSON
// *prefix* of the eventual object — e.g. `{"file_path": "/Users/b` — not a
// malformed document. Only content_block_stop (which this function has no
// visibility into; see MapAssistantMessage's doc on why it reads fields
// directly) guarantees Input is complete.
//
// A byte-slice json.Unmarshal can't tell a truncated prefix from a syntax
// error — both just fail. Decoding through a json.Decoder over a
// bytes.Reader does: reading a token stream that runs out of input before
// the value closes surfaces io.ErrUnexpectedEOF (or, for a zero-length
// Input, plain io.EOF) specifically, while a value that is syntactically
// broken but *complete* (e.g. `{a:1}`, unquoted key) surfaces a
// *json.SyntaxError distinguishable from either. That is exactly the
// truncated-vs-broken distinction this function needs, and it costs nothing
// extra: it's a bounded decode of an already-in-memory byte slice, not a
// stream read.
//
// A still-streaming block reports empty arguments ({}) with no warning,
// deliberately mirroring the API's own initial wire state for a fresh
// tool_use block (content_block_start's Input is always "{}") rather than
// omitting the block. Omitting it would make the block appear, vanish (once
// the first delta lands and Input stops being "{}"), then reappear (once the
// block closes) in successive message_update frames — worse for an attached
// TUI than staying present throughout with a placeholder that fills in once
// complete, and it exactly matches the pre-regression AsAny() path's
// behavior for this case ("wrong, but benign" — see this file's package
// doc). A genuinely malformed *complete* input still warns and falls back to
// {"_raw": ...}, unchanged from before.
func streamingToolArgs(id, name string, input json.RawMessage) map[string]any {
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(input))
	err := dec.Decode(&args)
	switch {
	case err == nil:
		return args
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// Input ran out before the value closed: still streaming, not an
		// error. Do not warn.
		return map[string]any{}
	default:
		slog.Warn("emit: tool_use input unmarshal failed", "tool", name, "id", id, "error", err)
		return map[string]any{"_raw": string(input)}
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
		// Read the union's flattened variant fields directly rather than going
		// through b.AsAny(). AsAny() delegates to AsText()/AsToolUse()/etc., each
		// of which unmarshals from ContentBlockUnion.JSON.raw — and
		// anthropic.Message.Accumulate only rewrites that raw JSON on
		// content_block_stop/message_stop (see messageutil.go). While a block is
		// still accumulating (i.e. on every message_update fired from a
		// content_block_delta), the struct fields (b.Text, b.Thinking, ...) have
		// already grown but JSON.raw has not been re-marshaled yet. So on a
		// mid-stream accumulated message every As*() returns an EMPTY block,
		// which is how streaming shipped emitting 23 content-free message_update
		// frames per turn while hasContent (reading b.Text directly) correctly
		// saw content. A complete API response populates both the fields and
		// the raw JSON, so reading fields directly is correct for both and
		// needs no second code path.
		switch b.Type {
		case "text":
			// Skip empty text. PiContentBlock.Text is `omitempty`, so an empty
			// string serializes to {type:"text"} with no `text` field, and pi's
			// TUI does c.text.trim() → crashes on the undefined field. Mirrors
			// the same guard in provider_claude_state.go's emitAssistant.
			if b.Text != "" {
				blocks = append(blocks, child.PiTextBlock(b.Text))
			}
		case "thinking":
			// Same hazard for thinking: an empty string would serialize to
			// {type:"thinking"} (no `thinking` field) and crash pi's TUI at
			// c.thinking.trim(). Skip empty thinking blocks entirely.
			if b.Thinking != "" {
				blocks = append(blocks, child.PiThinkingBlock(b.Thinking))
			}
		case "tool_use":
			blocks = append(blocks, child.PiToolCallBlock(b.ID, b.Name, streamingToolArgs(b.ID, b.Name, b.Input)))
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
