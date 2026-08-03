// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

type fakeProxyStore struct {
	intents              int
	completes            int
	fails                int
	lastStop             string
	lastOut              int64
	lastUpstream         string
	lastLatencyMS        int
	lastIntentModel      string
	lastIntentSource     string
	lastIntentAuthorKind string
	lastFailMsg          string // errMsg passed to the last FailTurn
	decomposes           int    // DecomposeRequest call count
	completeErr          error  // when set, CompleteTurn returns it (to exercise the FailTurn fallback)
}

func (f *fakeProxyStore) EnsureConversationByExternalRef(ctx context.Context, ref routing.ConversationRef) (string, error) {
	return "conv-1", nil
}

func (f *fakeProxyStore) InsertTurnIntent(ctx context.Context, t routing.TurnIntent) (string, time.Time, error) {
	f.intents++
	f.lastIntentModel = t.Model
	f.lastIntentSource = t.Source
	f.lastIntentAuthorKind = t.AuthorKind
	return "turn-1", time.Unix(0, 0), nil
}

func (f *fakeProxyStore) CompleteTurn(ctx context.Context, r routing.TurnResult) error {
	f.completes++
	f.lastStop = r.StopReason
	f.lastOut = r.OutputTokens
	f.lastUpstream = r.Upstream
	f.lastLatencyMS = r.LatencyMS
	return f.completeErr
}

func (f *fakeProxyStore) FailTurn(ctx context.Context, turnID string, createdAt time.Time, errMsg string) error {
	f.fails++
	f.lastFailMsg = errMsg
	return nil
}

func (f *fakeProxyStore) DecomposeRequest(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash string) (int, error) {
	f.decomposes++
	return 0, nil
}

func (f *fakeProxyStore) AppendResponseMessage(ctx context.Context, convID, turnID string, createdAt time.Time, ordinal int, canonical []byte, in, out int64, stopReason string) error {
	return nil
}

func TestMessagesProxyStreamsAndCaptures(t *testing.T) {
	// Fake Anthropic upstream returns a minimal SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "real-key" {
			t.Errorf("upstream missing real key, got %q", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs // inject fake

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "sess-1")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not receive the streamed body: %q", rec.Body.String())
	}
	if fs.intents != 1 || fs.completes != 1 {
		t.Errorf("capture: intents=%d completes=%d, want 1/1", fs.intents, fs.completes)
	}
	if fs.lastStop != "end_turn" || fs.lastOut != 9 {
		t.Errorf("captured stop=%q out=%d, want end_turn/9", fs.lastStop, fs.lastOut)
	}
	if fs.lastIntentModel != "claude" {
		t.Errorf("captured intent model=%q, want claude", fs.lastIntentModel)
	}
	// No X-Rafiki-Source header → source defaults to "claude"; interactive proxy
	// turns are human-authored.
	if fs.lastIntentSource != "claude" || fs.lastIntentAuthorKind != "human" {
		t.Errorf("captured provenance source=%q author_kind=%q, want claude/human", fs.lastIntentSource, fs.lastIntentAuthorKind)
	}
	if fs.lastLatencyMS < 0 {
		t.Errorf("captured latency_ms=%d, want >= 0", fs.lastLatencyMS)
	}
}

func TestMessagesProxySourceHeaderOverridesEntrypoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("X-Rafiki-Source", "slack")
	p.ServeHTTP(rec, req)

	if fs.lastIntentSource != "slack" {
		t.Errorf("source = %q, want slack (from X-Rafiki-Source)", fs.lastIntentSource)
	}
}

func TestMessagesProxyCompleteTurnFailureFallsBackToFailTurn(t *testing.T) {
	// A successful stream whose CompleteTurn write fails must not strand the turn
	// as 'pending' — the proxy falls back to FailTurn so it lands as 'error'.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{completeErr: errors.New("boom: jsonb write failed")}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "sess-cfail")
	p.ServeHTTP(rec, req)

	if fs.completes != 1 {
		t.Errorf("CompleteTurn should have been attempted once, got %d", fs.completes)
	}
	if fs.fails != 1 {
		t.Errorf("FailTurn fallback should fire on CompleteTurn failure, got fails=%d", fs.fails)
	}
}

