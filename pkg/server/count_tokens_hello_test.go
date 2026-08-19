// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/rawtrace"
)

// count_tokens is metadata, not a conversation turn: it must be proxied with
// model resolution and upstream routing but must NOT open a capture turn.
func TestServeCountTokensForwardsWithoutCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("upstream path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "real-key" {
			t.Errorf("upstream x-api-key = %q, want real-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":42}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hi"}]}`))
	p.ServeCountTokens(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":42`) {
		t.Errorf("client body = %q, want the count_tokens response", rec.Body.String())
	}
	if fs.intents != 0 || fs.completes != 0 || fs.fails != 0 {
		t.Errorf("capture: intents=%d completes=%d fails=%d, want 0/0/0 (no turn for a token count)",
			fs.intents, fs.completes, fs.fails)
	}
}

// The connectivity preflight probe (HEAD /api/hello) is forwarded to the
// Anthropic primary and does not open a capture turn either.
func TestServeHelloForwardsProbeWithoutCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("upstream method = %q, want HEAD", r.Method)
		}
		if r.URL.Path != "/api/hello" {
			t.Errorf("upstream path = %q, want /api/hello", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/api/hello", nil)
	p.ServeHello(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fs.intents != 0 || fs.completes != 0 || fs.fails != 0 {
		t.Errorf("capture: intents=%d completes=%d fails=%d, want 0/0/0 (no turn for a probe)",
			fs.intents, fs.completes, fs.fails)
	}
}

// Recording is the global RAFIKI_RECORD_REQUESTS=1 switch or the per-session
// X-Rafiki-Record-Requests header; the store itself must be non-nil.
func TestShouldRecord(t *testing.T) {
	p := &MessagesProxy{rawTrace: &rawtrace.RawTraceStore{}}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	if p.shouldRecord(req) {
		t.Error("shouldRecord = true with no global switch and no header")
	}
	p.rawTraceAll = true
	if !p.shouldRecord(req) {
		t.Error("shouldRecord = false with rawTraceAll set")
	}
	p.rawTraceAll = false
	req.Header.Set("X-Rafiki-Record-Requests", "1")
	if !p.shouldRecord(req) {
		t.Error("shouldRecord = false with X-Rafiki-Record-Requests: 1")
	}
	req.Header.Set("X-Rafiki-Record-Requests", "0")
	if p.shouldRecord(req) {
		t.Error("shouldRecord = true with X-Rafiki-Record-Requests: 0")
	}

	p.rawTrace = nil
	p.rawTraceAll = true
	if p.shouldRecord(req) {
		t.Error("shouldRecord = true with no raw trace store")
	}
}

// The mux must route /v1/messages/count_tokens to the count_tokens handler and
// leave /v1/messages on the main face — the two paths sit next to each other and
// a routing regression here is exactly the kind of thing a unit test misses.
func TestHandlerMountServesCountTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":7}`)
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	messages := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)

	mux := http.NewServeMux()
	(&Handler{Messages: messages}).Mount(mux, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude","messages":[]}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"input_tokens":7`) {
		t.Errorf("count_tokens through mux = %d %q, want 200 + input_tokens", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "end_turn") {
		t.Errorf("/v1/messages through mux = %d %q, want 200 + message", rec2.Code, rec2.Body.String())
	}
}
