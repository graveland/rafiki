// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/providers"
)

// newOpenAISenderForTest builds an openAISender against a fixture server, the
// way SenderForKey will thread it together once KindOpenAI is wired (Task
// 2.1): base URL from the provider entry, explicit key, rt passed straight
// through.
func newOpenAISenderForTest(t *testing.T, key string, rt http.RoundTripper, handler http.HandlerFunc) *openAISender {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := providers.Provider{Name: "fixture", Kind: providers.KindOpenAI, BaseURL: srv.URL}
	s, err := newOpenAISender(p, key, rt)
	if err != nil {
		t.Fatalf("newOpenAISender: %v", err)
	}
	return s
}

// openAIRepresentativeParams exercises every column of the request-translation
// table at once: a system prompt (two blocks — one concatenated message, not
// one per block), a multi-block assistant message (text + tool_use in one
// message), a tool_result user message that must split into a tool message
// plus a user message, a function tool, a named tool_choice, an optional
// temperature, and unset TopP/StopSequences that must be omitted.
func openAIRepresentativeParams() anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		MaxTokens:   1024,
		Model:       "gpt-4o",
		Temperature: anthropic.Float(0.2),
		System: []anthropic.TextBlockParam{
			{Text: "You are helpful."},
			{Text: "Be brief."},
		},
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "What's the weather in Paris?"}},
			}},
			{Role: "assistant", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "Let me check."}},
				{OfToolUse: &anthropic.ToolUseBlockParam{
					ID:    "call_1",
					Name:  "get_weather",
					Input: map[string]any{"city": "Paris"},
				}},
			}},
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: "call_1",
					Content: []anthropic.ToolResultBlockParamContentUnion{
						{OfText: &anthropic.TextBlockParam{Text: "18C and sunny"}},
					},
				}},
				{OfText: &anthropic.TextBlockParam{Text: "Now summarize."}},
			}},
		},
		Tools: []anthropic.ToolUnionParam{{
			OfTool: &anthropic.ToolParam{
				Name:        "get_weather",
				Description: anthropic.String("Get the weather for a city"),
				InputSchema: anthropic.ToolInputSchemaParam{
					Type:       "object",
					Properties: map[string]any{"city": map[string]any{"type": "string"}},
					Required:   []string{"city"},
				},
			},
		}},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "get_weather"},
		},
	}
}

