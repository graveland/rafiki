// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/rafiki/routing"
	"git.graveland.dev/brent/rafiki/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The fidelity rule: the tee mutates nothing except the model field.
// cache_control blocks, anthropic-beta headers and OpenRouter x-session-id
// pass through byte-faithful — sentinel's cache breakpoints and session
// pinning must survive the proxy.

const fidelityBody = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,` +
	`"system":[{"type":"text","text":"You are sentinel.","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`

// weirdSSE is a realistic, fully-accumulatable stream (message_start with a
// complete skeleton + content_block_start, as Anthropic actually sends)
// written in adversarial chunk sizes.
const weirdSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_tee","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunky"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

func TestMessagesTeePassesBodyAndHeadersByteFaithful(t *testing.T) {
	var upstreamGotBody []byte
	var upstreamGotHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamGotBody, _ = io.ReadAll(r.Body)
		upstreamGotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Adversarial chunking: 7-byte writes with flushes.
		for i := 0; i < len(weirdSSE); i += 7 {
			end := min(i+7, len(weirdSSE))
			_, _ = io.WriteString(w, weirdSSE[i:end])
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fidelityBody))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31,other-beta")
	req.Header.Set("x-session-id", "sentinel-session-42")
	req.Header.Set("Authorization", "Bearer client-secret-must-not-leak")
	p.ServeHTTP(rec, req)

	// Request body reached the upstream byte-identical (model was already
	// concrete: no resolution rewrite).
	if !bytes.Equal(upstreamGotBody, []byte(fidelityBody)) {
		t.Errorf("request body mutated by the tee:\n got: %s\nwant: %s", upstreamGotBody, fidelityBody)
	}
	// Fidelity headers forwarded; inbound client auth stripped and replaced.
	if got := upstreamGotHeaders.Get("anthropic-beta"); got != "prompt-caching-2024-07-31,other-beta" {
		t.Errorf("anthropic-beta = %q", got)
	}
	// x-session-id is an OpenRouter session-pinning concept: never forwarded
	// to the Anthropic primary (keeps the primary request byte-exact to
	// pre-extraction), forwarded on OpenRouter paths (tested below).
	if got := upstreamGotHeaders.Get("x-session-id"); got != "" {
		t.Errorf("x-session-id forwarded to the Anthropic primary: %q", got)
	}
	if got := upstreamGotHeaders.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := upstreamGotHeaders.Get("x-api-key"); got != "real-key" {
		t.Errorf("x-api-key = %q, want the server key", got)
	}
	if got := upstreamGotHeaders.Get("Authorization"); got != "" {
		t.Errorf("client Authorization leaked upstream: %q", got)
	}
	// Response stream reached the client byte-identical.
	if rec.Body.String() != weirdSSE {
		t.Errorf("response stream mutated:\n got: %q\nwant: %q", rec.Body.String(), weirdSSE)
	}
}

// The OpenRouter path of the messages face DOES forward x-session-id
// (sentinel's session pinning must survive failover and slash routing).
func TestMessagesOpenRouterPathForwardsSessionID(t *testing.T) {
	var gotSession string
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", "http://unused-primary", "", nil, logger)
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"openai/gpt-4o","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-session-id", "sentinel-session-42")
	p.ServeHTTP(rec, req)

	if gotSession != "sentinel-session-42" {
		t.Errorf("x-session-id on the OpenRouter path = %q, want forwarded", gotSession)
	}
}

