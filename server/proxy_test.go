package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/timescale/rafiki/routing"
	"github.com/timescale/rafiki/store"
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

func (f *fakeProxyStore) DecomposeRequest(ctx context.Context, convID, turnID string, createdAt time.Time, reqBody []byte, prefixHash string) (int, error) {
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
		{ID: "moonshotai/kimi-k3", Created: 3},
	})

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
	// short model alias -> the line's newest OR slash id (selectUpstream then
	// routes the slash id to OpenRouter, per the slash-routing tests above)
	body, resolved, err = p.resolveModel([]byte(`{"model":"kimi-k3","max_tokens":1}`))
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
	if _, _, err = p.resolveModel([]byte(`{"model":"deepseek-v4-pro"}`)); err == nil {
		t.Error("deepseek-v4-pro absent from catalog must error")
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

	logger, _ := tslogs.NewLogger(tslogs.LevelError, false, "test", 0)
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
