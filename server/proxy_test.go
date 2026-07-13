package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/timescale/rafiki/routing"
	"github.com/timescale/savannah-common/go/tslogs"
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
	completeErr          error // when set, CompleteTurn returns it (to exercise the FailTurn fallback)
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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
	if fs.fails != 1 || fs.completes != 0 {
		t.Errorf("capture: fails=%d completes=%d, want 1/0", fs.fails, fs.completes)
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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

// A slash model on a proxy with no OpenRouter key returns 502 AND must resolve
// the turn it began, or the row is stranded 'pending' forever (a false orphan).
func TestMessagesProxySlashWithoutOpenRouterFailsTurn(t *testing.T) {
	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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
	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
	cat := routing.NewModelCatalog(nil, time.Minute, logger)
	p := &MessagesProxy{logger: logger, catalog: cat, defaultModel: "haiku-latest"}
	// Seed the catalog via its test hook instead of the network:
	cat.SeedForTest([]routing.CatalogEntry{{ID: "anthropic/claude-haiku-4.5", Created: 1}, {ID: "anthropic/claude-sonnet-5", Created: 2}})

	// empty model -> default (haiku-latest) -> concrete anthropic id
	body, resolved, err := p.resolveModel([]byte(`{"max_tokens":1,"messages":[]}`))
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
	body, resolved, err = p.resolveModel([]byte(`{"model":"sonnet-latest","max_tokens":1}`))
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
	body, resolved, err = p.resolveModel([]byte(`{"model":"claude-opus-4-8"}`))
	if err != nil {
		t.Fatalf("concrete errored: %v", err)
	}
	if got := modelOf(t, body); got != "claude-opus-4-8" {
		t.Errorf("concrete = %q, want unchanged", got)
	}
	if resolved != "claude-opus-4-8" {
		t.Errorf("concrete resolved = %q, want unchanged", resolved)
	}
	// a "<family>-latest" the catalog can't resolve -> error (no hardcoded fallback)
	if _, _, err = p.resolveModel([]byte(`{"model":"opus-latest"}`)); err == nil {
		t.Error("opus-latest absent from catalog must error")
	}
	// malformed body -> best-effort passthrough, no error
	garbage := []byte(`not json`)
	body, resolved, err = p.resolveModel(garbage)
	if err != nil || resolved != "" || string(body) != string(garbage) {
		t.Errorf("malformed body = (%q,%q,%v), want passthrough+empty+nil", body, resolved, err)
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
