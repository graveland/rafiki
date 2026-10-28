// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/klauspost/compress/zstd"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// ---- helpers ----

const (
	responsesFixtureTurn1 = "testdata/responses/std_turn1.response.sse"
	responsesFixtureTurn2 = "testdata/responses/std_turn2.response.sse"
	responsesFixtureReq2  = "testdata/responses/std_turn2.request.json"
)

// newResponsesSenderForTest builds a plain (api-key) sender against a fixture
// server, the way SenderForKey threads it together.
func newResponsesSenderForTest(t *testing.T, key string, handler http.HandlerFunc) *responsesSender {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := providers.Provider{Name: "fixture", Kind: providers.KindOpenAIResponses, BaseURL: srv.URL}
	s, err := newResponsesSender(p, key, nil)
	assert.NewAborting(t).NoError(err, "newResponsesSender")
	return s
}

// newChatGPTResponsesSenderForTest builds the owner-bound sender and points it at
// a fixture server (the real base URL is fixed; only tests move it).
func newChatGPTResponsesSenderForTest(t *testing.T, ts TokenSource, facts ModelFacts, handler http.HandlerFunc) *responsesSender {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	sender, err := NewChatGPTResponsesSender(providers.Provider{Name: "chatgpt", Kind: providers.KindOpenAIResponsesChatGPT}, ts, facts, nil)
	assert.NewAborting(t).NoError(err, "NewChatGPTResponsesSender")
	s, ok := sender.(*responsesSender)
	assert.NewAborting(t).True(ok, "NewChatGPTResponsesSender returned %T, want *responsesSender", sender)
	s.baseURL = srv.URL
	return s
}

// recordingTokenSource counts Token calls and the force flag of each, and can
// hand back headers alongside the bearer (the ChatGPT account id).
type recordingTokenSource struct {
	mu      sync.Mutex
	forces  []bool
	bearer  string
	headers http.Header
}

func (r *recordingTokenSource) Token(_ context.Context, force bool) (string, http.Header, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forces = append(r.forces, force)
	return r.bearer, r.headers, nil
}

func (r *recordingTokenSource) calls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.forces...)
}

// responsesSSE joins JSON payloads into an SSE body. The `event:` line is
// omitted on purpose: the decoder reads the type from the payload, which is the
// shape most real events arrive in.
func responsesSSE(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: " + p + "\n\n")
	}
	return sb.String()
}

// responsesCompleted is a minimal well-formed terminal event.
func responsesCompleted(extra string) string {
	return `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"` + extra + `}}`
}

// runResponsesStream drains a stream the way pkg/llm/client.go does, so the
// assertions cover the consumer path (Accumulate plus usage backfill).
func runResponsesStream(t *testing.T, stream *ssestream.Stream[anthropic.MessageStreamEventUnion]) (types []string, acc anthropic.Message, err error) {
	t.Helper()
	defer stream.Close()
	for stream.Next() {
		ev := stream.Current()
		types = append(types, ev.Type)
		assert.NewAborting(t).NoError(acc.Accumulate(ev), "Accumulate(%s)", ev.Type)
		backfillDeltaUsage(&acc, ev)
	}
	return types, acc, stream.Err()
}

// streamResponsesFixture replays one fixture file through a plain sender.
func streamResponsesFixture(t *testing.T, path string) ([]string, anthropic.Message, error) {
	t.Helper()
	body, err := os.ReadFile(path)
	assert.NewAborting(t).NoError(err, "read fixture")
	s := newResponsesSenderForTest(t, "k", openAIStreamHandler(string(body)))
	stream, err := s.NewStreaming(context.Background(), minimalParams())
	assert.NewAborting(t).NoError(err, "NewStreaming")
	return runResponsesStream(t, stream)
}

// readRequestBody reads a request body, transparently decompressing the zstd
// encoding ChatGPT mode sends. It returns bytes rather than a decoded map, and
// asserts nothing: request handlers run on another goroutine, where t.Fatalf is
// not allowed.
func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	assert.NewAborting(t).NoError(err, "read request body")
	if r.Header.Get("Content-Encoding") == "zstd" {
		dec, derr := zstd.NewReader(nil)
		assert.NewAborting(t).NoError(derr, "zstd reader")
		defer dec.Close()
		raw, err = dec.DecodeAll(raw, nil)
		assert.NewAborting(t).NoError(err, "zstd decode")
	}
	return raw
}

// decodeMap unmarshals a recorded body into a map.
func decodeMap(c *assert.C, raw []byte, what string) map[string]any {
	var body map[string]any
	c.Require().NoError(json.Unmarshal(raw, &body), "unmarshal %s", what)
	return body
}

// ---- golden request translation ----

