// Package openaichat is the codec between rafiki's native message model
// (pkg/msg) and OpenAI's Chat Completions wire format, the one a generic
// OpenAI-compatible endpoint (Ollama, vLLM, Fireworks, …) speaks.
//
// It is stdlib-only: the dialect is a plain JSON request body, a plain JSON
// response, and a text/event-stream of JSON chunks, so nothing here needs a
// provider SDK. Blocks the dialect cannot express are dropped rather than
// approximated — a thinking block is never replayed, an Opaque block (a block
// this model has no native core for) is never guessed at, and a block's Ext
// residue is ignored entirely, since only the dialect that produced it may
// re-emit it.
package openaichat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
)

// Wire roles, per the Chat Completions spec.
const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// droppedBlockMessage is the log line for a block this dialect cannot carry. It
// keeps the wording the previous sender used, so an operator grepping a log for
// it still finds it.
const droppedBlockMessage = "llm: openai: dropping content block with no OpenAI Chat Completions equivalent"

// chatRequest is the wire shape of one Chat Completions request. Fields the
// native request has no source for are deliberately absent: top_p, stop and n
// have no counterpart in msg.Request, and none of them changes response
// correctness.
type chatRequest struct {
	Model     string        `json:"model"`
	MaxTokens int64         `json:"max_tokens"`
	Stream    bool          `json:"stream"`
	Messages  []chatMessage `json:"messages,omitempty"`
	// Temperature is a pointer so a present-but-zero value (a meaningful
	// choice: deterministic sampling) still marshals, while an unset one is
	// omitted.
	Temperature *float64   `json:"temperature,omitempty"`
	Tools       []chatTool `json:"tools,omitempty"`
	ToolChoice  any        `json:"tool_choice,omitempty"`
}

// chatMessage is one entry of the messages array. Content is a pointer: absent
// for an assistant message that carries only tool_calls, and an explicit empty
// string for role "tool", whose content the schema requires even when the tool
// result was empty.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

// chatToolCall is shared by both directions: a request embeds it in an
// assistant message's tool_calls, and a response carries the same shape back.
type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatTool is one entry of the tools array.
type chatTool struct {
	Type     string      `json:"type"` // always "function"
	Function chatToolDef `json:"function"`
}

type chatToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// chatToolChoiceFunction is the {"type":"function","function":{"name":…}} form
// a forced tool takes on the wire.
type chatToolChoiceFunction struct {
	Type     string           `json:"type"`
	Function chatToolChoiceFn `json:"function"`
}

type chatToolChoiceFn struct {
	Name string `json:"name"`
}

