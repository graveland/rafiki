// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"go.graveland.dev/rafiki/pkg/providers"
)

// openAISender is the Sender for providers.KindOpenAI: a generic
// OpenAI-compatible Chat Completions endpoint (Ollama, vLLM, Fireworks, …).
// It translates the Anthropic Messages params every rafiki caller builds into
// an OpenAI request and the OpenAI response back into *anthropic.Message, so
// nothing above pkg/llm learns a new wire format. Streaming shares the
// request-body builder (buildOpenAIChatRequest) with the non-streaming path.
type openAISender struct {
	baseURL string // scheme+host+path prefix, trailing "/" trimmed; never empty
	apiKey  string // empty means keyless: no Authorization header at all
	client  *http.Client
}

// newOpenAISender builds the KindOpenAI sender. The shape mirrors SenderForKey
// (provider table entry + resolved key + optional RoundTripper) so the
// SenderForKey wiring is the same one-liner the other kinds get.
//
// Two behaviors are carried over from SenderForKey exactly, not reinvented:
//
//   - Keyless convention (sender.go:189-191): an empty key sends NO
//     Authorization header at all — never "Bearer " with an empty value.
//     This is what lets an unauthenticated OpenAI-compatible server (Ollama
//     is the concrete case) work with an unset or placeholder api_key_env.
//   - Transport capture (sender.go:192-195): the client's Transport wraps
//     whatever rt was passed in with headerCaptureTransport{base: rt},
//     applied unconditionally regardless of whether rt was nil, or fundi's
//     raw-trace capture silently sees nothing for this provider.
//
// Unlike Anthropic/OpenRouter there is no canonical default base URL for a
// generic OpenAI-compatible endpoint, so an empty BaseURL is a config error
// rather than a silent default.
func newOpenAISender(p providers.Provider, key string, rt http.RoundTripper) (*openAISender, error) {
	if p.BaseURL == "" {
		return nil, fmt.Errorf("llm: provider %q: kind %q requires a base_url in providers.toml (no canonical default for OpenAI-compatible endpoints)", p.Name, p.Kind)
	}
	return &openAISender{
		baseURL: strings.TrimRight(p.BaseURL, "/"),
		apiKey:  key,
		client:  &http.Client{Transport: headerCaptureTransport{base: rt}},
	}, nil
}

// New issues one Chat Completions call and translates the response into an
// anthropic.Message. Non-2xx responses come back as *anthropic.Error carrying
// the response status — exactly what pkg/agentloop's isRetryable already
// type-switches on, so the existing retry machinery works unchanged.
func (s *openAISender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	body, err := buildOpenAIChatRequest(params, false)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Keyless convention: an empty key sends no Authorization header at all.
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: openai: post chat/completions: %w", err)
	}
	// Read the body on both paths before closing: the error path is done with
	// the status alone (apierror.Error's raw-body field is unexported and
	// cannot be populated from here), and draining keeps the connection
	// reusable instead of leaking one per failed attempt.
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &anthropic.Error{
			StatusCode: resp.StatusCode,
			Request:    req,
			Response:   resp,
		}
	}
	if readErr != nil {
		return nil, fmt.Errorf("llm: openai: read response: %w", readErr)
	}
	var wire openAIChatResponse
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return nil, fmt.Errorf("llm: openai: decode response (status %d): %w: %.200s", resp.StatusCode, err, respBody)
	}
	return parseOpenAIChatResponse(&wire)
}