// responsesGoldenParams builds the MessageParams the std_turn2 fixture was built
// from: one user turn, the reasoning item and function call the endpoint
// returned, and the tool result that answered it.
func responsesGoldenParams(t *testing.T) anthropic.MessageNewParams {
	t.Helper()
	fixture, err := os.ReadFile(responsesFixtureReq2)
	assert.NewAborting(t).NoError(err, "read request fixture")
	var wire struct {
		Input []struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"input"`
	}
	assert.NewAborting(t).NoError(json.Unmarshal(fixture, &wire), "unmarshal request fixture")
	var reasoningID, reasoningBlob string
	for _, item := range wire.Input {
		if item.Type == "reasoning" {
			reasoningID, reasoningBlob = item.ID, item.EncryptedContent
		}
	}
	assert.NewAborting(t).True(reasoningID != "", "fixture carries no reasoning item to round-trip")

	return anthropic.MessageNewParams{
		Model:     "gpt-6.1-sol",
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: "You are a careful assistant. Use the tool when asked."},
		},
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "Think step by step: a train leaves at 09:47 and the trip takes 2h 38m; what time does it arrive? Then call get_weather for that arrival city, Oslo."}},
			}},
			{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
				{OfThinking: &anthropic.ThinkingBlockParam{
					Signature: responsesSignature(reasoningID, reasoningBlob),
				}},
				{OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    "call_70b6c42145b04a3eac6712e9e23c36df",
					Name:  "get_weather",
					Input: map[string]any{"city": "Oslo"},
				}},
			}},
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: "call_70b6c42145b04a3eac6712e9e23c36df",
					Content: []anthropic.ToolResultBlockParamContentUnion{
						{OfText: &anthropic.TextBlockParam{Text: "Oslo: 4C, light rain"}},
					},
				}},
			}},
		},
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        "get_weather",
			Description: anthropic.String("Get the weather for a city"),
			InputSchema: anthropic.ToolInputSchemaParam{
				Type:       "object",
				Properties: map[string]any{"city": map[string]any{"type": "string"}},
				Required:   []string{"city"},
			},
		}}},
		ToolChoice: anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}},
		Thinking:   anthropic.ThinkingConfigParamOfEnabled(16384),
	}
}

// TestResponsesGoldenRequestTranslation pins the request shape against the
// hand-built std_turn2 capture (the STANDARD Responses shape). The codex
// captures (turn1/turn2 without the std_ prefix) are the lite dialect and are
// reference only: they are not a translation target.
func TestResponsesGoldenRequestTranslation(t *testing.T) {
	c := assert.NewCollecting(t)
	fixtureRaw, err := os.ReadFile(responsesFixtureReq2)
	c.Require().NoError(err, "read request fixture")
	var want map[string]any
	c.Require().NoError(json.Unmarshal(fixtureRaw, &want), "unmarshal request fixture")

	gotRaw, err := buildResponsesRequestBody(responsesGoldenParams(t), responsesBuildOptions{
		chatGPT:   true,
		stream:    true,
		sessionID: "74f25229-6e30-4eb3-9e3f-24b777c4f083",
		facts: func(string) (ModelFact, bool) {
			return ModelFact{SupportsReasoningSummary: true, SupportsVerbosity: true, DefaultVerbosity: "low"}, true
		},
	})
	c.Require().NoError(err, "buildResponsesRequestBody")
	var got map[string]any
	c.Require().NoError(json.Unmarshal(gotRaw, &got), "unmarshal built request")

	// Input item types and order.
	wantTypes := inputItemTypes(want["input"])
	gotTypes := inputItemTypes(got["input"])
	c.Eq("message,reasoning,function_call,function_call_output", strings.Join(wantTypes, ","), "fixture input item order")
	c.Eq(strings.Join(wantTypes, ","), strings.Join(gotTypes, ","), "input item types and order")

	// The scalar structural fields the fixture pins.
	for _, key := range []string{"model", "instructions", "tool_choice", "parallel_tool_calls", "store", "stream", "prompt_cache_key"} {
		c.EqDiff(want[key], got[key], "%s", key)
	}
	c.EqDiff(want["include"], got["include"], "include")
	c.EqDiff(want["reasoning"], got["reasoning"], "reasoning")

	// tools: the fixture's parameters carry additionalProperties:false, a
	// JSON-schema keyword an Anthropic ToolInputSchemaParam cannot express (see
	// responsesToolSchema), so it is normalised out of both sides before the
	// comparison rather than invented on the wire.
	c.EqDiff(normaliseToolParameters(want["tools"]), normaliseToolParameters(got["tools"]), "tools")

	// The documented three-argument entry point is this same builder with no
	// facts known and ChatGPT-mode streaming defaults: pin that it is wired to
	// the one builder (and that with no facts nothing claims summary support).
	plainRaw, err := buildResponsesRequest(responsesGoldenParams(t), true, "sess")
	c.Require().NoError(err, "buildResponsesRequest")
	var plain map[string]any
	c.Require().NoError(json.Unmarshal(plainRaw, &plain), "unmarshal body")
	c.EqDiff(true, plain["stream"], "stream")
	c.EqDiff(false, plain["store"], "store")
	plainReasoning, _ := plain["reasoning"].(map[string]any)
	c.EqDiff(any("high"), plainReasoning["effort"], "effort")
	c.NotHasKey(plainReasoning, "summary", "with no facts, ChatGPT mode must not claim summary support")
	c.EqDiff(strings.Join(gotTypes, ","), strings.Join(inputItemTypes(plain["input"]), ","), "input item order")
}

// inputItemTypes lists the "type" of every input item, in order.
func inputItemTypes(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		s, _ := m["type"].(string)
		out = append(out, s)
	}
	return out
}

// normaliseToolParameters strips the one schema keyword the Anthropic tool shape
// cannot carry, so a fixture written with it still compares.
func normaliseToolParameters(v any) any {
	tools, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(tools))
	for i, tool := range tools {
		m, ok := tool.(map[string]any)
		if !ok {
			out[i] = tool
			continue
		}
		clone := make(map[string]any, len(m))
		for k, v := range m {
			clone[k] = v
		}
		if params, ok := clone["parameters"].(map[string]any); ok {
			cp := make(map[string]any, len(params))
			for k, v := range params {
				if k == "additionalProperties" {
					continue
				}
				cp[k] = v
			}
			clone["parameters"] = cp
		}
		out[i] = clone
	}
	return out
}

// ---- stream translation ----

// TestResponsesStreamTranslatesFixture replays the standard-shape turn 1 reply:
// a reasoning item with an EMPTY summary followed by a function call. The
// reasoning item still becomes a thinking block (its signature is what makes it
// resendable), and the block order, signature and usage arithmetic are pinned
// here.
func TestResponsesStreamTranslatesFixture(t *testing.T) {
	c := assert.NewCollecting(t)
	types, acc, err := streamResponsesFixture(t, responsesFixtureTurn1)
	c.Require().NoError(err, "stream error")

	// One thinking block, then one tool_use block: the reasoning item precedes
	// the call it led to.
	c.True(len(types) > 0, "the stream emitted no events")
	c.Require().Len(acc.Content, 2, "accumulated blocks = %d, want 2", len(acc.Content))
	c.Eq("thinking", string(acc.Content[0].Type), "block 0 type")
	c.Eq("tool_use", string(acc.Content[1].Type), "block 1 type")
	c.Eq("tool_use", string(acc.StopReason), "stop reason")
	c.Eq("resp_0cf3ceb209f8671c016ae18df08954819ba356b3c5b74b1d53", acc.ID, "message id")
	c.Eq("gpt-6.1-sol", acc.Model, "message model")

	// The thinking block carries the Responses signature: prefix, item id, blob.
	id, blob, ok := ParseResponsesSignature(acc.Content[0].Signature)
	c.True(ok, "thinking signature %q is not a Responses signature", acc.Content[0].Signature)
	c.Eq("rs_0cf3ceb209f8671c016ae18df1d0b8819baece265f64916983", id, "signature item id")
	c.True(blob != "", "signature blob is empty")
	c.True(IsResponsesSignature(acc.Content[0].Signature), "IsResponsesSignature")
	// Empty summary: the thinking text stays empty, not " " or a placeholder.
	c.Eq("", acc.Content[0].Thinking, "thinking text")

	// Tool call: id is the endpoint's call_id, arguments reassembled from
	// deltas.
	c.Eq("call_70b6c42145b04a3eac6712e9e23c36df", acc.Content[1].ID, "tool_use id = call_id")
	c.Eq("get_weather", acc.Content[1].Name, "tool_use name")
	c.Eq(`{"city":"Oslo"}`, strings.TrimSpace(string(acc.Content[1].Input)), "tool_use input")

	// Usage: input_tokens excludes the cached prefix; cache counters carry it.
	c.Eq(int64(100), acc.Usage.InputTokens, "input tokens")
	c.Eq(int64(0), acc.Usage.CacheReadInputTokens, "cache read tokens")
	c.Eq(int64(35), acc.Usage.OutputTokens, "output tokens")
}

// TestResponsesNoReasoningIsFine replays the standard turn 2 reply, which has a
// text message and no reasoning item: no thinking block, no error. It also pins
// that a history with no thinking blocks builds a request with no reasoning
// input items.
func TestResponsesNoReasoningIsFine(t *testing.T) {
	c := assert.NewCollecting(t)
	_, acc, err := streamResponsesFixture(t, responsesFixtureTurn2)
	c.Require().NoError(err, "stream error")
	for _, b := range acc.Content {
		c.NotEq("thinking", string(b.Type), "unexpected thinking block in a response with no reasoning item")
	}
	c.True(len(acc.Content) > 0, "no content blocks accumulated")
	c.Eq(int64(0), acc.Usage.CacheReadInputTokens, "cache read tokens")

	body, err := buildResponsesRequestBody(anthropic.MessageNewParams{
		Model:     "gpt-6.1-sol",
		MaxTokens: 64,
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "hi"}},
			}},
		},
	}, responsesBuildOptions{chatGPT: true, stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody")
	var wire map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
	c.Eq("message", strings.Join(inputItemTypes(wire["input"]), ","), "input item types")
}

// TestResponsesReasoningRoundTrip pins that a thinking block carrying a
// Responses signature becomes a reasoning input item at the SAME position, which
// is where the endpoint expects it (before the call it led to).
func TestResponsesReasoningRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	sig := responsesSignature("rs_abc", "gAAAA-big-blob")
	params := anthropic.MessageNewParams{
		Model:     "gpt-6.1-sol",
		MaxTokens: 64,
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "think, then answer"}},
			}},
			{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "before"}},
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "weighing options", Signature: sig}},
				{OfText: &anthropic.TextBlockParam{Text: "after"}},
			}},
		},
	}
	body, err := buildResponsesRequestBody(params, responsesBuildOptions{chatGPT: true, stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody")
	var wire struct {
		Input []struct {
			Type             string `json:"type"`
			Role             string `json:"role"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
			Summary          []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"input"`
	}
	c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
	var got []string
	for _, it := range wire.Input {
		got = append(got, it.Type)
	}
	c.Eq("message,message,reasoning,message", strings.Join(got, ","), "input item types")
	reasoning := wire.Input[2]
	c.Eq("rs_abc", reasoning.ID, "reasoning id")
	c.Eq("gAAAA-big-blob", reasoning.EncryptedContent, "encrypted_content")
	c.Require().Len(reasoning.Summary, 1, "summary parts")
	c.Eq("summary_text", reasoning.Summary[0].Type, "summary part type")
	c.Eq("weighing options", reasoning.Summary[0].Text, "summary text")
}

// TestResponsesDropsEmptyReasoning pins that a reasoning item whose encrypted
// content is empty is not resent: an empty blob carries nothing, so resending it
// only costs tokens.
func TestResponsesDropsEmptyReasoning(t *testing.T) {
	c := assert.NewCollecting(t)
	params := anthropic.MessageNewParams{
		Model:     "gpt-6.1-sol",
		MaxTokens: 64,
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "hi"}},
			}},
			{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
				{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "kept in the transcript only", Signature: ResponsesSignaturePrefix + "rs_empty:"}},
				{OfText: &anthropic.TextBlockParam{Text: "answer"}},
			}},
		},
	}
	body, err := buildResponsesRequestBody(params, responsesBuildOptions{chatGPT: true, stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody")
	var wire map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
	c.Eq("message,message", strings.Join(inputItemTypes(wire["input"]), ","), "input item types")
}

// TestResponsesFunctionCallIDRule pins the id rule: a function_call item carries
// call_id and NO id key, ever. An Anthropic toolu_ id would be rejected by the
// endpoint as an id with the wrong prefix; a call_id is what correlates the call
// with its output.
func TestResponsesFunctionCallIDRule(t *testing.T) {
	mk := func(toolUseID string) map[string]any {
		params := anthropic.MessageNewParams{
			Model:     "gpt-6.1-sol",
			MaxTokens: 64,
			Messages: []anthropic.MessageParam{
				{Role: "user", Content: []anthropic.ContentBlockParamUnion{
					{OfText: &anthropic.TextBlockParam{Text: "call it"}},
				}},
				{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
					{OfToolUse: &anthropic.ToolUseBlockParam{ID: toolUseID, Name: "get_weather", Input: map[string]any{"city": "Oslo"}}},
				}},
			},
		}
		body, err := buildResponsesRequestBody(params, responsesBuildOptions{chatGPT: true, stream: true})
		assert.NewAborting(t).NoError(err, "buildResponsesRequestBody")
		var wire struct {
			Input []map[string]any `json:"input"`
		}
		assert.NewAborting(t).NoError(json.Unmarshal(body, &wire), "unmarshal body")
		assert.NewAborting(t).Len(wire.Input, 2, "input items")
		return wire.Input[1]
	}

	c := assert.NewCollecting(t)
	for _, id := range []string{"call_70b6c42145b04a3eac6712e9e23c36df", "toolu_x"} {
		item := mk(id)
		c.Eq("function_call", item["type"], "%s: item type", id)
		c.EqDiff(any(id), item["call_id"], "%s: call_id", id)
		c.NotHasKey(item, "id", "%s: function_call must carry no id key", id)
	}
}

// ---- ChatGPT mode ----

// TestResponsesChatGPTModeRequest pins the ChatGPT-mode body and headers: zstd,
// store:false, stream:true, encrypted reasoning included, prompt_cache_key and
// the session-id header both naming the session, the rafiki originator, and the
// TokenSource's own headers.
func TestResponsesChatGPTModeRequest(t *testing.T) {
	c := assert.NewCollecting(t)
	var (
		mu     sync.Mutex
		raw    []byte
		hdr    http.Header
		path   string
		method string
	)
	ts := &recordingTokenSource{bearer: "tok-1", headers: http.Header{"Chatgpt-Account-Id": {"acct-1"}}}
	s := newChatGPTResponsesSenderForTest(t, ts, nil, func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		mu.Lock()
		raw = body
		hdr = r.Header.Clone()
		path, method = r.URL.Path, r.Method
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})

	ctx := WithSessionID(context.Background(), "sess-42")
	_, err := s.New(ctx, minimalParams())
	c.Require().NoError(err, "New")

	mu.Lock()
	defer mu.Unlock()
	got := decodeMap(c, raw, "request body")
	c.Eq("/responses", path, "path")
	c.Eq("POST", method, "method")
	c.Eq("zstd", hdr.Get("Content-Encoding"), "Content-Encoding")
	c.Eq("sess-42", hdr.Get("session-id"), "session-id header")
	c.Eq("rafiki", hdr.Get("originator"), "originator")
	c.Eq("acct-1", hdr.Get("Chatgpt-Account-Id"), "TokenSource header")
	c.Eq("Bearer tok-1", hdr.Get("Authorization"), "Authorization")
	c.Eq("text/event-stream", hdr.Get("Accept"), "Accept")
	c.NotContains(hdr.Values("x-openai-internal-codex-responses-lite"), "true", "lite header must never be sent")
	c.NotHasKey(hdr, "OpenAI-Beta", "OpenAI-Beta is an older codex header and must not be sent")
	c.EqDiff(false, got["store"], "store")
	c.EqDiff(true, got["stream"], "stream")
	c.EqDiff(any([]any{"reasoning.encrypted_content"}), got["include"], "include")
	c.EqDiff("sess-42", got["prompt_cache_key"], "prompt_cache_key")
}

// TestResponsesChatGPTModeOmitsTemperatureAndLite pins the two things the
// ChatGPT dialect rejects: temperature (400 "Unsupported parameter") and the
// codex lite header (which 400s a standard body).
func TestResponsesChatGPTModeOmitsTemperatureAndLite(t *testing.T) {
	c := assert.NewCollecting(t)
	params := minimalParams()
	params.Temperature = anthropic.Float(0.5)

	body, err := buildResponsesRequestBody(params, responsesBuildOptions{chatGPT: true, stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody")
	var wire map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
	c.NotHasKey(wire, "temperature", "ChatGPT mode must never send temperature")

	// Plain mode does send it: the parameter is legal on api.openai.com.
	plainBody, err := buildResponsesRequestBody(params, responsesBuildOptions{stream: false})
	c.Require().NoError(err, "buildResponsesRequestBody (plain)")
	var plain map[string]any
	c.Require().NoError(json.Unmarshal(plainBody, &plain), "unmarshal plain body")
	c.EqDiff(0.5, plain["temperature"], "plain temperature")
	c.NotHasKey(plain, "store", "plain mode omits store")

	var hdr http.Header
	ts := &recordingTokenSource{bearer: "tok"}
	s := newChatGPTResponsesSenderForTest(t, ts, nil, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})
	_, err = s.New(context.Background(), minimalParams())
	c.Require().NoError(err, "New")
	c.NotContains(hdr.Values("x-openai-internal-codex-responses-lite"), "true", "lite header must never be sent")
}

// TestResponsesRetriesOnce401 pins the 401 rule: force a credential refresh and
// retry exactly once. A second 401 is the answer.
func TestResponsesRetriesOnce401(t *testing.T) {
	c := assert.NewCollecting(t)
	var requests int
	var mu sync.Mutex
	ts := &recordingTokenSource{bearer: "tok"}
	s := newChatGPTResponsesSenderForTest(t, ts, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})
	_, err := s.New(context.Background(), minimalParams())
	c.Require().NoError(err, "New")
	c.EqDiff([]bool{false, true}, ts.calls(), "Token force flags")
	c.Eq(2, requests, "requests")

	// Two 401s: the error is returned as-is after exactly two requests.
	requests = 0
	ts2 := &recordingTokenSource{bearer: "tok"}
	s2 := newChatGPTResponsesSenderForTest(t, ts2, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err = s2.New(context.Background(), minimalParams())
	c.Require().Error(err, "New with two 401s")
	var apiErr *anthropic.Error
	c.True(errors.As(err, &apiErr), "error %T is not an *anthropic.Error", err)
	c.Eq(http.StatusUnauthorized, apiErr.StatusCode, "status")
	c.Eq(2, requests, "requests with a repeated 401")
	c.EqDiff([]bool{false, true}, ts2.calls(), "Token force flags with a repeated 401")
}

// TestResponsesKeylessSendsNoAuthorization pins the keyless convention: an empty
// key sends NO Authorization header (never "Bearer ").
func TestResponsesKeylessSendsNoAuthorization(t *testing.T) {
	c := assert.NewCollecting(t)
	var hdr http.Header
	s := newResponsesSenderForTest(t, "", func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","model":"m","status":"completed","output":[]}`)
	})
	_, err := s.New(context.Background(), minimalParams())
	c.Require().NoError(err, "New")
	c.NotHasKey(hdr, "Authorization", "Authorization header")
	c.Eq("", hdr.Get("Authorization"), "Authorization")
}