// Encode translates req into a Chat Completions request body. Stream is the
// only EncodeOptions field that reaches the body: the session id rides a
// header the sender owns, and req.Cache has no Chat Completions equivalent.
// Extra merges top-level keys afterwards, which is how an OpenRouter caller
// sets provider routing.
//
// A request whose user message carries blocks none of which survive
// translation is an error rather than a silently shortened turn: total loss of
// user content is a limitation to fail on.
func Encode(req *msg.Request, opts msg.EncodeOptions) ([]byte, error) {
	body := chatRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Stream:      opts.Stream,
		Messages:    make([]chatMessage, 0, len(req.Messages)+1),
		Temperature: req.Temperature,
	}

	// The native request's system is a top-level field; Chat Completions has no
	// such field — it is a message. Concatenate every system text in order into
	// one message, not one per block.
	if len(req.System) > 0 {
		var sb strings.Builder
		for _, s := range req.System {
			sb.WriteString(s.Text)
		}
		body.Messages = append(body.Messages, chatMessage{Role: roleSystem, Content: stringPtr(sb.String())})
	}

	for _, m := range req.Messages {
		translated, err := translateMessage(m)
		if err != nil {
			return nil, err
		}
		body.Messages = append(body.Messages, translated...)
	}

	for _, t := range req.Tools {
		params, err := toolParameters(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("openaichat: tool %q: %w", t.Name, err)
		}
		body.Tools = append(body.Tools, chatTool{
			Type:     "function",
			Function: chatToolDef{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	// tool_choice without tools is an API error on Chat Completions, so it is
	// only sent when at least one tool survived.
	if len(body.Tools) > 0 {
		body.ToolChoice = toolChoice(req.ForceTool)
	}

	return marshalWithExtra(body, opts.Extra)
}

// translateMessage maps one native message onto one or more Chat Completions
// messages: a single native message can carry several blocks, and a tool_use
// or tool_result block becomes a message of its own rather than a part of the
// surrounding one.
func translateMessage(m msg.Message) ([]chatMessage, error) {
	switch m.Role {
	case msg.Assistant:
		return translateAssistant(m)
	case msg.User:
		return translateUser(m)
	}
	return nil, fmt.Errorf("openaichat: message role %q has no Chat Completions equivalent", m.Role)
}

// translateAssistant produces one assistant message: content from concatenated
// text blocks plus one tool_call per tool_use block. A message with neither —
// thinking-only history, say — produces nothing, because an empty assistant
// message has no meaning on the wire.
func translateAssistant(m msg.Message) ([]chatMessage, error) {
	var text strings.Builder
	var calls []chatToolCall
	for _, b := range m.Content {
		switch t := b.(type) {
		case msg.Text:
			text.WriteString(t.Text)
		case msg.ToolUse:
			args, err := toolArguments(t.Input)
			if err != nil {
				return nil, fmt.Errorf("openaichat: tool_use %q: %w", t.ID, err)
			}
			calls = append(calls, chatToolCall{
				ID:       t.ID,
				Type:     "function",
				Function: chatFunction{Name: t.Name, Arguments: args},
			})
		default:
			logDroppedBlock(b, "assistant")
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil, nil
	}
	out := chatMessage{Role: roleAssistant, ToolCalls: calls}
	if text.Len() > 0 {
		out.Content = stringPtr(text.String())
	}
	return []chatMessage{out}, nil
}

// translateUser produces one role:"tool" message per tool_result block plus, if
// the message carries text, one user message. Tool messages come FIRST
// regardless of block order: Chat Completions requires every role:"tool"
// message to directly follow the assistant message whose tool_calls it answers,
// and a user message sandwiched between them is an API error.
//
// A tool_result's own content contributes its text blocks only; an image inside
// one is dropped, because a role:"tool" message carries text. ToolResult.IsError
// has no equivalent and is dropped too — the failure text still reaches the
// model.
func translateUser(m msg.Message) ([]chatMessage, error) {
	var text strings.Builder
	var toolMsgs []chatMessage
	for _, b := range m.Content {
		switch t := b.(type) {
		case msg.Text:
			text.WriteString(t.Text)
		case msg.ToolResult:
			var sb strings.Builder
			for _, c := range t.Content {
				if tx, ok := c.(msg.Text); ok {
					sb.WriteString(tx.Text)
				}
			}
			toolMsgs = append(toolMsgs, chatMessage{
				Role:       roleTool,
				ToolCallID: t.ToolUseID,
				Content:    stringPtr(sb.String()), // required for role:"tool", even when empty
			})
		default:
			logDroppedBlock(b, "user")
		}
	}
	if text.Len() == 0 && len(toolMsgs) == 0 {
		if len(m.Content) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("openaichat: user message contains only content blocks with no Chat Completions equivalent (e.g. images), which kind=\"openai\" providers do not support")
	}
	out := toolMsgs
	if text.Len() > 0 {
		out = append(out, chatMessage{Role: roleUser, Content: stringPtr(text.String())})
	}
	return out, nil
}

// toolParameters renders a tool's input schema as the JSON Schema object the
// wire carries in function.parameters. A tool with no schema gets the
// permissive object every parameterless tool gets.
func toolParameters(schema json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 {
		return map[string]any{"type": "object"}, nil
	}
	var params map[string]any
	if err := json.Unmarshal(trimmed, &params); err != nil {
		return nil, fmt.Errorf("input schema is not a JSON object: %w", err)
	}
	if params == nil {
		return map[string]any{"type": "object"}, nil
	}
	return params, nil
}

// toolArguments renders a tool_use's input as the JSON string the wire carries
// in function.arguments — Chat Completions carries arguments encoded, not as an
// object. Absent input is an empty object; input that is not valid JSON fails
// here rather than at the far end of the request.
func toolArguments(input json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "{}", nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", fmt.Errorf("input is not valid JSON: %w", err)
	}
	return buf.String(), nil
}

// toolChoice maps the native forced-tool name onto tool_choice. An empty name
// means the model decides, which is expressed by sending no tool_choice at all
// rather than the string "auto".
func toolChoice(name string) any {
	if name == "" {
		return nil
	}
	return chatToolChoiceFunction{Type: "function", Function: chatToolChoiceFn{Name: name}}
}

// logDroppedBlock reports a content block this translation cannot represent.
// Images and documents carry user content, so losing one is worth a warning;
// thinking blocks are routine for a thinking model and carry none.
func logDroppedBlock(b msg.Block, role string) {
	name := blockName(b)
	loud := name == "image" || name == "document" || name == "search_result"
	if loud {
		slog.Warn(droppedBlockMessage, "role", role, "type", name)
		return
	}
	slog.Debug(droppedBlockMessage, "role", role, "type", name)
}

// blockName returns the wire name of a block, for a log line. An Opaque block
// reports the type it declares, so a dropped server-tool block is still named.
func blockName(b msg.Block) string {
	switch t := b.(type) {
	case msg.Text:
		return "text"
	case msg.Thinking:
		return "thinking"
	case msg.ToolUse:
		return "tool_use"
	case msg.ToolResult:
		return "tool_result"
	case msg.Image:
		return "image"
	case msg.Opaque:
		if name := msg.OpaqueType(t); name != "" {
			return name
		}
	}
	return "unknown"
}

// marshalWithExtra marshals the body and merges extra's top-level keys into it.
// The round trip is what lets a caller add fields this codec does not model
// (OpenRouter's provider routing, a provider's own knobs); an extra key wins
// over the codec's own, because the caller asked for it explicitly.
func marshalWithExtra(body chatRequest, extra map[string]any) ([]byte, error) {
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaichat: encode request: %w", err)
	}
	if len(extra) == 0 {
		return out, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		return nil, fmt.Errorf("openaichat: read encoded request for extra: %w", err)
	}
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("openaichat: encode extra field %q: %w", k, err)
		}
		fields[k] = raw
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("openaichat: encode request with extra: %w", err)
	}
	return merged, nil
}