func TestMessagesProxyFailsTurnOnUpstreamError(t *testing.T) {
	// Upstream returns a non-2xx with no failover configured: the turn must be
	// recorded as failed, never completed.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error"}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs // inject fake

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "sess-err")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 passed through", rec.Code)
	}
	// A non-envelope error body must reach the client byte-for-byte unmangled
	// (surfaceProviderError returns false → passthrough).
	if got := rec.Body.String(); got != `{"type":"error","error":{"type":"api_error"}}` {
		t.Errorf("client body = %q, want the upstream body passed through unchanged", got)
	}
	if fs.fails != 1 || fs.completes != 0 {
		t.Errorf("capture: fails=%d completes=%d, want 1/0", fs.fails, fs.completes)
	}
	// The failure reason must carry the upstream error body, not just the status,
	// so a failed turn is diagnosable from the capture store alone.
	if !strings.Contains(fs.lastFailMsg, "upstream status 500") {
		t.Errorf("fail msg = %q, want it to mention the status", fs.lastFailMsg)
	}
	if !strings.Contains(fs.lastFailMsg, `"api_error"`) {
		t.Errorf("fail msg = %q, want it to include the upstream error body", fs.lastFailMsg)
	}
	// The request is decomposed even on failure, so the messages that triggered
	// the error are recorded.
	if fs.decomposes != 1 {
		t.Errorf("decomposes = %d, want 1 (request decomposed on failure)", fs.decomposes)
	}
}