// okCompletion is the minimal response the fixture servers answer with on the
// request-translation tests; the request is what those tests assert.
const openAIOK = `{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

func TestOpenAISenderNewBuildsRequest(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPath string
		gotMeth string
		gotCT   string
		gotAuth string
		gotBody map[string]any
		bodyErr string
	)
	s := newOpenAISenderForTest(t, "test-key", nil, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		r.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotMeth = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		if err != nil {
			bodyErr = err.Error()
		} else if err := json.Unmarshal(raw, &gotBody); err != nil {
			bodyErr = err.Error()
		}
		_, _ = w.Write([]byte(openAIOK))
	})

	if _, err := s.New(context.Background(), openAIRepresentativeParams()); err != nil {
		t.Fatalf("New: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if bodyErr != "" {
		t.Fatalf("fixture server could not read request body: %s", bodyErr)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("request path = %q, want /chat/completions", gotPath)
	}
	if gotMeth != http.MethodPost {
		t.Errorf("request method = %q, want POST", gotMeth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotBody["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want 1024", gotBody["max_tokens"])
	}
	if v, ok := gotBody["stream"]; !ok || v != false {
		t.Errorf("stream = %v (present=%v), want an explicit false", gotBody["stream"], ok)
	}
	if gotBody["temperature"] != float64(0.2) {
		t.Errorf("temperature = %v, want 0.2", gotBody["temperature"])
	}
	for _, absent := range []string{"top_p", "stop", "top_k", "container", "inference_geo", "metadata", "service_tier", "thinking"} {
		if _, ok := gotBody[absent]; ok {
			t.Errorf("body contains %q, want it omitted", absent)
		}
	}

	var msgs []map[string]any
	if err := json.Unmarshal([]byte(mustJSON(gotBody["messages"])), &msgs); err != nil {
		t.Fatalf("messages not an array: %v (%s)", err, mustJSON(gotBody["messages"]))
	}
	if len(msgs) != 5 {
		t.Fatalf("messages = %d entries, want 5 (system, user, assistant, tool, user); got %s",
			len(msgs), mustJSON(msgs))
	}
	for i, want := range []string{
		`{"role":"system","content":"You are helpful.Be brief."}`,
		`{"role":"user","content":"What's the weather in Paris?"}`,
		`{"role":"assistant","content":"Let me check.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}`,
		`{"role":"tool","tool_call_id":"call_1","content":"18C and sunny"}`,
		`{"role":"user","content":"Now summarize."}`,
	} {
		if !jsonEqual(mustJSON(msgs[i]), want) {
			t.Errorf("messages[%d] = %s, want %s", i, mustJSON(msgs[i]), want)
		}
	}

	wantTools := `[{"type":"function","function":{"name":"get_weather","description":"Get the weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
	if !jsonEqual(mustJSON(gotBody["tools"]), wantTools) {
		t.Errorf("tools = %s, want %s", mustJSON(gotBody["tools"]), wantTools)
	}
	wantChoice := `{"type":"function","function":{"name":"get_weather"}}`
	if !jsonEqual(mustJSON(gotBody["tool_choice"]), wantChoice) {
		t.Errorf("tool_choice = %s, want %s", mustJSON(gotBody["tool_choice"]), wantChoice)
	}
}

// TestOpenAISenderNewUserMessageContentBlocks pins how user messages whose
// content blocks do not fully translate behave. An image-only message (the
// shape llm.UserContent documents, reachable from pkg/fundi's send path) must
// fail the turn with the limitation named — a silent whole-message drop would
// lose all of the user's content — while a text+image message still
// translates: text kept on the wire, image dropped with the existing Warn.
func TestOpenAISenderNewUserMessageContentBlocks(t *testing.T) {
	imageBlock := anthropic.NewImageBlockBase64("image/png", "aGVsbG8=")

	t.Run("image_only_is_an_error", func(t *testing.T) {
		s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(openAIOK))
		})
		params := minimalParams()
		params.Messages = []anthropic.MessageParam{{
			Role:    "user",
			Content: []anthropic.ContentBlockParamUnion{imageBlock},
		}}
		_, err := s.New(context.Background(), params)
		if err == nil {
			t.Fatal("New: want an error for an image-only user message, got nil — the message silently vanished from the request")
		}
		if !strings.Contains(err.Error(), "images") || !strings.Contains(err.Error(), "do not support") {
			t.Errorf("error = %v, want it to name the limitation (image blocks unsupported by kind=openai providers)", err)
		}
	})

	t.Run("text_plus_image_keeps_text", func(t *testing.T) {
		var (
			mu      sync.Mutex
			gotBody map[string]any
		)
		s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			r.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				_ = json.Unmarshal(raw, &gotBody)
			}
			_, _ = w.Write([]byte(openAIOK))
		})
		params := minimalParams()
		params.Messages = []anthropic.MessageParam{{
			Role: "user",
			Content: []anthropic.ContentBlockParamUnion{
				imageBlock,
				{OfText: &anthropic.TextBlockParam{Text: "What is in this picture?"}},
			},
		}}
		if _, err := s.New(context.Background(), params); err != nil {
			t.Fatalf("New: %v — a partially translatable user message must still send (text kept, image dropped with the Warn)", err)
		}
		mu.Lock()
		defer mu.Unlock()
		var msgs []map[string]any
		if err := json.Unmarshal([]byte(mustJSON(gotBody["messages"])), &msgs); err != nil {
			t.Fatalf("messages not an array: %v (%s)", err, mustJSON(gotBody["messages"]))
		}
		if len(msgs) != 1 {
			t.Fatalf("messages = %d entries, want exactly the one user message: %s", len(msgs), mustJSON(msgs))
		}
		if !jsonEqual(mustJSON(msgs[0]), `{"role":"user","content":"What is in this picture?"}`) {
			t.Errorf("messages[0] = %s, want the text kept and nothing else on the wire", mustJSON(msgs[0]))
		}
	})
}

// TestOpenAISenderNewParsesResponse drives three fixture responses through the
// sender: plain text, tool_calls, and an unrecognized finish_reason.
func TestOpenAISenderNewParsesResponse(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there."},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
		})
		msg, err := s.New(context.Background(), minimalParams())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if msg.ID != "chatcmpl-1" {
			t.Errorf("ID = %q, want chatcmpl-1", msg.ID)
		}
		if msg.Model != "gpt-4o" {
			t.Errorf("Model = %q, want gpt-4o", msg.Model)
		}
		if msg.Role != "assistant" {
			t.Errorf("Role = %q, want assistant (constant.Assistant)", msg.Role)
		}
		if msg.Type != "message" {
			t.Errorf("Type = %q, want message (constant.Message)", msg.Type)
		}
		if msg.StopReason != anthropic.StopReasonEndTurn {
			t.Errorf("StopReason = %q, want end_turn", msg.StopReason)
		}
		if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Hello there." {
			t.Errorf("Content = %s, want one text block %q", mustJSON(msg.Content), "Hello there.")
		}
		if msg.Usage.InputTokens != 11 {
			t.Errorf("Usage.InputTokens = %d, want 11", msg.Usage.InputTokens)
		}
		if msg.Usage.OutputTokens != 7 {
			t.Errorf("Usage.OutputTokens = %d, want 7", msg.Usage.OutputTokens)
		}
	})

	t.Run("tool_calls", func(t *testing.T) {
		s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-2","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9}}`))
		})
		msg, err := s.New(context.Background(), minimalParams())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if msg.StopReason != anthropic.StopReasonToolUse {
			t.Errorf("StopReason = %q, want tool_use", msg.StopReason)
		}
		if len(msg.Content) != 1 {
			t.Fatalf("Content = %d blocks, want 1 tool_use (null content must not add a text block)", len(msg.Content))
		}
		block := msg.Content[0]
		if block.Type != "tool_use" || block.ID != "call_9" || block.Name != "get_weather" {
			t.Errorf("tool_use block = %s, want {tool_use, call_9, get_weather}", mustJSON(block))
		}
		if string(block.Input) != `{"city":"Paris"}` {
			t.Errorf("Input = %s, want the raw arguments JSON, not double-encoded", block.Input)
		}
		if msg.Usage.InputTokens != 20 || msg.Usage.OutputTokens != 9 {
			t.Errorf("Usage = %d/%d, want 20/9", msg.Usage.InputTokens, msg.Usage.OutputTokens)
		}
	})

	t.Run("unrecognized_finish_reason", func(t *testing.T) {
		s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-3","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"unknown_reason_xyz"}]}`))
		})
		msg, err := s.New(context.Background(), minimalParams())
		if err != nil {
			t.Fatalf("New returned an error for an unrecognized finish_reason (must fall back, not fail): %v", err)
		}
		if msg.StopReason != anthropic.StopReasonEndTurn {
			t.Errorf("StopReason = %q, want end_turn", msg.StopReason)
		}
	})
}

// agentloopIsRetryableMirror transcribes the *anthropic.Error branch of
// pkg/agentloop's isRetryable (pkg/agentloop/retry.go:44-52): 429 is false
// (sendStreaming's own rate-limit loop owns it), 500..599 true, everything
// else false. It exists because the real isRetryable cannot be reached from
// this package two ways at once: pkg/agentloop imports pkg/llm, so importing
// it here fails with "import cycle not allowed in test", and the function is
// unexported besides — no file outside package agentloop can call it. If that
// branch ever changes, this mirror must change with it; the errors.As +
// StatusCode assertions beside it pin the shape isRetryable actually switches
// on, so a sender that stops producing *anthropic.Error still fails here.
func agentloopIsRetryableMirror(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.StatusCode
	if code == 429 {
		return false
	}
	return code >= 500 && code < 600
}

func TestOpenAISenderNewMaps5xxToAnthropicError(t *testing.T) {
	s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusServiceUnavailable)
	})
	_, err := s.New(context.Background(), minimalParams())
	if err == nil {
		t.Fatal("New: want an error for a 503 response, got nil")
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *anthropic.Error (isRetryable type-switches on it)", err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", apiErr.StatusCode)
	}
	if apiErr.Request == nil || apiErr.Response == nil {
		t.Errorf("Request/Response = %v/%v, want both set (the SDK error shape)", apiErr.Request, apiErr.Response)
	}
	// The classification pkg/agentloop's own isRetryable applies to exactly
	// this error shape (see agentloopIsRetryableMirror above): a 503 retries.
	if !agentloopIsRetryableMirror(err) {
		t.Errorf("isRetryable(503) = false, want true — this provider would silently never retry")
	}
}

func TestOpenAISenderNewMaps400ToNonRetryable(t *testing.T) {
	s := newOpenAISenderForTest(t, "k", nil, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	})
	_, err := s.New(context.Background(), minimalParams())
	if err == nil {
		t.Fatal("New: want an error for a 400 response, got nil")
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *anthropic.Error", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", apiErr.StatusCode)
	}
	if agentloopIsRetryableMirror(err) {
		t.Errorf("isRetryable(400) = true, want false — retrying a permanent 4xx burns budget")
	}
}

func TestOpenAISenderNewOmitsAuthHeaderWhenKeyless(t *testing.T) {
	var mu sync.Mutex
	var sawKey bool
	s := newOpenAISenderForTest(t, "", nil, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := r.Header["Authorization"]; ok {
			// Direct map lookup, not Header.Get: Get cannot distinguish an
			// absent header from an empty one, and "Bearer " with an empty
			// value is exactly the inversion this test exists to catch.
			sawKey = true
		}
		_, _ = w.Write([]byte(openAIOK))
	})
	if _, err := s.New(context.Background(), minimalParams()); err != nil {
		t.Fatalf("New: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawKey {
		t.Error("keyless sender sent an Authorization header (even empty) — want none at all")
	}
}

// recordingRoundTripper records whether it was invoked, so the
// capture-transport test can prove the wrap reached the real HTTP call
// instead of being bypassed.
type recordingRoundTripper struct {
	called atomic.Bool
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.called.Store(true)
	return http.DefaultTransport.RoundTrip(req)
}

func TestOpenAISenderNewWrapsRoundTripperForCapture(t *testing.T) {
	rt := &recordingRoundTripper{}
	s := newOpenAISenderForTest(t, "k", rt, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(openAIOK))
	})
	if _, err := s.New(context.Background(), minimalParams()); err != nil {
		t.Fatalf("New: %v", err)
	}
	if !rt.called.Load() {
		t.Error("caller's RoundTripper was never invoked — headerCaptureTransport wrapping is not on the real request path, so raw-trace capture would silently see nothing")
	}
}

// The openai kind builds its own *http.Request rather than going through
// sessionIDTransport (which wraps an SDK client's RoundTripper), so session
// pinning needs its own assertion here — sender_provider_test.go covers the
// SDK-based kinds. Unset SessionHeader must stay silent (no implicit
// default, unlike KindAnthropicOpenRouter): confirmed by the second case
// below sending no header at all despite a session id being on ctx.
func TestOpenAISenderNewHonorsConfiguredSessionHeader(t *testing.T) {
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("x-session-affinity")
		mu.Unlock()
		_, _ = w.Write([]byte(openAIOK))
	}))
	defer srv.Close()

	s, err := newOpenAISender(providers.Provider{
		Name: "fireworks-openai", Kind: providers.KindOpenAI,
		BaseURL: srv.URL, SessionHeader: "x-session-affinity",
	}, "", nil)
	if err != nil {
		t.Fatalf("newOpenAISender: %v", err)
	}
	ctx := WithSessionID(context.Background(), "conv-shard-2")
	if _, err := s.New(ctx, minimalParams()); err != nil {
		t.Fatalf("New: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "conv-shard-2" {
		t.Errorf("x-session-affinity = %q, want conv-shard-2", got)
	}
}

func TestOpenAISenderNewNoSessionHeaderWhenUnconfigured(t *testing.T) {
	var mu sync.Mutex
	var sawAny bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_, sawAny = r.Header["X-Session-Affinity"]
		mu.Unlock()
		_, _ = w.Write([]byte(openAIOK))
	}))
	defer srv.Close()

	s, err := newOpenAISender(providers.Provider{Name: "x", Kind: providers.KindOpenAI, BaseURL: srv.URL}, "", nil)
	if err != nil {
		t.Fatalf("newOpenAISender: %v", err)
	}
	ctx := WithSessionID(context.Background(), "conv-shard-2")
	if _, err := s.New(ctx, minimalParams()); err != nil {
		t.Fatalf("New: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawAny {
		t.Error("session header present with no session_header configured — the openai kind must stay silent, it has no implicit default")
	}
}

// TestOpenAISenderNewRequiresBaseURL pins the constructor contract Task 2.1's
// SenderForKey wiring depends on: unlike Anthropic/OpenRouter there is no
// canonical default base URL for a generic OpenAI-compatible endpoint, so an
// empty BaseURL must be a config error, not a silent default.
func TestOpenAISenderNewRequiresBaseURL(t *testing.T) {
	p := providers.Provider{Name: "no-base", Kind: providers.KindOpenAI, BaseURL: ""}
	if _, err := newOpenAISender(p, "k", nil); err == nil {
		t.Fatal("newOpenAISender with empty BaseURL: want a config error, got nil")
	}
	p.BaseURL = "http://localhost:11434/v1"
	if _, err := newOpenAISender(p, "k", nil); err != nil {
		t.Fatalf("newOpenAISender with a base_url: %v", err)
	}
}

// minimalParams is the smallest MessageNewParams the SDK requires.
func minimalParams() anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		MaxTokens: 16,
		Model:     "gpt-4o",
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: []anthropic.ContentBlockParamUnion{
				{OfText: &anthropic.TextBlockParam{Text: "hi"}},
			}},
		},
	}
}

// mustJSON marshals v for assertions and error messages; on failure it says
// so instead of panicking inside a t.Errorf argument.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable: " + err.Error() + ">"
	}
	return string(b)
}

// jsonEqual compares two JSON documents by value (whitespace-insensitive), so
// the expected literals above can be written compactly while the actual wire
// bytes carry Go's own spacing.
func jsonEqual(a, b string) bool {
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		return false
	}
	return mustJSON(va) == mustJSON(vb)
}

// ---- streaming (NewStreaming + the SSE translation) ----

// openAIStreamBody joins SSE data lines into one text/event-stream body: each
// element becomes `data: <line>\n\n`. "[DONE]" is just another line.
func openAIStreamBody(lines ...string) string {
	var sb strings.Builder
	for _, line := range lines {
		sb.WriteString("data: " + line + "\n\n")
	}
	return sb.String()
}

// openAIStreamHandler serves a pre-built SSE body with the content type real
// OpenAI-compatible endpoints answer streams with.
func openAIStreamHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// openAIToolOpenChunk is the FIRST chunk for a tool call: it carries the id,
// the function name, and an empty arguments string (OpenAI does not repeat
// any of these on later chunks).
func openAIToolOpenChunk(id, name string) string {
	return `{"id":"chatcmpl-s2","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":""}}]},"finish_reason":null}]}`
}

// openAIToolArgsChunk builds a tool-call continuation chunk carrying the next
// raw arguments fragment, with the fragment JSON-escaped exactly as OpenAI
// escapes it inside the chunk's string field.
func openAIToolArgsChunk(args string) string {
	return `{"id":"chatcmpl-s2","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + mustJSONString(args) + `}}]},"finish_reason":null}]}`
}

