// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

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