func TestChatCompletionsProxyStreamsAndCaptures(t *testing.T) {
	const chatSSE = `data: {"id":"cc-1","model":"openai/gpt-4o","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"cc-1","choices":[{"delta":{"content":" world"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"cc-1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}}` + "\n\n" +
		"data: [DONE]\n\n"

	var gotAuth, gotSession string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("x-session-id")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSE)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &recordingChatStore{}
	p := NewChatCompletionsProxy(nil, nil,
		[]OpenAIUpstream{{Name: "openrouter", BaseURL: upstream.URL, APIKey: "or-key"}},
		nil, "openrouter", logger)
	p.store = fs

	body := `{"model":"openai/gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Rafiki-Session", "chat-sess-1")
	req.Header.Set("x-session-id", "or-pin-9")
	p.ServeHTTP(rec, req)

	if rec.Body.String() != chatSSE {
		t.Errorf("stream mutated:\n got: %q", rec.Body.String())
	}
	if gotAuth != "Bearer or-key" {
		t.Errorf("upstream auth = %q", gotAuth)
	}
	if gotSession != "or-pin-9" {
		t.Errorf("x-session-id = %q", gotSession)
	}
	if !bytes.Equal(gotBody, []byte(body)) {
		t.Errorf("request body mutated (this face resolves nothing): %s", gotBody)
	}
	if fs.completes != 1 || fs.fails != 0 {
		t.Fatalf("capture: completes=%d fails=%d, want 1/0", fs.completes, fs.fails)
	}
	if fs.last.StopReason != "stop" || fs.last.InputTokens != 12 || fs.last.OutputTokens != 9 || fs.last.CacheReadTokens != 4 {
		t.Errorf("captured turn = %+v, want stop/12/9/cache4", fs.last)
	}
	if !strings.Contains(string(fs.last.Response), "Hello world") {
		t.Errorf("canonical response missing accumulated content: %s", fs.last.Response)
	}
	if fs.lastIntent.Protocol != "openai" {
		t.Errorf("protocol = %q, want openai", fs.lastIntent.Protocol)
	}
}

func TestChatCompletionsProxyFailsTurnOnUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &recordingChatStore{}
	p := NewChatCompletionsProxy(nil, nil,
		[]OpenAIUpstream{{Name: "openrouter", BaseURL: upstream.URL, APIKey: "k"}}, nil, "openrouter", logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openai/gpt-4o"}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 passed through", rec.Code)
	}
	if fs.fails != 1 || fs.completes != 0 {
		t.Errorf("capture: fails=%d completes=%d, want 1/0", fs.fails, fs.completes)
	}
}

func TestChatCompletionsRoutesByModelPrefix(t *testing.T) {
	var hits []string
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits = append(hits, name)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
	}
	def, special := mk("default"), mk("special")
	defer def.Close()
	defer special.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewChatCompletionsProxy(nil, nil,
		[]OpenAIUpstream{
			{Name: "openrouter", BaseURL: def.URL, APIKey: "k1"},
			{Name: "special", BaseURL: special.URL, APIKey: "k2"},
		},
		[]OpenAIRoute{{Prefix: "special/", Upstream: "special"}}, "openrouter", logger)

	for _, model := range []string{"openai/gpt-4o", "special/model-x"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
		p.ServeHTTP(rec, req)
	}
	if len(hits) != 2 || hits[0] != "default" || hits[1] != "special" {
		t.Errorf("routing hits = %v, want [default special]", hits)
	}
}

func TestStaticTokenAuth(t *testing.T) {
	auth := NewStaticTokenAuth(map[string]string{"sentinel": "tok-sentinel", "pi": "tok-pi"})
	var gotIdentity *Identity
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := auth.Middleware(inner)

	cases := []struct {
		name       string
		hdr, value string
		wantStatus int
		wantUser   string
	}{
		{"bearer", "Authorization", "Bearer tok-sentinel", 200, "sentinel"},
		{"x-api-key", "x-api-key", "tok-pi", 200, "pi"},
		{"unknown", "Authorization", "Bearer nope", 401, ""},
		{"missing", "", "", 401, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotIdentity = nil
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if c.hdr != "" {
				req.Header.Set(c.hdr, c.value)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if c.wantUser != "" && (gotIdentity == nil || gotIdentity.Username != c.wantUser) {
				t.Errorf("identity = %+v, want %q", gotIdentity, c.wantUser)
			}
		})
	}
}

// recordingChatStore is a fake proxyStore for the OpenAI face.
type recordingChatStore struct {
	completes, fails int
	last             routing.TurnResult
	lastIntent       routing.TurnIntent
}

func (f *recordingChatStore) EnsureConversationByExternalRef(_ context.Context, _ routing.ConversationRef) (string, error) {
	return "conv-openai", nil
}

func (f *recordingChatStore) InsertTurnIntent(_ context.Context, t routing.TurnIntent) (string, time.Time, error) {
	f.lastIntent = t
	return "turn-openai", time.Unix(0, 0), nil
}

func (f *recordingChatStore) CompleteTurn(_ context.Context, r routing.TurnResult) error {
	f.completes++
	f.last = r
	return nil
}

func (f *recordingChatStore) FailTurn(_ context.Context, _ string, _ time.Time, _ string) error {
	f.fails++
	return nil
}

func (f *recordingChatStore) DecomposeRequest(_ context.Context, _, _ string, _ time.Time, _ []byte, _ string) (int, error) {
	return 0, nil
}

func (f *recordingChatStore) AppendResponseMessage(_ context.Context, _, _ string, _ time.Time, _ int, _ []byte, _, _ int64, _ string) error {
	return nil
}

// When the client sends no x-session-id, the OpenRouter path falls back to
// rafiki's own conversation id — so a client that never sets the header
// (e.g. Claude Code) still gets a stable, correlatable session pin.
func TestMessagesOpenRouterPathFallsBackToConvID(t *testing.T) {
	var gotSession string
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", "http://unused-primary", "", nil, logger)
	p.store = &recordingChatStore{}
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"openai/gpt-4o","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	// No x-session-id set.
	p.ServeHTTP(rec, req)

	if gotSession != "conv-openai" {
		t.Errorf("x-session-id fallback = %q, want rafiki's conversation id", gotSession)
	}
}

// The OpenAI face has the same fallback: no client x-session-id, fall back
// to rafiki's own conversation id.
func TestChatCompletionsFallsBackToConvID(t *testing.T) {
	var gotSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &recordingChatStore{}
	p := NewChatCompletionsProxy(nil, nil,
		[]OpenAIUpstream{{Name: "openrouter", BaseURL: upstream.URL, APIKey: "or-key"}},
		nil, "openrouter", logger)
	p.store = fake

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	// No x-session-id set.
	p.ServeHTTP(rec, req)

	if gotSession != "conv-openai" {
		t.Errorf("x-session-id fallback = %q, want rafiki's conversation id", gotSession)
	}
}

// End-to-end tee against a REAL capture store (RAFIKI_TEST_DSN): adversarial
// chunking upstream, then assert the persisted turn is complete with a valid
// reassembled canonical response — the fake-store tests can't catch
// store-layer marshaling issues.
func TestMessagesTeeWithRealStore(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_tee_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < len(weirdSSE); i += 5 {
			end := min(i+5, len(weirdSSE))
			_, _ = io.WriteString(w, weirdSSE[i:end])
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(routing.NewCaptureStore(pool), nil, "real-key", upstream.URL, "", nil, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fidelityBody))
	req.Header.Set("X-Rafiki-Session", "tee-real-store")
	p.ServeHTTP(rec, req)

	if rec.Body.String() != weirdSSE {
		t.Fatalf("stream mutated under real store")
	}
	var status string
	var responseNull bool
	var outTokens int64
	if err := pool.QueryRow(ctx, `SELECT t.status, t.response IS NULL, t.output_tokens
		FROM conversations.conversation_turn t
		JOIN conversations.conversation c ON c.id = t.conversation_id
		WHERE c.external_ref = 'tee-real-store'`).Scan(&status, &responseNull, &outTokens); err != nil {
		t.Fatalf("read captured turn: %v", err)
	}
	if status != "complete" || outTokens != 7 {
		t.Errorf("turn = %s/%d tokens, want complete/7", status, outTokens)
	}
	if !responseNull {
		t.Error("turn.response should be NULL; decomposition replaces the full-JSONB write")
	}
	// The canonical response's marshaling correctness is now verified via the
	// decomposed assistant conversation_message instead of turn.response.
	var msgContent string
	if err := pool.QueryRow(ctx, `SELECT m.content::text
		FROM conversations.conversation_message m
		JOIN conversations.conversation c ON c.id = m.conversation_id
		WHERE c.external_ref = 'tee-real-store' AND m.role = 'assistant'`).Scan(&msgContent); err != nil {
		t.Fatalf("read decomposed assistant message: %v", err)
	}
	var content any
	if err := json.Unmarshal([]byte(msgContent), &content); err != nil {
		t.Errorf("decomposed assistant content is not valid JSON: %v", err)
	}
}

// OpenAI SSE tool_calls deltas (fragmented arguments) reassemble into the
// canonical message; undecodable chunks and empty streams are parse errors.
func TestParseOpenAIResponseToolCallsAndStrictness(t *testing.T) {
	toolSSE := `data: {"id":"cc-2","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"cc-2","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"sf\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	finish, _, canonical, err := parseOpenAIResponse("text/event-stream", []byte(toolSSE))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q", finish)
	}
	var m struct {
		Choices []struct {
			Message struct {
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(canonical, &m); err != nil {
		t.Fatalf("canonical: %v", err)
	}
	tc := m.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "get_weather" ||
		tc[0].Function.Arguments != `{"city":"sf"}` {
		t.Errorf("tool_calls reassembly wrong: %+v", tc)
	}

	// Strictness: garbage chunk and empty stream are errors, not silent
	// zero-usage completions.
	if _, _, _, err := parseOpenAIResponse("text/event-stream", []byte("data: {garbage\n\n")); err == nil {
		t.Error("undecodable chunk must be a parse error")
	}
	if _, _, _, err := parseOpenAIResponse("text/event-stream", []byte("data: [DONE]\n\n")); err == nil {
		t.Error("empty stream must be a parse error")
	}
	if _, _, _, err := parseOpenAIResponse("application/json", []byte("not json")); err == nil {
		t.Error("undecodable JSON body must be a parse error")
	}
}