// mustJSONString JSON-encodes s as a wire string value (the arguments field is
// a JSON-encoded string, not an object).
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// collectStream drains a stream via Next()/Current(), accumulating the message
// the way pkg/llm/client.go's sendStreaming does (Accumulate plus
// backfillDeltaUsage), so the assertions cover the real consumer path.
// Deliberately omits the Fix/Sanitize helpers the real loop also applies
// (client.go); they are no-ops for the shapes this decoder emits (tool blocks
// always carry input:{}), so the binding holds without them.
func collectStream(t *testing.T, stream interface {
	Next() bool
	Current() anthropic.MessageStreamEventUnion
	Err() error
	Close() error
}) (events []anthropic.MessageStreamEventUnion, types []string, acc anthropic.Message) {
	t.Helper()
	for stream.Next() {
		ev := stream.Current()
		events = append(events, ev)
		types = append(types, ev.Type)
		if err := acc.Accumulate(ev); err != nil {
			t.Fatalf("Accumulate(%s): %v", ev.Type, err)
		}
		backfillDeltaUsage(&acc, ev)
	}
	return events, types, acc
}

func TestOpenAISenderNewStreamingTextOnly(t *testing.T) {
	s := newOpenAISenderForTest(t, "k", nil, openAIStreamHandler(openAIStreamBody(
		`{"id":"chatcmpl-s1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-s1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo "},"finish_reason":null}]}`,
		`{"id":"chatcmpl-s1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-s1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		`[DONE]`,
	)))

	stream, err := s.NewStreaming(context.Background(), minimalParams())
	if err != nil {
		t.Fatalf("NewStreaming: %v", err)
	}
	defer stream.Close()
	events, types, acc := collectStream(t, stream)

	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err() after a healthy stream: %v", err)
	}
	// Cases 1-3-3-3-6-7: message_start, one content_block_start, one delta per
	// content chunk, one stop, message_delta, message_stop — exactly, nothing
	// extra.
	wantTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if !slices.Equal(types, wantTypes) {
		t.Fatalf("event types = %v, want %v", types, wantTypes)
	}

	start := events[0]
	if start.Message.ID != "chatcmpl-s1" || start.Message.Model != "gpt-4o" {
		t.Errorf("message_start message = %s, want id chatcmpl-s1 model gpt-4o", mustJSON(start.Message))
	}
	if start.Message.Role != "assistant" || start.Message.Type != "message" {
		t.Errorf("message_start role/type = %q/%q, want assistant/message", start.Message.Role, start.Message.Type)
	}
	if len(start.Message.Content) != 0 {
		t.Errorf("message_start content = %s, want empty", mustJSON(start.Message.Content))
	}
	if start.Message.Usage.InputTokens != 0 || start.Message.Usage.OutputTokens != 0 {
		t.Errorf("message_start usage = %s, want zeroed (real numbers are not known yet)", mustJSON(start.Message.Usage))
	}

	blockStart := events[1]
	if blockStart.Index != 0 {
		t.Errorf("content_block_start index = %d, want 0", blockStart.Index)
	}
	if blockStart.ContentBlock.Type != "text" || blockStart.ContentBlock.Text != "" {
		t.Errorf("content_block_start content_block = %s, want {type:text,text:\"\"}", mustJSON(blockStart.ContentBlock))
	}

	wantTexts := []string{"Hel", "lo ", "world"}
	for i, want := range wantTexts {
		ev := events[2+i]
		if ev.Index != 0 {
			t.Errorf("content_block_delta[%d] index = %d, want 0", i, ev.Index)
		}
		if ev.Delta.Text != want {
			t.Errorf("content_block_delta[%d] text = %q, want %q", i, ev.Delta.Text, want)
		}
	}
	if events[5].Type != "content_block_stop" || events[5].Index != 0 {
		t.Errorf("content_block_stop = %s index %d, want index 0", events[5].Type, events[5].Index)
	}

	delta := events[6]
	if delta.Delta.StopReason != anthropic.StopReasonEndTurn {
		t.Errorf("message_delta stop_reason = %q, want end_turn", delta.Delta.StopReason)
	}
	if delta.Usage.InputTokens != 11 || delta.Usage.OutputTokens != 7 {
		t.Errorf("message_delta usage = %d/%d, want 11/7", delta.Usage.InputTokens, delta.Usage.OutputTokens)
	}

	// The accumulated message is what the caller actually keeps: text
	// reassembled across deltas, the mapped stop reason, and usage backfilled
	// from message_delta (input arrives only there — message_start is zeroed).
	if acc.ID != "chatcmpl-s1" || acc.Model != "gpt-4o" {
		t.Errorf("accumulated id/model = %q/%q, want chatcmpl-s1/gpt-4o", acc.ID, acc.Model)
	}
	if acc.StopReason != anthropic.StopReasonEndTurn {
		t.Errorf("accumulated stop_reason = %q, want end_turn", acc.StopReason)
	}
	if len(acc.Content) != 1 || acc.Content[0].Text != "Hello world" {
		t.Errorf("accumulated content = %s, want one text block \"Hello world\"", mustJSON(acc.Content))
	}
	if acc.Usage.InputTokens != 11 || acc.Usage.OutputTokens != 7 {
		t.Errorf("accumulated usage = %d/%d, want 11/7", acc.Usage.InputTokens, acc.Usage.OutputTokens)
	}
}

func TestOpenAISenderNewStreamingToolCall(t *testing.T) {
	// The arguments JSON, split across four fragments exactly as OpenAI streams
	// them: the first chunk carries id+name with empty arguments, then the raw
	// fragments follow one per chunk. This is the test that dies if the
	// translation accumulates and re-emits: a snapshotting implementation
	// produces garbage when the fragments are concatenated.
	fragments := []string{
		`{"city"`,
		`:"Paris",`,
		`"unit":"cel`,
		`sius"}`,
	}
	lines := []string{
		openAIToolOpenChunk("call_7", "get_weather"),
		openAIToolArgsChunk(fragments[0]),
		openAIToolArgsChunk(fragments[1]),
		openAIToolArgsChunk(fragments[2]),
		openAIToolArgsChunk(fragments[3]),
		`{"id":"chatcmpl-s2","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9}}`,
		`[DONE]`,
	}
	s := newOpenAISenderForTest(t, "k", nil, openAIStreamHandler(openAIStreamBody(lines...)))

	stream, err := s.NewStreaming(context.Background(), minimalParams())
	if err != nil {
		t.Fatalf("NewStreaming: %v", err)
	}
	defer stream.Close()
	events, types, acc := collectStream(t, stream)

	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err() after a healthy stream: %v", err)
	}
	wantTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if !slices.Equal(types, wantTypes) {
		t.Fatalf("event types = %v, want %v", types, wantTypes)
	}

	blockStart := events[1]
	if blockStart.ContentBlock.Type != "tool_use" {
		t.Errorf("content_block_start type = %q, want tool_use", blockStart.ContentBlock.Type)
	}
	if blockStart.ContentBlock.ID != "call_7" || blockStart.ContentBlock.Name != "get_weather" {
		t.Errorf("content_block_start id/name = %q/%q, want call_7/get_weather (id and name arrive once, on the first chunk)", blockStart.ContentBlock.ID, blockStart.ContentBlock.Name)
	}
	if input := mustJSON(blockStart.ContentBlock.Input); input != `{}` {
		t.Errorf("content_block_start input = %s, want {}", input)
	}

	// Each partial_json must be the EXACT raw fragment OpenAI sent, in order,
	// unmodified — not a re-serialized snapshot.
	var gotFragments []string
	for _, ev := range events {
		if ev.Type != "content_block_delta" {
			continue
		}
		if ev.Delta.Type != "input_json_delta" {
			t.Fatalf("delta type = %q, want input_json_delta", ev.Delta.Type)
		}
		gotFragments = append(gotFragments, ev.Delta.PartialJSON)
	}
	if !slices.Equal(gotFragments, fragments) {
		t.Fatalf("partial_json fragments = %q, want the raw fragments %q (forwarded as-is, not accumulated)",
			gotFragments, fragments)
	}
	joined := strings.Join(gotFragments, "")
	if !json.Valid([]byte(joined)) {
		t.Fatalf("concatenated fragments = %q, which is not valid JSON", joined)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(joined), &parsed); err != nil {
		t.Fatalf("concatenated fragments %q do not unmarshal: %v", joined, err)
	}
	if parsed["city"] != "Paris" || parsed["unit"] != "celsius" {
		t.Errorf("parsed fragments = %s, want the arguments the fixture sent", mustJSON(parsed))
	}

	if acc.StopReason != anthropic.StopReasonToolUse {
		t.Errorf("accumulated stop_reason = %q, want tool_use", acc.StopReason)
	}
	if len(acc.Content) != 1 {
		t.Fatalf("accumulated content = %d blocks, want 1 tool_use", len(acc.Content))
	}
	if string(acc.Content[0].Input) != `{"city":"Paris","unit":"celsius"}` {
		t.Errorf("accumulated input = %s, want the fragments concatenated into the original JSON", acc.Content[0].Input)
	}
	if acc.Usage.InputTokens != 20 || acc.Usage.OutputTokens != 9 {
		t.Errorf("accumulated usage = %d/%d, want 20/9", acc.Usage.InputTokens, acc.Usage.OutputTokens)
	}
}

func TestOpenAISenderNewStreamingErrorMidStream(t *testing.T) {
	// Two healthy chunks, then a line that is not a chunk (case 8's
	// "non-[DONE] line that fails to parse as a chunk").
	s := newOpenAISenderForTest(t, "k", nil, openAIStreamHandler(openAIStreamBody(
		`{"id":"chatcmpl-s3","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"partial "},"finish_reason":null}]}`,
		`{"id":"chatcmpl-s3","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"text"},"finish_reason":null}]}`,
		`{oops-not-json`,
		`[DONE]`,
	)))

	stream, err := s.NewStreaming(context.Background(), minimalParams())
	if err != nil {
		t.Fatalf("NewStreaming: %v", err)
	}
	defer stream.Close()
	events, types, _ := collectStream(t, stream)

	// The healthy prefix is delivered before the failure surfaces.
	wantPrefix := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta"}
	if len(types) < len(wantPrefix) || !slices.Equal(types[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("events before the failure = %v, want the healthy prefix %v first", types, wantPrefix)
	}

	serr := stream.Err()
	if serr == nil {
		t.Fatal("stream.Err() = nil after an unparseable chunk, want the in-band error")
	}
	// Case 8's contract: the error event must be shaped for ParseStreamError —
	// the exact string sseStreamErrPrefix expects, carrying a type from
	// streamerr.go's vocabulary.
	if se, ok := ParseStreamError(serr); !ok {
		t.Fatalf("ParseStreamError(%v) = ok:false — the error event is not shaped for recovery", serr)
	} else if se.ErrType != "api_error" {
		t.Errorf("error.type = %q, want api_error (the safe default for an unparseable chunk)", se.ErrType)
	}
	if !IsTransientStreamError(serr) {
		t.Errorf("IsTransientStreamError = false, want true — a mid-stream failure must classify as transient")
	}
	if len(events) < len(wantPrefix) {
		t.Fatalf("only %d events delivered, want at least the healthy prefix", len(events))
	}
}

func TestSenderForKeyBuildsOpenAISender(t *testing.T) {
	p := providers.Provider{Name: "oai", Kind: providers.KindOpenAI, BaseURL: "http://example.invalid", APIKeyEnv: "OPENAI_API_KEY"}
	s, err := SenderForKey(p, "resolved-key", nil)
	if err != nil {
		t.Fatalf("SenderForKey with a base_url: %v", err)
	}
	if s == nil {
		t.Fatal("SenderForKey returned nil Sender with no error")
	}
	// The wiring must go through newOpenAISender (not an SDK client), and the
	// result must carry the streaming capability agentloop type-asserts for.
	if _, ok := s.(*openAISender); !ok {
		t.Fatalf("SenderForKey(KindOpenAI) returned %T, want *openAISender", s)
	}
	if _, ok := s.(StreamingSender); !ok {
		t.Fatalf("SenderForKey(KindOpenAI) returned %T, which does not implement StreamingSender — streaming would silently fall back to non-streamed sends", s)
	}

	// The key threaded through SenderForKey must reach the wire as
	// "Authorization: Bearer <key>". The assertions above are type/capability
	// only, so a regression to newOpenAISender(p, "", rt) — silently unkeying
	// every keyed OpenAI provider — would pass them; only a fixture round
	// trip catches it.
	var (
		mu      sync.Mutex
		gotAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(openAIOK))
	}))
	t.Cleanup(srv.Close)
	p.BaseURL = srv.URL
	sw, err := SenderForKey(p, "resolved-key", nil)
	if err != nil {
		t.Fatalf("SenderForKey against a fixture: %v", err)
	}
	if _, err := sw.New(context.Background(), minimalParams()); err != nil {
		t.Fatalf("New through SenderForKey: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer resolved-key" {
		t.Errorf("Authorization = %q, want Bearer resolved-key — the key did not survive the SenderForKey → newOpenAISender threading", gotAuth)
	}

	// No canonical default base URL exists for a generic OpenAI-compatible
	// endpoint: an empty base_url is a config error, not a silent default.
	p.BaseURL = ""
	if s2, err := SenderForKey(p, "resolved-key", nil); err == nil {
		t.Fatalf("SenderForKey with empty BaseURL = %v, want a config error", s2)
	}
}