func stringPtr(s string) *string { return &s }

// chatResponse is the wire shape of one Chat Completions response. Only the
// fields this codec consumes are declared; everything else (created, object,
// service_tier, …) is ignored.
type chatResponse struct {
	ID       string       `json:"id"`
	Model    string       `json:"model"`
	Provider string       `json:"provider"`
	Choices  []chatChoice `json:"choices"`
	Usage    *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      chatRespMessage `json:"message"`
	FinishReason *string         `json:"finish_reason"`
}

type chatRespMessage struct {
	Content   *string        `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls"`
}

// chatUsage is a usage object. Every count is a pointer because an absent count
// and a reported zero are different facts: cost math and the provider guard
// both need the difference.
type chatUsage struct {
	PromptTokens            *int64                 `json:"prompt_tokens"`
	CompletionTokens        *int64                 `json:"completion_tokens"`
	PromptTokensDetails     *chatPromptDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *chatCompletionDetails `json:"completion_tokens_details"`
}

type chatPromptDetails struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

type chatCompletionDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens"`
}

// DecodeResponse translates a Chat Completions response body into a native
// response. Content mapping matches the sender this codec replaces: a text
// block only when the completion actually said something, then one tool_use per
// tool call, with the arguments string compacted back into raw JSON.
func DecodeResponse(body []byte) (*msg.Response, error) {
	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("openaichat: decode response: %w", err)
	}
	if len(wire.Choices) == 0 {
		return nil, fmt.Errorf("openaichat: completion response carries no choices")
	}
	choice := wire.Choices[0]
	resp := &msg.Response{ID: wire.ID, Model: wire.Model, Provider: wire.Provider}
	// Content is null on a tool-call-only completion, and a zero-length text
	// block would be noise.
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		resp.Content = append(resp.Content, msg.Text{Text: *choice.Message.Content})
	}
	for _, c := range choice.Message.ToolCalls {
		input, err := toolCallInput(c.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("openaichat: tool call %q: %w", c.ID, err)
		}
		resp.Content = append(resp.Content, msg.ToolUse{ID: c.ID, Name: c.Function.Name, Input: input})
	}
	if choice.FinishReason != nil {
		resp.StopReason = stopReason(*choice.FinishReason)
		resp.StopReasonRaw = *choice.FinishReason
	}
	resp.Usage = decodeUsage(wire.Usage)
	return resp, nil
}

