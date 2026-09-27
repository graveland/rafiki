// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
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

	"github.com/jackc/pgx/v5/pgxpool"
	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
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
	c.True(bytes.Equal(upstreamGotBody, []byte(fidelityBody)), "request body mutated by the tee:\n got: %s\nwant: %s", upstreamGotBody, fidelityBody)
	// Fidelity headers forwarded; inbound client auth stripped and replaced.
	c.Eq("prompt-caching-2024-07-31,other-beta", upstreamGotHeaders.Get("anthropic-beta"), "anthropic-beta =")
	// x-session-id is an OpenRouter session-pinning concept: never forwarded
	// to the Anthropic primary (keeps the primary request byte-exact to
	// pre-extraction), forwarded on OpenRouter paths (tested below).
	c.Eq("", upstreamGotHeaders.Get("x-session-id"), "x-session-id forwarded to the Anthropic primary")
	c.Eq("2023-06-01", upstreamGotHeaders.Get("anthropic-version"), "anthropic-version =")
	c.Eq("real-key", upstreamGotHeaders.Get("x-api-key"), "x-api-key")
	c.Eq("", upstreamGotHeaders.Get("Authorization"), "client Authorization leaked upstream")
	// Response stream reached the client byte-identical.
	c.Eq(weirdSSE, rec.Body.String(), "response stream mutated:\n got")
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

	assert.NewCollecting(t).Eq("sentinel-session-42", gotSession, "x-session-id on the OpenRouter path")
}

func TestChatCompletionsProxyStreamsAndCaptures(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(chatSSE, rec.Body.String(), "stream mutated:\n got")
	c.Eq("Bearer or-key", gotAuth, "upstream auth =")
	c.Eq("or-pin-9", gotSession, "x-session-id =")
	c.True(bytes.Equal(gotBody, []byte(body)), "request body mutated (this face resolves nothing): %s", gotBody)
	if fs.completes != 1 || fs.fails != 0 {
		t.Fatalf("capture: completes=%d fails=%d, want 1/0", fs.completes, fs.fails)
	}
	if fs.last.StopReason != "stop" || fs.last.InputTokens != 12 || fs.last.OutputTokens != 9 || fs.last.CacheReadTokens != 4 {
		t.Errorf("captured turn = %+v, want stop/12/9/cache4", fs.last)
	}
	c.StrContains(string(fs.last.Response), "Hello world", "canonical response missing accumulated content: %s", fs.last.Response)
	c.Eq("openai", fs.lastIntent.Protocol, "protocol")
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

	assert.NewCollecting(t).Eq(http.StatusTooManyRequests, rec.Code, "status")
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
	assert.NewCollecting(t).False(len(hits) != 2 || hits[0] != "default" || hits[1] != "special", "routing hits = %v, want [default special]", hits)
}

func TestUserTokenAuth_HeaderVariantsMultipleUsers(t *testing.T) {
	auth := newTestUserAuth(map[string]string{"tok-sentinel": "sentinel", "tok-pi": "pi"})
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
			ck := assert.NewCollecting(t)
			gotIdentity = nil
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if c.hdr != "" {
				req.Header.Set(c.hdr, c.value)
			}
			h.ServeHTTP(rec, req)
			ck.Require().Eq(c.wantStatus, rec.Code, "status")
			ck.False(c.wantUser != "" && (gotIdentity == nil || gotIdentity.Username != c.wantUser), "identity = %+v, want %q", gotIdentity, c.wantUser)
		})
	}
}

// recordingChatStore is a fake proxyStore for the OpenAI face.
type recordingChatStore struct {
	completes, fails int
	last             capture.TurnResult
	lastIntent       capture.TurnIntent
}

func (f *recordingChatStore) EnsureConversationByExternalRef(_ context.Context, _ capture.ConversationRef) (string, error) {
	return "conv-openai", nil
}
func (s *recordingChatStore) ResolveThreadConversation(ctx context.Context, ref capture.ConversationRef, threadID string) (string, error) {
	return s.EnsureConversationByExternalRef(ctx, ref)
}

func (s *recordingChatStore) ThreadOfPredecessorInSession(ctx context.Context, session, prevMessageID string) (string, error) {
	_ = session
	_ = prevMessageID
	return "", nil
}

func (f *recordingChatStore) InsertTurnIntent(_ context.Context, t capture.TurnIntent) (string, time.Time, error) {
	f.lastIntent = t
	return "turn-openai", time.Unix(0, 0), nil
}