func TestMessagesProxyMalformedSuccessBecomes502(t *testing.T) {
	// The OpenRouter shared-pool failure that motivated the guard: a streaming
	// request gets HTTP 200 with a plain-text gateway error body. Forwarding
	// that hands the client an unparseable "success" it cannot retry — surface
	// it as the upstream failure it is, with the body preserved everywhere.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "error code: 521")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs // inject fake

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a malformed success", rec.Code)
	}
	// The client gets an Anthropic-style error envelope whose message carries
	// the upstream body, so the failure is displayable and diagnosable.
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "error code: 521") {
		t.Errorf("client body = %q, want an error envelope containing the upstream body", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if fs.fails != 1 || fs.completes != 0 {
		t.Errorf("capture: fails=%d completes=%d, want 1/0", fs.fails, fs.completes)
	}
	if !strings.Contains(fs.lastFailMsg, "text/plain") || !strings.Contains(fs.lastFailMsg, "error code: 521") {
		t.Errorf("fail msg = %q, want the content type and the upstream body", fs.lastFailMsg)
	}
}

func TestMessagesProxyNonStreamJSON200PassesThrough(t *testing.T) {
	// The guard must not mangle a legitimate non-streaming response: no
	// "stream" in the request → a JSON 200 is the expected shape.
	msg := `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, msg)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 passed through", rec.Code)
	}
	if rec.Body.String() != msg {
		t.Errorf("body mutated:\n got: %s\nwant: %s", rec.Body.String(), msg)
	}
}

func TestBoundedErrorBody(t *testing.T) {
	if got := boundedErrorBody([]byte("   ")); got != "" {
		t.Errorf("blank body => %q, want empty", got)
	}
	if got := boundedErrorBody([]byte(`{"e":1}`)); got != `{"e":1}` {
		t.Errorf("small body => %q, want passthrough", got)
	}
	// An oversized body whose byte cut lands mid multi-byte rune must still be
	// valid UTF-8 (3-byte '€' guarantees the 8 KiB offset splits a rune).
	out := boundedErrorBody([]byte(strings.Repeat("€", 3000)))
	if !utf8.ValidString(out) {
		t.Errorf("truncated output is not valid UTF-8")
	}
	if !strings.HasSuffix(out, "…(truncated)") {
		t.Errorf("want truncation marker, got tail %q", out[len(out)-16:])
	}
}

func TestSurfaceProviderError(t *testing.T) {
	innerRaw := `{"error":{"message":"Unsupported value: 'high' is not supported with the 'gpt-5-codex' model. Supported values are: 'medium'.","param":"text.verbosity","code":"unsupported_value"}}`
	envelope, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":  "Provider returned error",
		"metadata": map[string]any{"raw": innerRaw, "provider_name": "OpenAI"},
	}})
	rawNotJSON, _ := json.Marshal(map[string]any{"error": map[string]any{
		"metadata": map[string]any{"raw": "not json"},
	}})

	cases := []struct {
		name         string
		in           []byte
		wantOK       bool
		wantContains string
	}{
		{"openrouter envelope", envelope, true, "OpenAI: Unsupported value: 'high'"},
		{"plain anthropic error", []byte(`{"type":"error","error":{"type":"api_error","message":"overloaded"}}`), false, ""},
		{"no metadata.raw", []byte(`{"error":{"message":"x","metadata":{"provider_name":"OpenAI"}}}`), false, ""},
		{"malformed body", []byte(`not json`), false, ""},
		{"raw not json", rawNotJSON, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := surfaceProviderError(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && !strings.Contains(string(out), c.wantContains) {
				t.Errorf("out = %s, want it to contain %q", out, c.wantContains)
			}
		})
	}
}

func TestMessagesProxySurfacesProviderErrorToClient(t *testing.T) {
	// OpenRouter buries the provider's real error in error.metadata.raw and shows
	// only "Provider returned error" at the top level. The proxy must lift the
	// real message so a client displaying error.message (Claude Code) sees it.
	innerRaw := `{"error":{"message":"Unsupported value: 'high' is not supported with the 'gpt-5-codex' model. Supported values are: 'medium'.","param":"text.verbosity","code":"unsupported_value"}}`
	envBytes, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":  "Provider returned error",
		"code":     400,
		"metadata": map[string]any{"raw": innerRaw, "provider_name": "OpenAI"},
	}})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(envBytes)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "sess-surface")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through", rec.Code)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("client body not JSON: %v", err)
	}
	if !strings.Contains(got.Error.Message, "OpenAI:") || !strings.Contains(got.Error.Message, "gpt-5-codex") {
		t.Errorf("client error.message = %q, want the provider detail surfaced", got.Error.Message)
	}
	// Content-Length must match the rewritten body, not the upstream's.
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(rec.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d", cl, rec.Body.Len())
	}
	// Capture still records the failure (with the original raw body) and
	// decomposes the request.
	if fs.fails != 1 || fs.decomposes != 1 {
		t.Errorf("fails=%d decomposes=%d, want 1/1", fs.fails, fs.decomposes)
	}
	if !strings.Contains(fs.lastFailMsg, "unsupported_value") {
		t.Errorf("fail msg = %q, want the original raw body stored", fs.lastFailMsg)
	}
}

func TestHandleUpstreamErrorContentEncoding(t *testing.T) {
	// A rewritten (surfaced) error body is plaintext, so an upstream
	// Content-Encoding must be dropped or the client mis-decodes it; a passthrough
	// (non-envelope) body keeps upstream's encoding. `br` is used because Go's
	// transport passes it through untouched (it only auto-decodes the gzip it
	// requests), so the strip logic is what's exercised.
	innerRaw := `{"error":{"message":"boom","code":"unsupported_value"}}`
	envBytes, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":  "Provider returned error",
		"metadata": map[string]any{"raw": innerRaw, "provider_name": "OpenAI"},
	}})

	cases := []struct {
		name         string
		body         []byte
		wantEncoding string // expected client-facing Content-Encoding
	}{
		{"surfaced rewrite drops encoding", envBytes, ""},
		{"passthrough preserves encoding", []byte(`{"type":"error","error":{"type":"api_error"}}`), "br"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "br")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(tc.body)
			}))
			defer upstream.Close()

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
			p.store = &fakeProxyStore{}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != tc.wantEncoding {
				t.Errorf("Content-Encoding = %q, want %q", got, tc.wantEncoding)
			}
		})
	}
}

func TestMessagesProxyFailsOverToOpenRouter(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529) // overloaded
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenRouter authenticates via Authorization: Bearer, not x-api-key.
		if got := r.Header.Get("Authorization"); got != "Bearer or-key" {
			t.Errorf("OR auth = %q, want %q", got, "Bearer or-key")
		}
		if r.Header.Get("x-api-key") != "" {
			t.Errorf("OR should not receive x-api-key, got %q", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", primary.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not get the OR stream: %q", rec.Body.String())
	}
	if fs.lastUpstream != "openrouter" {
		t.Errorf("captured upstream=%q, want openrouter", fs.lastUpstream)
	}
}

// A retryable primary failure (5xx/429) is retried against the primary per
// routing.RetryBackoffs before failing over; a primary that recovers within
// the retry budget never reaches OpenRouter.
func TestMessagesProxyRetriesPrimaryBeforeFailover(t *testing.T) {
	orig := routing.RetryBackoffs
	routing.RetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { routing.RetryBackoffs = orig }()

	var primaryCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls++
		if primaryCalls < 3 {
			w.WriteHeader(529) // overloaded — retryable
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("openrouter must not be called when the primary recovers within the retry budget")
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", primary.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec, req)

	if primaryCalls != 3 {
		t.Errorf("primary calls = %d, want 3 (2 failures + 1 success)", primaryCalls)
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not get the primary's stream: %q", rec.Body.String())
	}
	if fs.lastUpstream != "anthropic" {
		t.Errorf("captured upstream=%q, want anthropic", fs.lastUpstream)
	}
}

// A primary that stays down through the entire retry budget still fails over
// to OpenRouter, after exhausting all configured retries.
func TestMessagesProxyFailsOverAfterExhaustingRetries(t *testing.T) {
	orig := routing.RetryBackoffs
	routing.RetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { routing.RetryBackoffs = orig }()

	var primaryCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls++
		w.WriteHeader(529) // overloaded — retryable, never recovers
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", primary.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec, req)

	if primaryCalls != 1+len(routing.RetryBackoffs) {
		t.Errorf("primary calls = %d, want %d (1 initial + %d retries)", primaryCalls, 1+len(routing.RetryBackoffs), len(routing.RetryBackoffs))
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not get the OR stream: %q", rec.Body.String())
	}
	if fs.lastUpstream != "openrouter" {
		t.Errorf("captured upstream=%q, want openrouter", fs.lastUpstream)
	}
}

// An out-of-credit primary (a 400, so not retryable by status) fails over to
// OpenRouter immediately — no retry burst against an account that cannot pay
// for the request — and trips the breaker so the next request skips the
// primary entirely.
func TestMessagesProxyFailsOverWhenPrimaryOutOfCredit(t *testing.T) {
	orig := routing.RetryBackoffs
	routing.RetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { routing.RetryBackoffs = orig }()

	var primaryCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`)
	}))
	defer primary.Close()
	var orCalls int
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		orCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", primary.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	breaker := routing.NewBreaker(15 * time.Minute)
	p.SetFallback("or-key", orSrv.URL, breaker)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec, req)

	if primaryCalls != 1 {
		t.Errorf("primary calls = %d, want 1 (no retries against an unfunded account)", primaryCalls)
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("client did not get the OR stream: %q", rec.Body.String())
	}
	if fs.lastUpstream != "openrouter" {
		t.Errorf("captured upstream=%q, want openrouter", fs.lastUpstream)
	}
	if !breaker.Open() {
		t.Error("breaker must be open after an out-of-credit primary rejection")
	}

	// Breaker now open: the next request skips the primary entirely.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec2, req2)
	if primaryCalls != 1 {
		t.Errorf("primary calls = %d after a second request, want 1 (breaker open)", primaryCalls)
	}
	if orCalls != 2 {
		t.Errorf("openrouter calls = %d, want 2", orCalls)
	}
}