// TestResponsesInstructionsPlacement pins the single switch that moves the system
// prompt: devMessage=true sends it as the first input item and omits the
// top-level instructions field.
func TestResponsesInstructionsPlacement(t *testing.T) {
	c := assert.NewCollecting(t)
	params := minimalParams()
	params.System = []anthropic.TextBlockParam{{Text: "sys one"}, {Text: "sys two"}}

	body, err := buildResponsesRequestBody(params, responsesBuildOptions{chatGPT: true, stream: true, devMessage: true})
	c.Require().NoError(err, "buildResponsesRequestBody (devMessage)")
	var wire struct {
		Instructions *string          `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
	c.Nil(wire.Instructions, "instructions must be omitted when the system prompt is a developer message")
	c.Require().Len(wire.Input, 2, "input items")
	c.Eq("message", wire.Input[0]["type"], "first item type")
	c.Eq("developer", wire.Input[0]["role"], "first item role")
	parts, _ := wire.Input[0]["content"].([]any)
	c.Require().Len(parts, 1, "developer content parts")
	c.EqDiff("sys one\n\nsys two", parts[0].(map[string]any)["text"], "system text joined with a blank line")

	// The default placement keeps the top-level field.
	body, err = buildResponsesRequestBody(params, responsesBuildOptions{chatGPT: true, stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody (instructions)")
	var wire2 map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire2), "unmarshal body")
	c.EqDiff("sys one\n\nsys two", wire2["instructions"], "instructions")
}

// ---- facts gating ----

// TestResponsesGatesOptionalFieldsOnFacts pins the three /models-derived gates:
// summary, text.verbosity, and (below) the effort clamp. A field the model's
// entry does not mention is omitted rather than sent with a default.
func TestResponsesGatesOptionalFieldsOnFacts(t *testing.T) {
	c := assert.NewCollecting(t)
	params := minimalParams()
	params.Thinking = anthropic.ThinkingConfigParamOfEnabled(8192)

	// Facts that allow nothing.
	body, err := buildResponsesRequestBody(params, responsesBuildOptions{
		chatGPT: true, stream: true,
		facts: func(string) (ModelFact, bool) { return ModelFact{}, true },
	})
	c.Require().NoError(err, "buildResponsesRequestBody (no support)")
	var wire map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
	reasoning, _ := wire["reasoning"].(map[string]any)
	c.NotHasKey(reasoning, "summary", "summary must be omitted when the facts do not support it")
	c.NotHasKey(wire, "text", "text must be omitted when the facts do not support verbosity")

	// Facts that allow both.
	body, err = buildResponsesRequestBody(params, responsesBuildOptions{
		chatGPT: true, stream: true,
		facts: func(string) (ModelFact, bool) {
			return ModelFact{SupportsReasoningSummary: true, SupportsVerbosity: true, DefaultVerbosity: "low"}, true
		},
	})
	c.Require().NoError(err, "buildResponsesRequestBody (support)")
	var wire2 map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire2), "unmarshal body")
	reasoning2, _ := wire2["reasoning"].(map[string]any)
	c.EqDiff("auto", reasoning2["summary"], "summary")
	text, _ := wire2["text"].(map[string]any)
	c.EqDiff("low", text["verbosity"], "verbosity")

	// openai-responses sends summary unconditionally: it is a documented field
	// on the public API, with no per-model /models entry to consult.
	body, err = buildResponsesRequestBody(params, responsesBuildOptions{stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody (plain)")
	var wire3 map[string]any
	c.Require().NoError(json.Unmarshal(body, &wire3), "unmarshal body")
	reasoning3, _ := wire3["reasoning"].(map[string]any)
	c.EqDiff("auto", reasoning3["summary"], "plain summary")
	c.NotHasKey(wire3, "text", "plain mode never sends verbosity")
}

// TestResponsesEffortClamp pins the clamp: the desired level if the model lists
// it, else the highest listed level below it, else the lowest listed level, and
// xhigh to high when nothing is known.
func TestResponsesEffortClamp(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		levels []string
		known  bool
		want   string
	}{
		{"xhigh lowered to the top supported level", 32768, []string{"low", "medium", "high"}, true, "high"},
		{"low raised to the only supported level", 4096, []string{"medium"}, true, "medium"},
		{"no facts: xhigh becomes high", 32768, nil, false, "high"},
		{"desired level supported", 8192, []string{"low", "medium", "high"}, true, "medium"},
		{"highest listed level below the desired one", 16384, []string{"minimal", "low"}, true, "low"},
		{"an unknown level name in the list is ignored", 16384, []string{"low", "turbo", "high"}, true, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			fact := ModelFact{ReasoningLevels: tc.levels}
			got, ok := responsesEffort(anthropic.ThinkingConfigParamOfEnabled(tc.budget), fact, tc.known)
			c.True(ok, "reasoning requested")
			c.Eq(tc.want, got, "effort")
		})
	}
}

// TestResponsesEffortMapping pins the budget table and the omission of the
// reasoning key when thinking is off.
func TestResponsesEffortMapping(t *testing.T) {
	c := assert.NewCollecting(t)
	cases := []struct {
		budget int64
		want   string
	}{
		{4096, "low"},
		{8192, "medium"},
		{16384, "high"},
		{32768, "high"}, // xhigh, lowered because the model's levels are unknown
	}
	for _, tc := range cases {
		params := minimalParams()
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(tc.budget)
		body, err := buildResponsesRequestBody(params, responsesBuildOptions{stream: true})
		c.Require().NoError(err, "buildResponsesRequestBody")
		var wire map[string]any
		c.Require().NoError(json.Unmarshal(body, &wire), "unmarshal body")
		reasoning, _ := wire["reasoning"].(map[string]any)
		c.EqDiff(any(tc.want), reasoning["effort"], "effort for budget %d", tc.budget)
		// reasoning.context is a lite-dialect field: never sent.
		c.NotHasKey(reasoning, "context", "budget %d: reasoning.context", tc.budget)
	}

	disabled, err := buildResponsesRequestBody(minimalParams(), responsesBuildOptions{stream: true})
	c.Require().NoError(err, "buildResponsesRequestBody (no thinking)")
	var plain map[string]any
	c.Require().NoError(json.Unmarshal(disabled, &plain), "unmarshal body")
	c.NotHasKey(plain, "reasoning", "no thinking budget must send no reasoning key")
	c.NotHasKey(plain, "include", "no thinking budget must not request encrypted reasoning")
}

// ---- failures ----

// TestResponsesUsageLimitIsTerminal pins the usage-limit classification: a 429 or
// a 404 whose body carries a usage-limit code is terminal, typed, and invisible
// to every retry classifier.
func TestResponsesUsageLimitIsTerminal(t *testing.T) {
	c := assert.NewCollecting(t)
	const limitBody = `{"error":{"type":"usage_limit_reached","code":"usage_limit_reached","plan_type":"plus","resets_at":1900000000,"limit_window_minutes":300}}`

	limitErr := func(t *testing.T, status int, body string, hdr http.Header) error {
		t.Helper()
		s := newResponsesSenderForTest(t, "k", func(w http.ResponseWriter, r *http.Request) {
			for k, vs := range hdr {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
		_, err := s.New(context.Background(), minimalParams())
		assert.NewAborting(t).Error(err, "New should fail")
		return err
	}

	err := limitErr(t, http.StatusTooManyRequests, limitBody, nil)
	var u *UsageLimitError
	c.True(errors.As(err, &u), "error %T is not a *UsageLimitError", err)
	c.Eq("usage_limit_reached", u.Code, "code")
	c.Eq("plus", u.PlanType, "plan")
	c.Eq(time.Unix(1900000000, 0).UTC().Format(time.RFC3339), u.ResetsAt.UTC().Format(time.RFC3339), "resets_at")
	var apiErr *anthropic.Error
	c.False(errors.As(err, &apiErr), "UsageLimitError must not be an *anthropic.Error")
	c.StrContains(err.Error(), "ChatGPT usage limit reached (usage_limit_reached, plan plus); resets at ", "error text")
	for _, forbidden := range []string{"EOF", "connection reset", "broken pipe", "i/o timeout", "tls:"} {
		c.NotStrContains(err.Error(), forbidden, "error text must not contain %q (agentloop would retry it)", forbidden)
	}
	// Invisible to both retry classifiers.
	isRL, _ := isRateLimit(err)
	c.False(isRL, "isRateLimit")
	c.False(IsRateLimitStreamError(err), "IsRateLimitStreamError")

	// The same body on a 404 is the same terminal error.
	err = limitErr(t, http.StatusNotFound, limitBody, nil)
	c.True(errors.As(err, &u), "404: error %T is not a *UsageLimitError", err)
	c.Eq("usage_limit_reached", u.Code, "404 code")

	// A 429 without a usage-limit code stays a retryable *anthropic.Error 429.
	err = limitErr(t, http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, nil)
	c.True(errors.As(err, &apiErr), "plain 429: error %T is not an *anthropic.Error", err)
	c.Eq(http.StatusTooManyRequests, apiErr.StatusCode, "plain 429 status")
	isRL, _ = isRateLimit(err)
	c.True(isRL, "plain 429 must stay retryable")

	// A 503 stays a 503.
	err = limitErr(t, http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`, nil)
	c.True(errors.As(err, &apiErr), "503: error %T is not an *anthropic.Error", err)
	c.Eq(http.StatusServiceUnavailable, apiErr.StatusCode, "503 status")

	// Header fallback: the family is chosen by x-codex-active-limit, and
	// codex_other reads the other-primary headers.
	before := time.Now()
	err = limitErr(t, http.StatusTooManyRequests, limitBody[:strings.Index(limitBody, `,"resets_at"`)]+`}}`, http.Header{
		"X-Codex-Active-Limit":                      {"codex_other"},
		"X-Codex-Other-Primary-Reset-After-Seconds": {"3600"},
		"X-Codex-Primary-Reset-After-Seconds":       {"60"},
	})
	c.True(errors.As(err, &u), "header fallback: error %T is not a *UsageLimitError", err)
	reset := u.ResetsAt.Sub(before)
	c.True(reset > 55*time.Minute && reset < 65*time.Minute,
		"resets_at from the codex_other family = %v after the request, want about 1h", reset)
}