func (f *recordingChatStore) CompleteTurn(_ context.Context, r capture.TurnResult) error {
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

func (f *recordingChatStore) RecordThread(_ context.Context, _, _, _ string, _ time.Time, _, _ string, _ bool) error {
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

	assert.NewCollecting(t).Eq("conv-openai", gotSession, "x-session-id fallback")
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

	assert.NewCollecting(t).Eq("conv-openai", gotSession, "x-session-id fallback")
}

// End-to-end tee against a REAL capture store (RAFIKI_TEST_DSN): adversarial
// chunking upstream, then assert the persisted turn is complete with a valid
// reassembled canonical response — the fake-store tests can't catch
// store-layer marshaling issues.
func TestMessagesTeeWithRealStore(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Require().Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	c.Require().NoError(err)
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_tee_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	c.Require().NoError(err)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.Require().NoError(err)
	t.Cleanup(pool.Close)
	c.Require().NoError(store.Migrate(ctx, pool))

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
	p := NewMessagesProxy(capture.NewCaptureStore(pool), nil, "real-key", upstream.URL, "", nil, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fidelityBody))
	req.Header.Set("X-Rafiki-Session", "tee-real-store")
	p.ServeHTTP(rec, req)

	c.Require().Eq(weirdSSE, rec.Body.String(), "stream mutated under real store")
	var status string
	var responseNull bool
	var outTokens int64
	c.Require().NoError(pool.QueryRow(ctx, `SELECT t.status, t.response IS NULL, t.output_tokens
		FROM conversations.conversation_turn t
		JOIN conversations.conversation c ON c.id = t.conversation_id
		WHERE c.external_ref = 'tee-real-store'`).Scan(&status, &responseNull, &outTokens), "read captured turn")
	c.False(status != "complete" || outTokens != 7, "turn = %s/%d tokens, want complete/7", status, outTokens)
	c.True(responseNull, "turn.response should be NULL; decomposition replaces the full-JSONB write")
	// The canonical response's marshaling correctness is now verified via the
	// decomposed assistant conversation_message instead of turn.response.
	var msgContent string
	c.Require().NoError(pool.QueryRow(ctx, `SELECT m.content::text
		FROM conversations.conversation_message m
		JOIN conversations.conversation c ON c.id = m.conversation_id
		WHERE c.external_ref = 'tee-real-store' AND m.role = 'assistant'`).Scan(&msgContent), "read decomposed assistant message")
	var content any
	c.NoError(json.Unmarshal([]byte(msgContent), &content), "decomposed assistant content is not valid JSON")
}

// OpenAI SSE tool_calls deltas (fragmented arguments) reassemble into the
// canonical message; undecodable chunks and empty streams are parse errors.
func TestParseOpenAIResponseToolCallsAndStrictness(t *testing.T) {
	c := assert.NewCollecting(t)
	toolSSE := `data: {"id":"cc-2","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"cc-2","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"sf\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	finish, _, canonical, err := parseOpenAIResponse("text/event-stream", []byte(toolSSE))
	c.Require().NoError(err, "parse")
	c.Eq("tool_calls", finish, "finish =")
	var m struct {
		Choices []struct {
			Message struct {
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	c.Require().NoError(json.Unmarshal(canonical, &m), "canonical")
	tc := m.Choices[0].Message.ToolCalls
	c.False(len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "get_weather" ||
		tc[0].Function.Arguments != `{"city":"sf"}`, "tool_calls reassembly wrong: %+v", tc)

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

// Liveness, not just fidelity: a chunk the proxy reads must reach a REAL
// client socket promptly — flushed, not buffered — even when the upstream
// then goes silent. Claude Code's stream idle watchdog
// (CLAUDE_STREAM_IDLE_TIMEOUT_MS, default 300s) does not count SSE ping
// events as activity, so during a long-thinking turn the only thing standing
// between the client and "Response stalled mid-stream" is every upstream
// byte being forwarded the moment it arrives. httptest.NewRecorder cannot
// test this (it buffers everything), so this uses a real client/server pair:
// the upstream parks after message_start + a ping, and the client must
// observe the ping BEFORE the upstream is released.
func TestMessagesTeeForwardsPingsDuringUpstreamSilence(t *testing.T) {
	c := assert.NewCollecting(t)
	sseHead := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_ping","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n"
	ssePing := "event: ping\n" + `data: {"type":"ping"}` + "\n\n"
	sseTail := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Envelope + first ping, then the "long thinking" silence: nothing
		// more is sent until the test releases the stream.
		_, _ = io.WriteString(w, sseHead)
		_, _ = io.WriteString(w, ssePing)
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, sseTail)
		flusher.Flush()
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(fidelityBody))
	c.Require().NoError(err, "post")
	defer resp.Body.Close()

	lines := make(chan string)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			line, rerr := r.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if rerr != nil {
				readErr <- rerr
				return
			}
		}
	}()

	// The ping must arrive while the upstream is still parked. If the proxy
	// buffered instead of flushing, nothing arrives here and the test times
	// out — which is exactly the "client sees a stalled stream" failure.
	var got strings.Builder
	deadline := time.After(10 * time.Second)
	for sawPing := false; !sawPing; {
		select {
		case line := <-lines:
			got.WriteString(line)
			sawPing = line == `data: {"type":"ping"}`+"\n"
		case rerr := <-readErr:
			t.Fatalf("stream ended before the ping arrived: %v (got %q)", rerr, got.String())
		case <-deadline:
			t.Fatalf("ping never reached the client during upstream silence; proxy is buffering (got %q)", got.String())
		}
	}
	close(release)

	// Drain to EOF: the whole stream must be byte-identical end to end.
	for {
		select {
		case line := <-lines:
			got.WriteString(line)
		case rerr := <-readErr:
			c.Require().False(rerr != io.EOF, "read: %v", rerr)
			c.Eq(sseHead+ssePing+sseTail, got.String(), "stream mutated:\n got")
			return
		case <-time.After(10 * time.Second):
			t.Fatal("stream did not finish after upstream release")
		}
	}
}

// ConversationTokens satisfies proxyStore; the tee fidelity tests assert on
// streamed bytes, not cost, so an empty rollup is the honest answer here.
func (s *recordingChatStore) ConversationTokens(context.Context, string) ([]capture.ModelTokens, error) {
	return nil, nil
}