// toolCallInput converts the arguments string Chat Completions ships back into
// the raw JSON a ToolUse carries. json.Compact validates and normalizes in one
// step, so a malformed string fails here with a clear error instead of when a
// tool executor unmarshals it later. An empty string is accepted as {}, which
// some local servers send for parameterless calls.
func toolCallInput(arguments string) (json.RawMessage, error) {
	if arguments == "" {
		return json.RawMessage("{}"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(arguments)); err != nil {
		return nil, fmt.Errorf("tool arguments are not valid JSON (%.200q): %w", arguments, err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// stopReason maps Chat Completions' finish_reason onto the native stop reason.
// An unrecognised value is StopUnknown, not a clean end of turn: a stop reason
// is a report, and a vendor string this mapping has not seen yet must not be
// recorded as if the model had finished normally.
func stopReason(finish string) msg.StopReason {
	switch finish {
	case "stop":
		return msg.StopEndTurn
	case "length":
		return msg.StopMaxTokens
	case "tool_calls", "function_call":
		return msg.StopToolUse
	case "content_filter":
		return msg.StopRefusal
	case "":
		return msg.StopUnknown
	}
	slog.Warn("llm: openai: unrecognized finish_reason, mapping to unknown", "finish_reason", finish)
	return msg.StopUnknown
}

// decodeUsage reads a usage object by presence. Chat Completions'
// prompt_tokens INCLUDES the cached prefix, so a reported cached_tokens is
// subtracted out of Input and carried in CacheRead — the native model's Input is
// the uncached remainder. CacheWrite is never reported by this dialect.
func decodeUsage(u *chatUsage) msg.Usage {
	var out msg.Usage
	if u == nil {
		return out
	}
	out.Output = u.CompletionTokens
	if d := u.PromptTokensDetails; d != nil && d.CachedTokens != nil {
		out.CacheRead = msg.Ptr(*d.CachedTokens)
		if u.PromptTokens != nil {
			out.Input = msg.Ptr(max(*u.PromptTokens-*d.CachedTokens, 0))
		}
	} else {
		out.Input = u.PromptTokens
	}
	if d := u.CompletionTokensDetails; d != nil {
		out.Reasoning = d.ReasoningTokens
	}
	return out
}

// Classify maps a Chat Completions refusal onto what a sender should do about
// it. status 0 is an in-band stream error, which carries its payload instead of
// an HTTP status, so the payload rules apply there as well as at the statuses
// they usually arrive with.
//
// Anything unrecognised is ErrOther, which is not retryable: retrying an error
// nobody understood is how one failure becomes a storm. h is unused because
// this dialect signals nothing in its headers that changes the kind.
func Classify(status int, h http.Header, body []byte) msg.ErrorKind {
	payload := parseErrorPayload(body)

	// An exhausted account is not a rate limit even when it arrives as a 429:
	// retrying it can never succeed.
	if matchesCode(payload, "insufficient_quota", "usage_limit_reached", "usage_not_included") {
		return msg.ErrUsageLimit
	}
	// A prompt the model cannot hold fails identically however often it is
	// retried.
	if matchesCode(payload, "context_length_exceeded") ||
		(status == 0 || status == 400 || status == 413) &&
			containsAny(payload.message, "maximum context length", "prompt is too long") {
		return msg.ErrPromptTooLarge
	}
	// OpenRouter's "no route to this model" refusal: a retry of the same
	// request cannot pick a different provider.
	if status == 0 || status == 400 || status == 404 {
		if containsAny(payload.message, "no endpoints found", "no allowed providers") ||
			matchesCode(payload, "no endpoints found", "no allowed providers") {
			return msg.ErrNoEligible
		}
	}
	if status == http.StatusTooManyRequests || payload.numeric == http.StatusTooManyRequests ||
		matchesCode(payload, "rate_limit_error", "rate_limit_exceeded") {
		return msg.ErrRateLimit
	}
	if matchesCode(payload, "overloaded_error") {
		return msg.ErrOverloaded
	}
	if isServerStatus(status) || isServerStatus(payload.numeric) ||
		matchesCode(payload, "server_error", "api_error", "timeout_error") {
		return msg.ErrTransient
	}
	return msg.ErrOther
}

// errorPayload is what a refusal carries, whether nested under "error" or bare.
// code may be a string ("insufficient_quota") or a number (429), and a number
// may arrive quoted.
type errorPayload struct {
	code    string
	typ     string
	message string
	numeric int
}

// parseErrorPayload reads the nested or bare error object. Anything that does
// not parse leaves every field empty, which classifies on the status alone.
func parseErrorPayload(body []byte) errorPayload {
	payload := body
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if trimmed := bytes.TrimSpace(envelope.Error); len(trimmed) > 0 && string(trimmed) != "null" {
			payload = trimmed
		}
	}
	var raw struct {
		Code    json.RawMessage `json:"code"`
		Type    string          `json:"type"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return errorPayload{}
	}
	code, numeric := parseCode(raw.Code)
	return errorPayload{code: code, typ: strings.ToLower(raw.Type), message: strings.ToLower(raw.Message), numeric: numeric}
}

// parseCode reads an error code that may be a string, a number, or a number
// quoted as a string. text is the code lowercased; numeric is its value when it
// is a number at all.
func parseCode(raw json.RawMessage) (text string, numeric int) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", 0
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return strings.ToLower(s), n
		}
		return strings.ToLower(s), 0
	}
	var n int
	if err := json.Unmarshal(trimmed, &n); err == nil {
		return "", n
	}
	return "", 0
}

// matchesCode reports whether the payload's code or type is one of needles.
// parseErrorPayload already lowercased both.
func matchesCode(p errorPayload, needles ...string) bool {
	return containsAny(p.code, needles...) || containsAny(p.typ, needles...)
}

// containsAny reports whether s contains any of needles. Callers pass a
// lowercased haystack and lowercase needles.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// isServerStatus reports whether status is a server-side failure.
func isServerStatus(status int) bool { return status >= 500 && status <= 599 }
