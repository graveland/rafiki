// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