// openAIChatRequest is the wire shape of one Chat Completions request. Fields
// with no analog in anthropic.MessageNewParams are deliberately absent: TopK,
// Container, InferenceGeo, CacheControl, Metadata, OutputConfig, ServiceTier
// and Thinking have no OpenAI Chat Completions equivalent and are silently
// dropped — none of them change response correctness, only Anthropic-specific
// behavior.
type openAIChatRequest struct {
	Model     string          `json:"model"`
	MaxTokens int64           `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	Messages  []openAIMessage `json:"messages,omitempty"`
	// Temperature and TopP are pointers so a present-but-zero value (a
	// meaningful choice: deterministic sampling) still marshals while an
	// unset param.Opt is omitted entirely.
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Stop        []string     `json:"stop,omitempty"`
	Tools       []openAITool `json:"tools,omitempty"`
	ToolChoice  any          `json:"tool_choice,omitempty"`
}

// openAIMessage is one entry of the Chat Completions messages array. Content
// is a pointer: absent (nil) for an assistant message that carries only
// tool_calls, explicit empty string for role "tool" (whose content is
// required by the Chat Completions schema, even when the tool result was
// empty).
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIToolCall is shared by both directions: the request embeds it inside an
// assistant message's tool_calls; the response carries the same shape back.
type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// buildOpenAIChatRequest translates MessageNewParams into a Chat Completions
// request body. stream is true for the streaming path (NewStreaming) and
// false for New — everything except the stream flag itself is identical for
// both, so this is the one place the request shape lives.
func buildOpenAIChatRequest(params anthropic.MessageNewParams, stream bool) ([]byte, error) {
	req := openAIChatRequest{
		Model:     params.Model,
		MaxTokens: params.MaxTokens,
		Stream:    stream,
		Messages:  make([]openAIMessage, 0, len(params.Messages)+1),
	}
	if params.Temperature.Valid() {
		t := params.Temperature.Value
		req.Temperature = &t
	}
	if params.TopP.Valid() {
		t := params.TopP.Value
		req.TopP = &t
	}
	if len(params.StopSequences) > 0 {
		req.Stop = params.StopSequences
	}

	// Anthropic's system is a top-level field; OpenAI has no such field — it
	// is a message. Concatenate every block's text in order, one system
	// message, not one per block.
	if len(params.System) > 0 {
		var sb strings.Builder
		for _, b := range params.System {
			sb.WriteString(b.Text)
		}
		req.Messages = append(req.Messages, openAIMessage{Role: openAIRoleSystem, Content: openAIStringPtr(sb.String())})
	}

	for i := range params.Messages {
		msgs, err := translateOpenAIMessage(&params.Messages[i])
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msgs...)
	}

	for _, t := range params.Tools {
		if t.OfTool == nil {
			// Server-side tools (web_search, bash_20250124, code_execution, …)
			// have no OpenAI function equivalent; dropped like the scalar
			// no-analog fields above.
			slog.Debug("llm: openai: dropping tool with no OpenAI function equivalent")
			continue
		}
		tp := t.OfTool
		def := openAIToolDef{Name: tp.Name}
		if tp.Description.Valid() {
			def.Description = tp.Description.Value
		}
		schema := map[string]any{"type": "object"}
		if s := string(tp.InputSchema.Type); s != "" {
			schema["type"] = s
		}
		if tp.InputSchema.Properties != nil {
			schema["properties"] = tp.InputSchema.Properties
		}
		if len(tp.InputSchema.Required) > 0 {
			schema["required"] = tp.InputSchema.Required
		}
		def.Parameters = schema
		req.Tools = append(req.Tools, openAITool{Type: "function", Function: def})
	}
	// tool_choice without tools is an API error on OpenAI, so it is only sent
	// when at least one function tool survived translation.
	if len(req.Tools) > 0 {
		req.ToolChoice = translateOpenAIToolChoice(params.ToolChoice)
	}

	return json.Marshal(req)
}

// translateOpenAIMessage maps one Anthropic message onto one or more OpenAI
// messages: a single Anthropic message can carry multiple content blocks, and
// tool_use / tool_result blocks become their own OpenAI messages rather than
// parts of the assistant/user message.
func translateOpenAIMessage(mp *anthropic.MessageParam) ([]openAIMessage, error) {
	switch mp.Role {
	case "assistant":
		return translateOpenAIAssistantMessage(mp)
	case "user":
		return translateOpenAIUserMessage(mp)
	default:
		return nil, fmt.Errorf("llm: openai: message role %q has no OpenAI equivalent", mp.Role)
	}
}

// translateOpenAIAssistantMessage produces one OpenAI assistant message:
// content from concatenated text blocks (if any) plus one tool_call entry per
// tool_use block (if any). A message with neither (e.g. thinking-only
// history) produces nothing — an empty assistant message has no meaning on
// the wire.
func translateOpenAIAssistantMessage(mp *anthropic.MessageParam) ([]openAIMessage, error) {
	var text strings.Builder
	var calls []openAIToolCall
	for i := range mp.Content {
		b := &mp.Content[i]
		switch {
		case b.OfText != nil:
			text.WriteString(b.OfText.Text)
		case b.OfToolUse != nil:
			tu := b.OfToolUse
			args, err := openAIToolArguments(tu.Input)
			if err != nil {
				return nil, fmt.Errorf("llm: openai: tool_use %q: %w", tu.ID, err)
			}
			calls = append(calls, openAIToolCall{
				ID:       tu.ID,
				Type:     "function",
				Function: openAIToolFunction{Name: tu.Name, Arguments: args},
			})
		default:
			logDroppedContentBlock(b, "assistant")
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil, nil
	}
	m := openAIMessage{Role: openAIRoleAssistant, ToolCalls: calls}
	if text.Len() > 0 {
		m.Content = openAIStringPtr(text.String())
	}
	return []openAIMessage{m}, nil
}

// translateOpenAIUserMessage produces one role:"tool" message per tool_result
// block plus, if the message carries text, one user message. Tool messages
// come FIRST regardless of block order: OpenAI requires every role:"tool"
// message to directly follow the assistant message whose tool_calls it
// answers, and a user message sandwiched in between is an API error
// ("An assistant message with 'tool_calls' must be followed by tool messages
// responding to each 'tool_call_id'"). The common rafiki shapes — a
// tool_result-only user message, a text-only user message — are unaffected.
func translateOpenAIUserMessage(mp *anthropic.MessageParam) ([]openAIMessage, error) {
	var text strings.Builder
	var toolMsgs []openAIMessage
	for i := range mp.Content {
		b := &mp.Content[i]
		switch {
		case b.OfText != nil:
			text.WriteString(b.OfText.Text)
		case b.OfToolResult != nil:
			tr := b.OfToolResult
			var sb strings.Builder
			for _, c := range tr.Content {
				if c.OfText != nil {
					sb.WriteString(c.OfText.Text)
				}
			}
			toolMsgs = append(toolMsgs, openAIMessage{
				Role:       openAIRoleTool,
				ToolCallID: tr.ToolUseID,
				Content:    openAIStringPtr(sb.String()), // content is required for role:"tool", even empty
			})
		default:
			logDroppedContentBlock(b, "user")
		}
	}
	if text.Len() == 0 && len(toolMsgs) == 0 {
		return nil, nil
	}
	out := toolMsgs
	if text.Len() > 0 {
		out = append(out, openAIMessage{Role: openAIRoleUser, Content: openAIStringPtr(text.String())})
	}
	return out, nil
}

// openAIToolArguments renders a tool_use Input as the JSON string Chat
// Completions carries in function.arguments. A nil input is an empty object.
func openAIToolArguments(input any) (string, error) {
	if input == nil {
		return "{}", nil
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal input: %w", err)
	}
	return string(b), nil
}

// translateOpenAIToolChoice maps Anthropic's tool_choice onto OpenAI's:
// auto → "auto", any → "required", tool → {"type":"function","function":
// {"name":…}}, none → "none". A zero union (no arm set) maps to nil, which
// omits the field.
func translateOpenAIToolChoice(tc anthropic.ToolChoiceUnionParam) any {
	switch {
	case tc.OfAuto != nil:
		return "auto"
	case tc.OfAny != nil:
		return "required"
	case tc.OfTool != nil:
		return openAIToolChoiceFunction{Type: "function", Function: openAIToolChoiceName{Name: tc.OfTool.Name}}
	case tc.OfNone != nil:
		return "none"
	default:
		return nil
	}
}

// logDroppedContentBlock reports a content block this translation cannot
// represent. Images and documents carry user content, so losing them is worth
// a warning; thinking blocks are routine for thinking models and carry no
// user content.
func logDroppedContentBlock(b *anthropic.ContentBlockParamUnion, role string) {
	name := "unknown"
	if t := b.GetType(); t != nil && *t != "" {
		name = *t
	}
	loud := name == "image" || name == "document" || name == "search_result"
	msg := "llm: openai: dropping content block with no OpenAI Chat Completions equivalent"
	if loud {
		slog.Warn(msg, "role", role, "type", name)
	} else {
		slog.Debug(msg, "role", role, "type", name)
	}
}

func openAIStringPtr(s string) *string { return &s }

// Wire roles, per the Chat Completions spec.
const (
	openAIRoleSystem    = "system"
	openAIRoleUser      = "user"
	openAIRoleAssistant = "assistant"
	openAIRoleTool      = "tool"
)

// openAITool is one entry of the Chat Completions tools array.
type openAITool struct {
	Type     string        `json:"type"` // always "function"
	Function openAIToolDef `json:"function"`
}

type openAIToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// openAIToolChoiceFunction is the {"type":"function","function":{"name":…}}
// form a named tool_choice takes on the wire.
type openAIToolChoiceFunction struct {
	Type     string               `json:"type"`
	Function openAIToolChoiceName `json:"function"`
}

type openAIToolChoiceName struct {
	Name string `json:"name"`
}

// openAIChatResponse is the wire shape of one Chat Completions response. Only
// the fields this translation consumes are declared; everything else
// (created, object, service_tier, …) is ignored.
type openAIChatResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
	Usage   *openAIChatUsage   `json:"usage,omitempty"`
}

type openAIChatChoice struct {
	Index        int                   `json:"index"`
	Message      openAIResponseMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type openAIResponseMessage struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIChatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// parseOpenAIChatResponse translates a Chat Completions response into an
// *anthropic.Message. Role and Type are the SDK's constant.Assistant and
// constant.Message values: Chat Completions only ever completes as the
// assistant, and rafiki's consumers key off the "message" type.
func parseOpenAIChatResponse(resp *openAIChatResponse) (*anthropic.Message, error) {
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("llm: openai: completion response carries no choices")
	}
	choice := resp.Choices[0]
	msg := &anthropic.Message{
		ID:    resp.ID,
		Model: resp.Model,
		Role:  "assistant", // constant.Assistant
		Type:  "message",   // constant.Message
	}
	// A text content block only if the completion actually said something:
	// content is null on tool-call-only completions, and a zero-length text
	// block would be noise.
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		msg.Content = append(msg.Content, anthropic.ContentBlockUnion{Type: "text", Text: *choice.Message.Content})
	}
	for _, c := range choice.Message.ToolCalls {
		input, err := openAIToolCallInput(c.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("llm: openai: tool call %q: %w", c.ID, err)
		}
		msg.Content = append(msg.Content, anthropic.ContentBlockUnion{
			Type:  "tool_use",
			ID:    c.ID,
			Name:  c.Function.Name,
			Input: input,
		})
	}
	msg.StopReason = openAIStopReason(choice.FinishReason)
	if resp.Usage != nil {
		msg.Usage = anthropic.Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}
	return msg, nil
}

// openAIToolCallInput converts the arguments STRING Chat Completions ships
// (function arguments are JSON-encoded, not a JSON object) back into the
// raw JSON anthropic.ContentBlockUnion.Input carries. json.Compact validates
// and normalizes in one step — a malformed arguments string fails here with a
// clear error instead of when a tool executor tries to unmarshal it later;
// an empty string is accepted as {} (some local servers send "" for
// parameterless calls).
func openAIToolCallInput(arguments string) (json.RawMessage, error) {
	if arguments == "" {
		return json.RawMessage("{}"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(arguments)); err != nil {
		return nil, fmt.Errorf("tool arguments are not valid JSON (%.200q): %w", arguments, err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// openAIStopReason maps OpenAI's finish_reason onto Anthropic's StopReason.
// An unrecognized value maps to end_turn with a warning rather than an error:
// a vendor string this mapping has not seen yet must not fail an
// otherwise-healthy turn.
func openAIStopReason(finish string) anthropic.StopReason {
	switch finish {
	case "stop":
		return anthropic.StopReasonEndTurn
	case "length":
		return anthropic.StopReasonMaxTokens
	case "tool_calls":
		return anthropic.StopReasonToolUse
	case "content_filter":
		return anthropic.StopReasonRefusal
	default:
		slog.Warn("llm: openai: unrecognized finish_reason, mapping to end_turn", "finish_reason", finish)
		return anthropic.StopReasonEndTurn
	}
}

// NewStreaming opens one Chat Completions call with "stream": true and returns
// the response translated into the Anthropic stream event sequence, so callers
// see the same *ssestream.Stream[anthropic.MessageStreamEventUnion] the
// Anthropic and OpenRouter senders produce. The request body comes from the
// same buildOpenAIChatRequest seam as New (with stream: true) — nothing about
// the request shape is re-derived here — and the path, Content-Type and the
// keyless Authorization convention match New exactly.
func (s *openAISender) NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	body, err := buildOpenAIChatRequest(params, true)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Keyless convention: an empty key sends no Authorization header at all.
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: openai: post chat/completions: %w", err)
	}
	// Non-2xx before any streaming starts: identical error construction to
	// New — *anthropic.Error carrying the status plus the request and
	// response, the shape pkg/agentloop's isRetryable type-switches on.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain before closing so the connection stays reusable (same
		// rationale as New); the status alone is what carries.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, &anthropic.Error{
			StatusCode: resp.StatusCode,
			Request:    req,
			Response:   resp,
		}
	}
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](newOpenAISSEDecoder(resp.Body), nil), nil
}

// openAISSEDecoder adapts an OpenAI Chat Completions text/event-stream body to
// ssestream.Decoder. Each `data:` line carries one chat.completion.chunk (or
// the `[DONE]` terminator); every chunk is translated into zero or more
// Anthropic stream events, which Next() hands out one at a time — one chunk
// can legitimately translate to several events (a content_block_start
// followed by its first delta), and some chunks (role-only openers) translate
// to none, so events are queued between pulls.
//
// The emitted Event.Type is the ANTHROPIC event type — that is the value
// ssestream.Stream.Next switches on — and Event.Data the full JSON for that
// MessageStreamEventUnion variant. Any failure after the connection is up
// (a chunk that does not parse, a transport error mid-body, a stream that
// ends without a finish_reason) surfaces as one in-band
// {"type":"error","error":{…}} event using pkg/llm/streamerr.go's own
// vocabulary: the generic Stream.Next wraps that Data into exactly the string
// ParseStreamError expects, so no error classification lives here.
type openAISSEDecoder struct {
	rc  io.ReadCloser
	scn *bufio.Scanner

	queue []ssestream.Event // translated events awaiting delivery
	cur   ssestream.Event
	err   error // always nil: in-band failures travel as error events

	closed bool
	done   bool // terminal: nothing left to pull or deliver

	// Translation state.
	started      bool // message_start emitted
	finishSeen   bool // finish_reason chunk processed (message_delta + message_stop emitted)
	activeValid  bool // a content block is currently open
	activeRef    openAIBlockRef
	activeIndex  int64 // Anthropic index of the open block
	blocksIssued int64 // Anthropic content-block indexes handed out so far
	lastUsage    *openAIChatUsage
}

// openAIBlockRef identifies the OpenAI source a translated content block
// streams from: a text block keyed by choice index, or a tool_use block keyed
// by tool-call index (only choices[0] is translated — rafiki never requests
// more than one choice, and buildOpenAIChatRequest sends no `n`).
//
// Only one block is ever open at a time. The SDK's Message.Accumulate appends
// a content block per content_block_start and applies every
// content_block_delta to the LAST block regardless of its Index, so deltas
// across blocks must never interleave: whenever a different source starts
// streaming, the open block is closed (content_block_stop) before the new one
// opens, keeping every delta adjacent to its own content_block_start.
type openAIBlockRef struct {
	kind  string // openAIBlockText or openAIBlockTool
	index int    // choice index for text, tool-call index for tool
}

const (
	openAIBlockText = "text"
	openAIBlockTool = "tool"
)

func newOpenAISSEDecoder(rc io.ReadCloser) *openAISSEDecoder {
	scn := bufio.NewScanner(rc)
	// Same ceiling the SDK's own SSE decoder sets; long tool-argument or
	// text chunks are the biggest lines in practice.
	scn.Buffer(nil, bufio.MaxScanTokenSize<<9)
	return &openAISSEDecoder{rc: rc, scn: scn}
}

func (d *openAISSEDecoder) Event() ssestream.Event { return d.cur }

func (d *openAISSEDecoder) Err() error { return d.err }

func (d *openAISSEDecoder) Close() error {
	d.closed = true
	return d.rc.Close()
}

func (d *openAISSEDecoder) Next() bool {
	if d.closed || d.err != nil {
		return false
	}
	for len(d.queue) == 0 && !d.done {
		d.pull()
	}
	if len(d.queue) == 0 {
		return false
	}
	d.cur = d.queue[0]
	d.queue = d.queue[1:]
	return true
}

// pull consumes ONE SSE event block — the accumulated `data:` lines up to the
// next blank line — and dispatches it. SSE comment lines (OpenRouter's
// `: OPENROUTER PROCESSING` keep-alives) and non-data fields are skipped;
// split data lines are joined with '\n' as the SSE spec requires.
func (d *openAISSEDecoder) pull() {
	var data []byte
	for d.scn.Scan() {
		line := d.scn.Bytes()
		if len(line) == 0 {
			if len(data) == 0 {
				continue // blank line between events
			}
			d.dispatch(data)
			return
		}
		if line[0] == ':' {
			continue
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		if string(name) != "data" {
			continue // "event:" etc. — the emitted type is the translated one
		}
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		data = append(data, value...)
		data = append(data, '\n')
	}
	// The scanner stopped: a transport error mid-body, or clean EOF — possibly
	// with one last event block that lacked its terminating blank line.
	if err := d.scn.Err(); err != nil {
		// After finish_reason the message is complete; a read failure in the
		// trailer must not fail an already-delivered turn.
		if !d.finishSeen {
			d.emitStreamError("api_error", fmt.Sprintf("llm: openai: stream transport error: %v", err))
		}
		d.done = true
		return
	}
	if len(data) > 0 {
		d.dispatch(data)
		return
	}
	if !d.finishSeen {
		// A stream that ends with no finish_reason never completed the
		// message: surfacing it beats silently persisting a truncated
		// response as a complete turn.
		d.emitStreamError("api_error", "llm: openai: stream ended without a finish_reason (response truncated)")
	}
	d.done = true
}

// dispatch translates one SSE data payload (a chat.completion.chunk, the
// [DONE] sentinel, or garbage that must become an in-band error).
func (d *openAISSEDecoder) dispatch(data []byte) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		// [DONE] without a preceding finish_reason is a truncated stream, not
		// a completed one.
		if !d.finishSeen {
			d.emitStreamError("api_error", "llm: openai: stream ended ([DONE]) without a finish_reason (response truncated)")
		}
		d.done = true
		return
	}
	var chunk openAIStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		d.emitStreamError("api_error", fmt.Sprintf("llm: openai: stream chunk failed to parse as a chat.completion.chunk: %v: %.500s", err, data))
		return
	}
	if len(chunk.Choices) == 0 && chunk.Usage == nil {
		// Parses as JSON but is not a chunk: this is how some servers stream
		// an in-band {"error":{…}} object instead of an HTTP status.
		d.emitStreamError("api_error", fmt.Sprintf("llm: openai: stream carried a non-chunk payload: %.500s", data))
		return
	}
	d.translate(&chunk)
}

// translate maps one chat.completion.chunk onto zero or more Anthropic
// events, per the brief's numbered cases:
//
//  1. first chunk received        -> message_start
//  2. first text delta for a      -> content_block_start, then the delta (3)
//     choice index
//  3. every text delta            -> content_block_delta (text_delta)
//  4. first arguments delta for   -> content_block_start, then the delta (5)
//     a tool-call index
//  5. every arguments delta       -> content_block_delta (input_json_delta,
//     raw fragment forwarded unmodified)
//  6. a block's source stops      -> content_block_stop, emitted lazily the
//     appearing                      moment anything else needs to speak
//  7. the finish_reason chunk     -> content_block_stop for the open block,
//     message_delta (stop_reason via
//     openAIStopReason — the same five-way
//     table New uses), message_stop
//  8. anything untranslatable     -> one in-band error event (api_error)
func (d *openAISSEDecoder) translate(chunk *openAIStreamChunk) {
	if d.finishSeen {
		// Trailer after the finish_reason chunk (e.g. OpenAI's usage-only tail
		// under stream_options.include_usage, which rafiki's own requests do
		// not set): the message is complete, ignore the rest.
		return
	}
	if chunk.Usage != nil {
		d.lastUsage = chunk.Usage
	}
	if !d.started {
		d.started = true
		d.emitMessageStart(chunk.ID, chunk.Model)
	}
	if len(chunk.Choices) == 0 {
		return // usage-only trailer chunk
	}
	choice := &chunk.Choices[0]

	// A present-but-empty content (the role-only opener chunk) contributes
	// nothing — matching New(), which emits no text block for a zero-length
	// completion.
	if choice.Delta.Content != nil && *choice.Delta.Content != "" {
		ref := openAIBlockRef{kind: openAIBlockText, index: choice.Index}
		if !d.activeValid || d.activeRef != ref {
			d.openTextBlock(choice.Index)
		}
		d.emitTextDelta(d.activeIndex, *choice.Delta.Content)
	}

	for i := range choice.Delta.ToolCalls {
		tc := &choice.Delta.ToolCalls[i]
		if tc.Function == nil && tc.ID == "" {
			continue // an entry carrying only an index has nothing to emit
		}
		ref := openAIBlockRef{kind: openAIBlockTool, index: tc.Index}
		if !d.activeValid || d.activeRef != ref {
			d.openToolBlock(tc)
		}
		if tc.Function != nil && tc.Function.Arguments != "" {
			// Forward the fragment exactly as OpenAI sent it: partial_json is
			// an incremental fragment, never a re-emitted snapshot.
			d.emitInputJSONDelta(d.activeIndex, tc.Function.Arguments)
		}
	}

	if choice.FinishReason != nil && *choice.FinishReason != "" {
		d.finishSeen = true
		d.closeActiveBlock()
		d.emitMessageDelta(openAIStopReason(*choice.FinishReason))
		d.emitMessageStop()
		// done is NOT set here: OpenAI may still send trailing chunks (its
		// [DONE] sentinel at minimum), and reading them to the end is what
		// lets the connection close cleanly.
	}
}

// openTextBlock closes any open block (case 6) and starts a text block (case
// 2). Indexes are issued sequentially because Message.Accumulate appends a
// block per content_block_start and ignores the carried Index.
func (d *openAISSEDecoder) openTextBlock(choiceIndex int) {
	d.closeActiveBlock()
	d.activeRef = openAIBlockRef{kind: openAIBlockText, index: choiceIndex}
	d.activeIndex = d.blocksIssued
	d.blocksIssued++
	d.activeValid = true
	d.emit("content_block_start", openAIEventContentBlockStart{
		Type:         "content_block_start",
		Index:        int64(d.activeIndex),
		ContentBlock: openAIEventTextBlock{Type: "text"},
	})
}

// openToolBlock closes any open block (case 6) and starts a tool_use block
// (case 4). id and name arrive once, on the first chunk for that tool call;
// input starts as the empty object a parameterless call legitimately ends up
// with.
func (d *openAISSEDecoder) openToolBlock(tc *openAIStreamToolCall) {
	d.closeActiveBlock()
	d.activeRef = openAIBlockRef{kind: openAIBlockTool, index: tc.Index}
	d.activeIndex = d.blocksIssued
	d.blocksIssued++
	d.activeValid = true
	name := ""
	if tc.Function != nil {
		name = tc.Function.Name
	}
	d.emit("content_block_start", openAIEventContentBlockStart{
		Type:  "content_block_start",
		Index: int64(d.activeIndex),
		ContentBlock: openAIEventToolBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  name,
			Input: struct{}{},
		},
	})
}

// closeActiveBlock emits content_block_stop for the open block, if any
// (case 6).
func (d *openAISSEDecoder) closeActiveBlock() {
	if !d.activeValid {
		return
	}
	d.emit("content_block_stop", openAIEventContentBlockStop{
		Type:  "content_block_stop",
		Index: d.activeIndex,
	})
	d.activeValid = false
}

// emitMessageDelta is case 7's message_delta: the stop_reason mapped through
// openAIStopReason — the SAME five-way table New uses, not a second copy — and
// the finish chunk's usage if it carried one, else the last usage seen on any
// chunk. (Accumulate merges only OutputTokens from a delta;
// pkg/llm/client.go's backfillDeltaUsage picks up the input count.)
func (d *openAISSEDecoder) emitMessageDelta(stop anthropic.StopReason) {
	var usage openAIEventEnvelopeUsage
	if d.lastUsage != nil {
		usage = openAIEventEnvelopeUsage{
			InputTokens:  d.lastUsage.PromptTokens,
			OutputTokens: d.lastUsage.CompletionTokens,
		}
	}
	d.emit("message_delta", openAIEventMessageDelta{
		Type: "message_delta",
		Delta: openAIEventMessageDeltaDelta{
			StopReason: stop,
		},
		Usage: usage,
	})
}

func (d *openAISSEDecoder) emitMessageStart(id, model string) {
	d.emit("message_start", openAIEventMessageStart{
		Type: "message_start",
		Message: openAIEventMessage{
			ID:      id,
			Model:   model,
			Role:    "assistant", // constant.Assistant
			Type:    "message",   // constant.Message
			Content: []any{},
			Usage:   openAIEventEnvelopeUsage{}, // real numbers are not known yet
		},
	})
}

func (d *openAISSEDecoder) emitTextDelta(index int64, text string) {
	d.emit("content_block_delta", openAIEventContentBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: openAIEventTextDelta{Type: "text_delta", Text: text},
	})
}

func (d *openAISSEDecoder) emitInputJSONDelta(index int64, fragment string) {
	d.emit("content_block_delta", openAIEventContentBlockDelta{
		Type:  "content_block_delta",
		Index: index,
		Delta: openAIEventInputJSONDelta{Type: "input_json_delta", PartialJSON: fragment},
	})
}

func (d *openAISSEDecoder) emitMessageStop() {
	d.emit("message_stop", openAIEventMessageStop{Type: "message_stop"})
}

// emit marshals one translated event and queues it. These payload shapes are
// fixed (strings and integers only), so a marshal failure cannot happen in
// practice; if one ever did, it must not wedge the stream — it becomes an
// in-band api_error like any other untranslatable input.
func (d *openAISSEDecoder) emit(eventType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		d.emitStreamError("api_error", fmt.Sprintf("llm: openai: marshal %s event: %v", eventType, err))
		return
	}
	d.queue = append(d.queue, ssestream.Event{Type: eventType, Data: data})
}

// emitStreamError queues the in-band Anthropic error envelope —
// {"type":"error","error":{"type":…,"message":…}} — that the generic
// ssestream.Stream.Next wraps into exactly the string ParseStreamError parses.
// errType comes from pkg/llm/streamerr.go's vocabulary only; api_error is the
// safe transient default when the specific cause is not known. Emitting the
// event is the whole job: constructing an error value here (fmt.Errorf) would
// bypass that wrapping and land in ParseStreamError's not-this-shape branch.
// The stream is terminal after an error event.
func (d *openAISSEDecoder) emitStreamError(errType, message string) {
	d.emit("error", openAIEventStreamError{
		Type:  "error",
		Error: openAIEventErrorBody{Type: errType, Message: message},
	})
	d.done = true
}

// openAIStreamChunk is the wire shape of one chat.completion.chunk SSE event.
// Only the fields this translation consumes are declared.
type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *openAIChatUsage     `json:"usage"`
}

type openAIStreamChoice struct {
	Index int               `json:"index"`
	Delta openAIStreamDelta `json:"delta"`
	// FinishReason is a pointer so null/absent (every non-terminal chunk) is
	// distinct from a present value; "" is treated like absent.
	FinishReason *string `json:"finish_reason"`
}

type openAIStreamDelta struct {
	Role      string                 `json:"role"`
	Content   *string                `json:"content"`
	ToolCalls []openAIStreamToolCall `json:"tool_calls"`
}

// openAIStreamToolCall is the delta form of openAIToolCall: id, type and name
// arrive once, on the first chunk for a tool call; later chunks carry only
// the index and the next function.arguments fragment.
type openAIStreamToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function *openAIToolFunction `json:"function"`
}

// Wire payloads of the Anthropic stream events this decoder emits. They are
// minimal on purpose: ssestream.Stream.Next unmarshals Event.Data into the
// flat MessageStreamEventUnion, so a subset of each variant's real fields is
// sufficient, and emitting only what OpenAI actually sent keeps the
// translated wire honest (a marshalled SDK union would carry every variant's
// zero fields).
type openAIEventEnvelopeUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type openAIEventMessage struct {
	ID      string                   `json:"id"`
	Model   string                   `json:"model"`
	Role    string                   `json:"role"`
	Type    string                   `json:"type"`
	Content []any                    `json:"content"`
	Usage   openAIEventEnvelopeUsage `json:"usage"`
}

type openAIEventMessageStart struct {
	Type    string             `json:"type"`
	Message openAIEventMessage `json:"message"`
}

type openAIEventTextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type openAIEventToolBlock struct {
	Type  string `json:"type"` // "tool_use"
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

type openAIEventContentBlockStart struct {
	Type         string `json:"type"`
	Index        int64  `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type openAIEventContentBlockDelta struct {
	Type  string `json:"type"`
	Index int64  `json:"index"`
	Delta any    `json:"delta"`
}

type openAIEventContentBlockStop struct {
	Type  string `json:"type"`
	Index int64  `json:"index"`
}

type openAIEventTextDelta struct {
	Type string `json:"type"` // "text_delta"
	Text string `json:"text"`
}

type openAIEventInputJSONDelta struct {
	Type        string `json:"type"` // "input_json_delta"
	PartialJSON string `json:"partial_json"`
}

type openAIEventMessageDeltaDelta struct {
	StopReason anthropic.StopReason `json:"stop_reason"`
}

type openAIEventMessageDelta struct {
	Type  string                       `json:"type"`
	Delta openAIEventMessageDeltaDelta `json:"delta"`
	Usage openAIEventEnvelopeUsage     `json:"usage"`
}

type openAIEventMessageStop struct {
	Type string `json:"type"`
}

type openAIEventStreamError struct {
	Type  string               `json:"type"`
	Error openAIEventErrorBody `json:"error"`
}

type openAIEventErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