// TestResponsesTerminalEvents pins the terminal-event table: each class of
// failure becomes the error class the layers above this package act on.
func TestResponsesTerminalEvents(t *testing.T) {
	c := assert.NewCollecting(t)
	created := `{"type":"response.created","response":{"id":"resp_1","model":"m"}}`
	cases := []struct {
		name  string
		sse   string
		check func(t *testing.T, types []string, acc anthropic.Message, err error)
	}{
		{
			name: "incomplete with max_output_tokens stops with max_tokens",
			sse: responsesSSE(created,
				`{"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"output_tokens":5}}}`),
			check: func(t *testing.T, _ []string, acc anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().NoError(err, "stream error")
				c.Eq("max_tokens", string(acc.StopReason), "stop reason")
				c.Eq(int64(6), acc.Usage.InputTokens, "input tokens (10 total - 4 cached)")
				c.Eq(int64(4), acc.Usage.CacheReadInputTokens, "cache read tokens")
				c.Eq(int64(2), acc.Usage.CacheCreationInputTokens, "cache creation tokens")
				c.Eq(int64(5), acc.Usage.OutputTokens, "output tokens")
			},
		},
		{
			name: "incomplete with content_filter is terminal",
			sse: responsesSSE(created,
				`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				c.StrContains(err.Error(), responsesContentFilterMessage, "error")
				c.False(IsTransientStreamError(err), "content filter must not be retried")
				c.False(IsRateLimitStreamError(err), "content filter is not a rate limit")
			},
		},
		{
			name: "failed with context_length_exceeded is prompt too large",
			sse: responsesSSE(created,
				`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window"}}}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				c.True(IsPromptTooLarge(err), "IsPromptTooLarge(%T) = false", err)
				c.StrContains(err.Error(), "context window", "error keeps the endpoint's detail")
			},
		},
		{
			name: "failed with usage_limit_reached is a UsageLimitError",
			sse: responsesSSE(created,
				`{"type":"response.failed","response":{"error":{"code":"usage_limit_reached","plan_type":"pro","resets_at":1900000000}}}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				var u *UsageLimitError
				c.True(errors.As(err, &u), "error %T is not a *UsageLimitError", err)
				c.Eq("pro", u.PlanType, "plan")
			},
		},
		{
			name: "error with rate_limit_exceeded is a rate limit",
			sse: responsesSSE(created,
				`{"type":"error","code":"rate_limit_exceeded","message":"Please slow down"}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				c.True(IsRateLimitStreamError(err), "IsRateLimitStreamError")
				c.False(IsTransientStreamError(err), "a rate limit is not the generic transient class")
			},
		},
		{
			name: "failed with server_is_overloaded is transient",
			sse: responsesSSE(created,
				`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"busy"}}}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				c.True(IsTransientStreamError(err), "IsTransientStreamError")
			},
		},
		{
			name: "failed with an unknown code is transient",
			sse: responsesSSE(created,
				`{"type":"response.failed","response":{"error":{"code":"something_new","message":"?"}}}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				c.True(IsTransientStreamError(err), "an unknown code must stay retryable")
			},
		},
		{
			name: "failed with invalid_prompt is terminal",
			sse: responsesSSE(created,
				`{"type":"response.failed","response":{"error":{"code":"invalid_prompt","message":"no"}}}`),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "stream error")
				c.False(IsTransientStreamError(err), "invalid_prompt must not be retried")
				c.StrContains(err.Error(), "no", "error keeps the message")
			},
		},
		{
			name: "a stream that ends without a terminal event is transient",
			sse:  responsesSSE(created),
			check: func(t *testing.T, _ []string, _ anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().Error(err, "truncated stream")
				c.True(IsTransientStreamError(err), "IsTransientStreamError")
			},
		},
		{
			name: "unknown event types are ignored",
			sse: responsesSSE(created,
				`{"type":"response.metadata","response_id":"resp_1","metadata":{}}`,
				responsesCompleted(``)),
			check: func(t *testing.T, _ []string, acc anthropic.Message, err error) {
				c := assert.NewCollecting(t)
				c.Require().NoError(err, "stream error")
				c.Eq("end_turn", string(acc.StopReason), "stop reason")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newResponsesSenderForTest(t, "k", openAIStreamHandler(tc.sse))
			stream, err := s.NewStreaming(context.Background(), minimalParams())
			c.Require().NoError(err, "NewStreaming")
			types, acc, serr := runResponsesStream(t, stream)
			tc.check(t, types, acc, serr)
		})
	}
}