// An ordinary 400 is neither retryable nor failover-worthy: it is the caller's
// own bad request, so it must be surfaced verbatim rather than re-sent to a
// second provider that would reject it identically.
func TestMessagesProxyOrdinaryBadRequestDoesNotFailOver(t *testing.T) {
	const badReq = `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than or equal to 1"}}`
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, badReq)
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("openrouter must not be called for an ordinary bad request")
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", primary.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	breaker := routing.NewBreaker(15 * time.Minute)
	p.SetFallback("or-key", orSrv.URL, breaker)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	// The body must survive the classification peek intact.
	if rec.Body.String() != badReq {
		t.Errorf("client body = %q, want the upstream rejection verbatim %q", rec.Body.String(), badReq)
	}
	if breaker.Open() {
		t.Error("breaker must stay closed on a caller-caused 400")
	}
}

func TestMessagesProxySlashRoutesDirectToOpenRouter(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("primary must not be called for slash model")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenRouter authenticates via Authorization: Bearer, not x-api-key.
		if got := r.Header.Get("Authorization"); got != "Bearer or-key" {
			t.Errorf("OR auth = %q, want %q", got, "Bearer or-key")
		}
		if r.Header.Get("x-api-key") != "" {
			t.Errorf("OR should not receive x-api-key, got %q", r.Header.Get("x-api-key"))
		}
		orBody, _ := io.ReadAll(r.Body)
		if got := modelOf(t, orBody); got != "openai/gpt-4o" {
			t.Errorf("OR received model = %q, want openai/gpt-4o", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"marker":"from-openrouter","type":"message","stop_reason":"end_turn","usage":{"output_tokens":3}}`)
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", primary.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"openai/gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	p.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "from-openrouter") {
		t.Errorf("client did not get the OR response: %q", rec.Body.String())
	}
	if fs.lastUpstream != "openrouter" {
		t.Errorf("captured upstream=%q, want openrouter", fs.lastUpstream)
	}
}

// A pinned model line (routing provider pins) gets its provider preferences
// injected into the OpenRouter body; a caller-supplied provider object wins.
func TestMessagesProxyPinsProviderForPinnedModel(t *testing.T) {
	var orBodies [][]byte
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		orBodies = append(orBodies, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":3}}`)
	}))
	defer orSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", "http://unused-primary", "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = &fakeProxyStore{}
	p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))

	send := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer client-token")
		p.ServeHTTP(rec, req)
	}
	send(`{"model":"z-ai/glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	send(`{"model":"z-ai/glm-5.2","provider":{"only":["baseten"]},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	send(`{"model":"openai/gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if len(orBodies) != 3 {
		t.Fatalf("OpenRouter received %d requests, want 3", len(orBodies))
	}

	providerOf := func(body []byte) map[string]any {
		t.Helper()
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal OR body: %v", err)
		}
		prov, _ := payload["provider"].(map[string]any)
		return prov
	}
	if prov := providerOf(orBodies[0]); fmt.Sprintf("%v", prov["only"]) != "[fireworks]" {
		t.Errorf("pinned model provider = %v, want only=[fireworks]", prov)
	}
	if prov := providerOf(orBodies[1]); fmt.Sprintf("%v", prov["only"]) != "[baseten]" {
		t.Errorf("caller-supplied provider must win, got %v", prov)
	}
	if prov := providerOf(orBodies[2]); prov != nil {
		t.Errorf("unpinned model must carry no provider, got %v", prov)
	}
}

// A slash model on a proxy with no OpenRouter key returns 502 AND must resolve
// the turn it began, or the row is stranded 'pending' forever (a false orphan).
func TestMessagesProxySlashWithoutOpenRouterFailsTurn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", "http://unused", "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs // capturing, but no setFallback -> orKey == ""

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"openai/gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openai/gpt-4o") {
		t.Errorf("body should name the model, got %q", rec.Body.String())
	}
	if fs.intents != 1 || fs.fails != 1 || fs.completes != 0 {
		t.Errorf("intents=%d fails=%d completes=%d, want 1/1/0 (turn must be failed, not stranded)", fs.intents, fs.fails, fs.completes)
	}
}

func TestProxyResolveModel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cat := routing.NewModelCatalog(nil, time.Minute, logger)
	p := &MessagesProxy{logger: logger, catalog: cat, defaultModel: "haiku-latest"}
	// Seed the catalog via its test hook instead of the network:
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
		{ID: "moonshotai/kimi-k3", Created: 3},
	})

	// empty model -> default (haiku-latest) -> concrete anthropic id
	body, resolved, err := p.resolveAndAdapt([]byte(`{"max_tokens":1,"messages":[]}`))
	if err != nil {
		t.Fatalf("empty->default errored: %v", err)
	}
	if got := modelOf(t, body); got != "claude-haiku-4-5" {
		t.Errorf("empty->default = %q, want claude-haiku-4-5", got)
	}
	if resolved != "claude-haiku-4-5" {
		t.Errorf("empty->default resolved = %q, want claude-haiku-4-5", resolved)
	}
	// explicit sonnet-latest -> concrete
	body, resolved, err = p.resolveAndAdapt([]byte(`{"model":"sonnet-latest","max_tokens":1}`))
	if err != nil {
		t.Fatalf("sonnet-latest errored: %v", err)
	}
	if got := modelOf(t, body); got != "claude-sonnet-5" {
		t.Errorf("sonnet-latest = %q, want claude-sonnet-5", got)
	}
	if resolved != "claude-sonnet-5" {
		t.Errorf("sonnet-latest resolved = %q, want claude-sonnet-5", resolved)
	}
	// concrete id -> untouched
	body, resolved, err = p.resolveAndAdapt([]byte(`{"model":"claude-opus-4-8"}`))
	if err != nil {
		t.Fatalf("concrete errored: %v", err)
	}
	if got := modelOf(t, body); got != "claude-opus-4-8" {
		t.Errorf("concrete = %q, want unchanged", got)
	}
	if resolved != "claude-opus-4-8" {
		t.Errorf("concrete resolved = %q, want unchanged", resolved)
	}
	// short model alias -> the line's newest OR slash id (selectUpstream then
	// routes the slash id to OpenRouter, per the slash-routing tests above)
	body, resolved, err = p.resolveAndAdapt([]byte(`{"model":"kimi-k3","max_tokens":1}`))
	if err != nil {
		t.Fatalf("kimi-k3 errored: %v", err)
	}
	if got := modelOf(t, body); got != "moonshotai/kimi-k3" {
		t.Errorf("kimi-k3 = %q, want moonshotai/kimi-k3", got)
	}
	if resolved != "moonshotai/kimi-k3" {
		t.Errorf("kimi-k3 resolved = %q, want moonshotai/kimi-k3", resolved)
	}
	// a model alias the catalog can't resolve -> error (no hardcoded fallback)
	if _, _, err = p.resolveAndAdapt([]byte(`{"model":"deepseek-v4-pro"}`)); err == nil {
		t.Error("deepseek-v4-pro absent from catalog must error")
	}
	// a "<family>-latest" the catalog can't resolve -> error (no hardcoded fallback)
	if _, _, err = p.resolveAndAdapt([]byte(`{"model":"opus-latest"}`)); err == nil {
		t.Error("opus-latest absent from catalog must error")
	}
	// malformed body -> best-effort passthrough, no error
	garbage := []byte(`not json`)
	body, resolved, err = p.resolveAndAdapt(garbage)
	if err != nil || resolved != "" || string(body) != string(garbage) {
		t.Errorf("malformed body = (%q,%q,%v), want passthrough+empty+nil", body, resolved, err)
	}
}

// End-to-end: a real proxied turn decomposes into conversation_message rows
// (user + assistant) and leaves conversation_turn.request NULL — the bulky
// full-request JSONB is no longer written now that decomposition covers it.
func TestProxy_DecomposesConversation(t *testing.T) {
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
	name := fmt.Sprintf("rafiki_decompose_%d", time.Now().UnixNano())
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

	cs := routing.NewCaptureStore(pool)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"hi back"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(cs, nil, "key", upstream.URL, "claude", nil, logger)

	body := `{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Rafiki-Session", "sess-decompose-1")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// user message + assistant response decomposed; turn.request left NULL
	var msgs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message`).Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	if msgs != 2 {
		t.Errorf("conversation_message count = %d, want 2", msgs)
	}
	var reqNull bool
	if err := pool.QueryRow(ctx,
		`SELECT request IS NULL FROM conversations.conversation_turn LIMIT 1`).Scan(&reqNull); err != nil {
		t.Fatal(err)
	}
	if !reqNull {
		t.Error("conversation_turn.request should be NULL; decomposition replaces the full-JSONB write")
	}
}

func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m.Model
}

func TestMessagesProxyAdaptsEffort(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		reqEffort  string // "" means no output_config
		wantEffort string // "" means output_config/effort absent on the wire
		rawReqBody string // if set, used verbatim in place of the model/reqEffort construction
	}{
		{"clamp high to medium", "openai/gpt-5-codex", "high", "medium", ""},
		{"already allowed untouched", "openai/gpt-5-codex", "medium", "medium", ""},
		{"strip on empty set", "vendor/rejects-effort", "high", "", ""},
		{"passthrough when absent", "vendor/unmapped", "high", "high", ""},
		{"mapped model, no output_config at all", "openai/gpt-5-codex", "", "", ""},
		{"mapped model, output_config present without effort key", "openai/gpt-5-codex", "", "", `{"model":"openai/gpt-5-codex","stream":true,"output_config":{"other_field":true}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotEffort string
			var sawOutputConfig bool
			or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var m map[string]any
				_ = json.Unmarshal(body, &m)
				if oc, ok := m["output_config"].(map[string]any); ok {
					sawOutputConfig = true
					gotEffort, _ = oc["effort"].(string)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
			}))
			defer or.Close()

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			p := NewMessagesProxy(nil, nil, "real-key", "http://unused.example", "" /*default*/, nil /*catalog*/, logger)
			p.SetFallback("or-key", or.URL, nil) // slash ids route here
			// Pre-seed the runtime cache to exercise proactive clamping (the
			// learn-from-rejection path is covered by TestMessagesProxyEffortRetry).
			p.effortCache.Learn("openai/gpt-5-codex", []string{"medium"})
			p.effortCache.Learn("vendor/rejects-effort", []string{})

			reqBody := tc.rawReqBody
			if reqBody == "" {
				reqBody = `{"model":"` + tc.model + `","stream":true}`
				if tc.reqEffort != "" {
					reqBody = `{"model":"` + tc.model + `","stream":true,"output_config":{"effort":"` + tc.reqEffort + `"}}`
				}
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if tc.wantEffort == "" {
				if sawOutputConfig && gotEffort != "" {
					t.Errorf("effort should be stripped, upstream saw %q", gotEffort)
				}
			} else if gotEffort != tc.wantEffort {
				t.Errorf("upstream effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

// TestMessagesProxyEffortRetry covers the learn-from-rejection path: a cold
// cache sends the client's effort, the upstream rejects it enumerating the
// allowed set, and the proxy learns, clamps, and retries once — then clamps
// proactively on the next request without a second rejection.
func TestMessagesProxyEffortRetry(t *testing.T) {
	innerRaw := `{"error":{"message":"Unsupported value: 'high' is not supported with the 'gpt-5-codex' model. Supported values are: 'medium'.","param":"text.verbosity","code":"unsupported_value"}}`
	envBytes, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":  "Provider returned error",
		"metadata": map[string]any{"raw": innerRaw, "provider_name": "OpenAI"},
	}})

	var mu sync.Mutex
	var efforts []string // effort seen by the upstream, per request
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		eff := ""
		if oc, ok := m["output_config"].(map[string]any); ok {
			eff, _ = oc["effort"].(string)
		}
		mu.Lock()
		efforts = append(efforts, eff)
		mu.Unlock()
		if eff == "high" { // reject exactly what the model can't do
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(envBytes)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer or.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", "http://unused.example", "" /*default*/, nil /*catalog*/, logger)
	p.SetFallback("or-key", or.URL, nil) // slash ids route here

	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"openai/gpt-5-codex","stream":true,"output_config":{"effort":"high"}}`))
		p.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK { // cold: reject -> learn -> clamp -> retry -> 200
		t.Fatalf("first request: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(); rec.Code != http.StatusOK { // warm: proactively clamped, no rejection
		t.Fatalf("second request: status = %d, body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"high", "medium", "medium"} // req1 high (rejected), req1-retry medium, req2 medium
	if !reflect.DeepEqual(efforts, want) {
		t.Errorf("upstream efforts = %v, want %v", efforts, want)
	}
}