// TestResponsesLargeEventLine pins the reader's line cap: output_item.done and
// completed carry whole output items, encrypted reasoning included, on ONE data
// line, and bufio.Scanner's 64 KiB default would fail them.
func TestResponsesLargeEventLine(t *testing.T) {
	c := assert.NewCollecting(t)
	huge := strings.Repeat("x", 5<<20)
	sse := responsesSSE(
		`{"type":"response.created","response":{"id":"resp_big","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":`+mustJSONString(huge)+`}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+mustJSONString(huge)+`}]}}`,
		responsesCompleted(``),
	)
	s := newResponsesSenderForTest(t, "k", openAIStreamHandler(sse))
	stream, err := s.NewStreaming(context.Background(), minimalParams())
	c.Require().NoError(err, "NewStreaming")
	_, acc, serr := runResponsesStream(t, stream)
	c.Require().NoError(serr, "stream error")
	c.Require().Len(acc.Content, 1, "content blocks")
	c.Eq(len(huge), len(acc.Content[0].Text), "accumulated text length")
}

// TestResponsesStreamIdleTimeout pins the idle timeout: a backend that goes
// silent after the first event is a transient failure, not a hung turn.
func TestResponsesStreamIdleTimeout(t *testing.T) {
	c := assert.NewCollecting(t)
	done := make(chan struct{})
	s := newResponsesSenderForTest(t, "k", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-done:
		}
	})
	t.Cleanup(func() { close(done) })
	s.streamIdleTimeout = 50 * time.Millisecond

	stream, err := s.NewStreaming(context.Background(), minimalParams())
	c.Require().NoError(err, "NewStreaming")
	_, _, serr := runResponsesStream(t, stream)
	c.Require().Error(serr, "stream error")
	c.True(IsTransientStreamError(serr), "IsTransientStreamError")
}

// ---- transport behaviour ----

// TestResponsesTurnStateEcho pins x-codex-turn-state: remembered per session and
// replayed on that session's next request, never on another session's.
func TestResponsesTurnStateEcho(t *testing.T) {
	c := assert.NewCollecting(t)
	var (
		mu   sync.Mutex
		seen []string
		n    int
	)
	s := newChatGPTResponsesSenderForTest(t, &recordingTokenSource{bearer: "tok"}, nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		seen = append(seen, r.Header.Get("x-codex-turn-state"))
		state := "ts-" + string(rune('0'+n))
		mu.Unlock()
		w.Header().Set("x-codex-turn-state", state)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	})

	ctxA := WithSessionID(context.Background(), "sess-a")
	ctxB := WithSessionID(context.Background(), "sess-b")
	_, err := s.New(ctxA, minimalParams())
	c.Require().NoError(err, "first request")
	_, err = s.New(ctxA, minimalParams())
	c.Require().NoError(err, "second request, same session")
	_, err = s.New(ctxB, minimalParams())
	c.Require().NoError(err, "first request, other session")

	mu.Lock()
	defer mu.Unlock()
	c.Eq("", seen[0], "first request of a session carries no turn state")
	c.Eq("ts-1", seen[1], "second request of the same session replays the first response's turn state")
	c.Eq("", seen[2], "another session must not inherit a turn state")
}

// TestResponsesUserAgentAndCookies pins the two transport details a bot filter
// and the Cloudflare edge care about: a real User-Agent, and a per-sender cookie
// jar that replays a Set-Cookie within one sender and never across senders.
func TestResponsesUserAgentAndCookies(t *testing.T) {
	c := assert.NewCollecting(t)
	var (
		mu      sync.Mutex
		ua      []string
		cookies []string
		first   = true
	)
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ua = append(ua, r.Header.Get("User-Agent"))
		cookies = append(cookies, r.Header.Get("Cookie"))
		set := first
		first = false
		mu.Unlock()
		if set {
			http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "abc", Path: "/"})
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
	}

	// One handler, two senders: each has its own jar, so the cookie a sender
	// receives never reaches the other.
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	mk := func() *responsesSender {
		sender, err := NewChatGPTResponsesSender(providers.Provider{Name: "chatgpt"}, &recordingTokenSource{bearer: "t"}, nil, nil)
		assert.NewAborting(t).NoError(err, "NewChatGPTResponsesSender")
		s := sender.(*responsesSender)
		s.baseURL = srv.URL
		return s
	}
	a, b := mk(), mk()

	_, err := a.New(context.Background(), minimalParams())
	c.Require().NoError(err, "sender a, request 1")
	_, err = a.New(context.Background(), minimalParams())
	c.Require().NoError(err, "sender a, request 2")
	_, err = b.New(context.Background(), minimalParams())
	c.Require().NoError(err, "sender b, request 1")

	mu.Lock()
	defer mu.Unlock()
	c.Require().Len(ua, 3, "requests")
	for i, got := range ua {
		c.True(strings.HasPrefix(got, "rafiki/"), "request %d User-Agent = %q, want a rafiki/ prefix", i, got)
		c.StrContains(got, "(darwin; arm64)", "request %d User-Agent", i)
	}
	c.False(strings.Contains(cookies[0], "cf_clearance"), "first request carried a cookie: %q", cookies[0])
	c.StrContains(cookies[1], "cf_clearance=abc", "the jar must replay the cookie within one sender")
	c.False(strings.Contains(cookies[2], "cf_clearance"), "another sender's jar leaked a cookie: %q", cookies[2])
}

// ---- the SDK direction ----

// TestSDKSenderDropsResponsesThinking pins the Anthropic direction: an sdkSender
// never sends a thinking block whose signature the Responses sender wrote.
func TestSDKSenderDropsResponsesThinking(t *testing.T) {
	c := assert.NewCollecting(t)
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = raw
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	sender := sdkSender{client: anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("k"),
		option.WithMaxRetries(0),
	)}
	params := minimalParams()
	params.Messages = []anthropic.MessageParam{
		{Role: "user", Content: []anthropic.ContentBlockParamUnion{
			{OfText: &anthropic.TextBlockParam{Text: "hi"}},
		}},
		{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
			{OfThinking: &anthropic.ThinkingBlockParam{Thinking: "foreign", Signature: responsesSignature("rs_1", "blob")}},
			{OfText: &anthropic.TextBlockParam{Text: "answer"}},
		}},
	}
	_, err := sender.New(context.Background(), params)
	c.Require().NoError(err, "sender.New")

	mu.Lock()
	defer mu.Unlock()
	c.NotStrContains(string(body), ResponsesSignaturePrefix, "the Anthropic request carried a Responses signature")
	c.StrContains(string(body), "answer", "the assistant text must survive the filter")
	// The caller's own history is untouched.
	c.Require().Len(params.Messages, 2, "caller messages")
	c.Require().Len(params.Messages[1].Content, 2, "caller message content (not mutated)")
}

// TestResponsesNonStreamingResponse pins the non-streamed api.openai.com path:
// New returns one JSON response object, translated into the same Anthropic shapes
// the streaming path produces (text, tool_use keyed by call_id, reasoning as a
// thinking block carrying the Responses signature, and usage).
func TestResponsesNonStreamingResponse(t *testing.T) {
	c := assert.NewCollecting(t)
	const body = `{"id":"resp_ns","model":"gpt-6.1-sol","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"weighing"}],"encrypted_content":"blob-1"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling get_weather"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}
	],"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":1},"output_tokens":7}}`
	var method, path string
	s := newResponsesSenderForTest(t, "sk", func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	msg, err := s.New(context.Background(), minimalParams())
	c.Require().NoError(err, "New")
	c.Eq("POST", method, "method")
	c.Eq("/responses", path, "path")
	c.Eq("resp_ns", msg.ID, "id")
	c.Eq("gpt-6.1-sol", msg.Model, "model")
	c.Require().Len(msg.Content, 3, "blocks")
	c.Eq("thinking", string(msg.Content[0].Type), "block 0")
	c.Eq("weighing", msg.Content[0].Thinking, "thinking text")
	_, _, ok := ParseResponsesSignature(msg.Content[0].Signature)
	c.True(ok, "signature %q is not a Responses signature", msg.Content[0].Signature)
	c.Eq("text", string(msg.Content[1].Type), "block 1")
	c.Eq("calling get_weather", msg.Content[1].Text, "text")
	c.Eq("tool_use", string(msg.Content[2].Type), "block 2")
	c.Eq("call_1", msg.Content[2].ID, "tool_use id")
	c.Eq("tool_use", string(msg.StopReason), "stop reason")
	c.Eq(int64(15), msg.Usage.InputTokens, "input tokens (20 - 5 cached)")
	c.Eq(int64(5), msg.Usage.CacheReadInputTokens, "cache read tokens")
	c.Eq(int64(7), msg.Usage.OutputTokens, "output tokens")
}
