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
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

type fakeProxyStore struct {
	convTokens []capture.ModelTokens

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
	threads              int    // RecordThread call count
	lastSession          string // session passed to the last RecordThread
	lastPrevMsg          string // prevMessageID passed to the last RecordThread
	lastOwnMsg           string // ownMessageID passed to the last RecordThread
	lastIsSubagent       bool   // isSubagent passed to the last RecordThread
	threadErr            error  // when set, RecordThread returns it (to exercise the swallow)
	appendErr            error  // when set, AppendResponseMessage returns it (to exercise the FailTurn path)

	// threadID, when non-empty, is the thread id ThreadOfPredecessorInSession
	// answers with, standing in for a request on a Claude Code subagent thread.
	// Empty (the default) is a root-thread request.
	threadID        string
	familyExists    bool   // what SessionFamilyExists answers (default false: a fresh session)
	familyErr       error  // when set, SessionFamilyExists returns it
	lastThreadID    string // threadID the last ResolveThreadConversation received
	lastResolvedRef string // external_ref the last ResolveThreadConversation received
	lastIntentConv  string // conversation the last InsertTurnIntent landed on
	lastIntentID    string // TurnIntent.ID the last InsertTurnIntent received (a pre-minted founding turn)
}

func (f *fakeProxyStore) EnsureConversationByExternalRef(ctx context.Context, ref capture.ConversationRef) (string, error) {
	return "conv-1", nil
}
func (s *fakeProxyStore) ResolveThreadConversation(ctx context.Context, ref capture.ConversationRef, threadID string) (string, error) {
	s.lastThreadID = threadID
	// Model the store's contract: the effective external_ref is the session
	// value suffixed with the thread id, which the real method applies inside.
	effective := ref.ExternalRef
	if threadID != "" && effective != "" {
		effective = effective + ":" + threadID
	}
	s.lastResolvedRef = effective
	if threadID != "" {
		return "conv-branch", nil
	}
	return s.EnsureConversationByExternalRef(ctx, ref)
}

func (s *fakeProxyStore) ThreadOfPredecessorInSession(ctx context.Context, session, prevMessageID string) (string, error) {
	_ = session
	_ = prevMessageID
	return s.threadID, nil
}

// SessionFamilyExists models the real store's discriminator. Implementing it
// on the fake keeps the default FALSE: a fresh session, the main-thread
// founding case, which is what every pre-existing test assumes.
func (s *fakeProxyStore) SessionFamilyExists(ctx context.Context, session string) (bool, error) {
	_ = ctx
	_ = session
	return s.familyExists, s.familyErr
}

func (f *fakeProxyStore) InsertTurnIntent(ctx context.Context, t capture.TurnIntent) (string, time.Time, error) {
	f.intents++
	f.lastIntentConv = t.ConversationID
	f.lastIntentID = t.ID
	f.lastIntentModel = t.Model
	f.lastIntentSource = t.Source
	f.lastIntentAuthorKind = t.AuthorKind
	return "turn-1", time.Unix(0, 0), nil
}

func (f *fakeProxyStore) CompleteTurn(ctx context.Context, r capture.TurnResult) error {
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
	return f.appendErr
}

func (f *fakeProxyStore) RecordThread(ctx context.Context, session, convID, turnID string, createdAt time.Time, prevMessageID, ownMessageID string, isSubagent bool) error {
	f.threads++
	f.lastSession = session
	f.lastPrevMsg = prevMessageID
	f.lastOwnMsg = ownMessageID
	f.lastIsSubagent = isSubagent
	return f.threadErr
}

func TestMessagesProxyStreamsAndCaptures(t *testing.T) {
	c := assert.NewCollecting(t)
	// Fake Anthropic upstream returns a minimal SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.Eq("real-key", r.Header.Get("x-api-key"), "upstream missing real key, got")
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

	c.Require().Eq(http.StatusOK, rec.Code, "status =")
	c.StrContains(rec.Body.String(), "message_stop", "client did not receive the streamed body")
	if fs.intents != 1 || fs.completes != 1 {
		t.Errorf("capture: intents=%d completes=%d, want 1/1", fs.intents, fs.completes)
	}
	if fs.lastStop != "end_turn" || fs.lastOut != 9 {
		t.Errorf("captured stop=%q out=%d, want end_turn/9", fs.lastStop, fs.lastOut)
	}
	c.Eq("claude", fs.lastIntentModel, "captured intent model")
	// No X-Rafiki-Source header → source defaults to "claude"; interactive proxy
	// turns are human-authored.
	if fs.lastIntentSource != "claude" || fs.lastIntentAuthorKind != "human" {
		t.Errorf("captured provenance source=%q author_kind=%q, want claude/human", fs.lastIntentSource, fs.lastIntentAuthorKind)
	}
	c.GreaterOrEqual(0, fs.lastLatencyMS, "captured latency_ms")
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

	assert.NewCollecting(t).Eq("slack", fs.lastIntentSource, "source")
}

func TestMessagesProxyCompleteTurnFailureFallsBackToFailTurn(t *testing.T) {
	c := assert.NewCollecting(t)
	// A successful stream whose CompleteTurn write fails must not strand the turn
	// as 'pending'; the proxy falls back to FailTurn so it lands as 'error'.
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

	c.Eq(1, fs.completes, "CompleteTurn should have been attempted once, got")
	c.Eq(1, fs.fails, "FailTurn fallback should fire on CompleteTurn failure, got fails=")
}

func TestMessagesProxyAppendFailureFallsBackToFailTurn(t *testing.T) {
	c := assert.NewCollecting(t)
	// A successful stream whose response write fails must not strand the turn
	// as 'pending'; the proxy falls back to FailTurn so it lands as 'error'.
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
	fs := &fakeProxyStore{appendErr: fmt.Errorf("%w: conversation c ordinal 5", capture.ErrOrdinalOccupied)}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "sess-cappend")
	p.ServeHTTP(rec, req)

	c.Eq(1, fs.fails, "FailTurn fallback should fire on AppendResponseMessage failure, got fails=")
	c.StrContains(fs.lastFailMsg, "append response failed:", "fail message")
}

func TestMessagesProxyFailsTurnOnUpstreamError(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(http.StatusInternalServerError, rec.Code, "status")
	// A non-envelope error body must reach the client byte-for-byte unmangled
	// (surfaceProviderError returns false → passthrough).
	c.Eq(`{"type":"error","error":{"type":"api_error"}}`, rec.Body.String(), "client body")
	if fs.fails != 1 || fs.completes != 0 {
		t.Errorf("capture: fails=%d completes=%d, want 1/0", fs.fails, fs.completes)
	}
	// The failure reason must carry the upstream error body, not just the status,
	// so a failed turn is diagnosable from the capture store alone.
	c.StrContains(fs.lastFailMsg, "upstream status 500", "fail msg")
	c.StrContains(fs.lastFailMsg, `"api_error"`, "fail msg")
	// The request is decomposed even on failure, so the messages that triggered
	// the error are recorded.
	c.Eq(1, fs.decomposes, "decomposes")
}

func TestMessagesProxyMalformedSuccessBecomes502(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(http.StatusBadGateway, rec.Code, "status")
	// The client gets an Anthropic-style error envelope whose message carries
	// the upstream body, so the failure is displayable and diagnosable.
	body := rec.Body.String()
	c.False(!strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "error code: 521"), "client body = %q, want an error envelope containing the upstream body", body)
	c.StrContains(rec.Header().Get("Content-Type"), "application/json", "content-type")
	if fs.fails != 1 || fs.completes != 0 {
		t.Errorf("capture: fails=%d completes=%d, want 1/0", fs.fails, fs.completes)
	}
	if !strings.Contains(fs.lastFailMsg, "text/plain") || !strings.Contains(fs.lastFailMsg, "error code: 521") {
		t.Errorf("fail msg = %q, want the content type and the upstream body", fs.lastFailMsg)
	}
}

func TestMessagesProxyNonStreamJSON200PassesThrough(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Eq(msg, rec.Body.String(), "body mutated:\n got")
}

func TestBoundedErrorBody(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("", boundedErrorBody([]byte("   ")), "blank body =>")
	c.Eq(`{"e":1}`, boundedErrorBody([]byte(`{"e":1}`)), "small body =>")
	// An oversized body whose byte cut lands mid multi-byte rune must still be
	// valid UTF-8 (3-byte '€' guarantees the 8 KiB offset splits a rune).
	out := boundedErrorBody([]byte(strings.Repeat("€", 3000)))
	c.True(utf8.ValidString(out), "truncated output is not valid UTF-8")
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
			ck := assert.NewCollecting(t)
			out, ok := surfaceProviderError(c.in)
			ck.Require().Eq(c.wantOK, ok, "ok")
			ck.False(ok && !strings.Contains(string(out), c.wantContains), "out = %s, want it to contain %q", out, c.wantContains)
		})
	}
}

func TestMessagesProxySurfacesProviderErrorToClient(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Require().Eq(http.StatusBadRequest, rec.Code, "status")
	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	c.Require().NoError(json.Unmarshal(rec.Body.Bytes(), &got), "client body not JSON")
	c.False(!strings.Contains(got.Error.Message, "OpenAI:") || !strings.Contains(got.Error.Message, "gpt-5-codex"), "client error.message = %q, want the provider detail surfaced", got.Error.Message)
	// Content-Length must match the rewritten body, not the upstream's.
	cl := rec.Header().Get("Content-Length")
	c.Eq(fmt.Sprint(rec.Body.Len()), cl, "Content-Length = %q, want %d", cl, rec.Body.Len())
	// Capture still records the failure (with the original raw body) and
	// decomposes the request.
	if fs.fails != 1 || fs.decomposes != 1 {
		t.Errorf("fails=%d decomposes=%d, want 1/1", fs.fails, fs.decomposes)
	}
	c.StrContains(fs.lastFailMsg, "unsupported_value", "fail msg")
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
			c := assert.NewCollecting(t)
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

			c.Require().Eq(http.StatusBadRequest, rec.Code, "status")
			c.Eq(tc.wantEncoding, rec.Header().Get("Content-Encoding"), "Content-Encoding")
		})
	}
}

func TestMessagesProxyFailsOverToOpenRouter(t *testing.T) {
	c := assert.NewCollecting(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529) // overloaded
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenRouter authenticates via Authorization: Bearer, not x-api-key.
		c.Eq("Bearer or-key", r.Header.Get("Authorization"), "OR auth")
		c.Eq("", r.Header.Get("x-api-key"), "OR should not receive x-api-key, got")
		c.Eq("https://github.com/graveland/rafiki", r.Header.Get("Referer"), "OR Referer")
		c.Eq("rafiki", r.Header.Get("X-Openrouter-Title"), "OR X-OpenRouter-Title")
		c.NotEq("", r.Header.Get("x-session-id"), "OR x-session-id is empty")
		// Source defaults to "claude" when unset → no categories.
		c.Eq("", r.Header.Get("X-Openrouter-Categories"), "OR X-OpenRouter-Categories")
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

	c.StrContains(rec.Body.String(), "message_stop", "client did not get the OR stream")
	c.Eq("openrouter", fs.lastUpstream, "captured upstream")

	// rafiki-claude source → categories header expected.
	catsSeen := false
	orSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Openrouter-Categories"); got == "cli-agent" {
			catsSeen = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\n"+
			`data: {"type":"message_stop"}`+"\n\n")
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	req2.Header.Set("X-Rafiki-Source", "rafiki-claude")
	p.ServeHTTP(rec2, req2)
	c.True(catsSeen, "OR X-OpenRouter-Categories missing; want cli-agent when source is rafiki-claude")
}

// A retryable primary failure (5xx/429) is retried against the primary per
// routing.RetryBackoffs before failing over; a primary that recovers within
// the retry budget never reaches OpenRouter.
func TestMessagesProxyRetriesPrimaryBeforeFailover(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(3, primaryCalls, "primary calls")
	c.StrContains(rec.Body.String(), "message_stop", "client did not get the primary's stream")
	c.Eq("anthropic", fs.lastUpstream, "captured upstream")
}

// A primary that stays down through the entire retry budget still fails over
// to OpenRouter, after exhausting all configured retries.
func TestMessagesProxyFailsOverAfterExhaustingRetries(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(1+len(routing.RetryBackoffs), primaryCalls, "primary calls = %d, want %d (1 initial + %d retries)", primaryCalls, 1+len(routing.RetryBackoffs), len(routing.RetryBackoffs))
	c.StrContains(rec.Body.String(), "message_stop", "client did not get the OR stream")
	c.Eq("openrouter", fs.lastUpstream, "captured upstream")
}

// An out-of-credit primary (a 400, so not retryable by status) fails over to
// OpenRouter immediately — no retry burst against an account that cannot pay
// for the request — and trips the breaker so the next request skips the
// primary entirely.
func TestMessagesProxyFailsOverWhenPrimaryOutOfCredit(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(1, primaryCalls, "primary calls")
	c.StrContains(rec.Body.String(), "message_stop", "client did not get the OR stream")
	c.Eq("openrouter", fs.lastUpstream, "captured upstream")
	c.True(breaker.Open(), "breaker must be open after an out-of-credit primary rejection")

	// Breaker now open: the next request skips the primary entirely.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5-20251001","stream":true}`))
	p.ServeHTTP(rec2, req2)
	c.Eq(1, primaryCalls, "primary calls")
	c.Eq(2, orCalls, "openrouter calls")
}

// An ordinary 400 is neither retryable nor failover-worthy: it is the caller's
// own bad request, so it must be surfaced verbatim rather than re-sent to a
// second provider that would reject it identically.
func TestMessagesProxyOrdinaryBadRequestDoesNotFailOver(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Eq(http.StatusBadRequest, rec.Code, "status")
	// The body must survive the classification peek intact.
	c.Eq(badReq, rec.Body.String(), "client body")
	c.False(breaker.Open(), "breaker must stay closed on a caller-caused 400")
}

func TestMessagesProxySlashRoutesDirectToOpenRouter(t *testing.T) {
	c := assert.NewCollecting(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("primary must not be called for slash model")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer primary.Close()
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenRouter authenticates via Authorization: Bearer, not x-api-key.
		c.Eq("Bearer or-key", r.Header.Get("Authorization"), "OR auth")
		c.Eq("", r.Header.Get("x-api-key"), "OR should not receive x-api-key, got")
		orBody, _ := io.ReadAll(r.Body)
		c.Eq("openai/gpt-4o", modelOf(t, orBody), "OR received model")
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

	c.StrContains(rec.Body.String(), "from-openrouter", "client did not get the OR response")
	c.Eq("openrouter", fs.lastUpstream, "captured upstream")
}

// A pinned model line (routing provider pins) gets its provider preferences
// injected into the OpenRouter body; a caller-supplied provider object wins.
func TestMessagesProxyPinsProviderForPinnedModel(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().Len(orBodies, 3, "OpenRouter received %d requests, want 3", len(orBodies))

	providerOf := func(body []byte) map[string]any {
		t.Helper()
		var payload map[string]any
		c.Require().NoError(json.Unmarshal(body, &payload), "unmarshal OR body")
		prov, _ := payload["provider"].(map[string]any)
		return prov
	}
	if prov := providerOf(orBodies[0]); fmt.Sprintf("%v", prov["only"]) != "[fireworks]" {
		t.Errorf("pinned model provider = %v, want only=[fireworks]", prov)
	}
	if prov := providerOf(orBodies[1]); fmt.Sprintf("%v", prov["only"]) != "[baseten]" {
		t.Errorf("caller-supplied provider must win, got %v", prov)
	}
	c.Nil(providerOf(orBodies[2]), "unpinned model must carry no provider, got")
}

// TestDoOpenRouterMergesIgnoreList proves an ejected provider reaches the wire
// as provider.ignore, AND that it survives a caller-supplied provider object.
// The union is deliberate: a budget guard any caller can switch off by sending
// its own provider block is not a guard. Contrast with
// TestMessagesProxyPinsProviderForPinnedModel, where a caller-supplied
// provider legitimately overrides a pin — the guard's ignore list is not a
// pin and must not be overridable the same way.
func TestDoOpenRouterMergesIgnoreList(t *testing.T) {
	g := routing.NewProviderGuard(routing.DefaultEjectTTL, slog.New(slog.DiscardHandler))
	now := time.Now()
	for range 6 {
		g.Observe(now, routing.Observation{
			Provider: "CoreWeave", Model: "deepseek/deepseek-v4-pro", Conversation: "c1",
			PrefixHash: "h1", InputTokens: 50000, CacheReadTokens: 0,
		})
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"no caller provider block", `{"model":"deepseek/deepseek-v4-pro","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"caller supplied provider block", `{"model":"deepseek/deepseek-v4-pro","provider":{"only":["novita"]},"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			var got map[string]any
			orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"type":"message","model":"deepseek/deepseek-v4-pro","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"provider":"Novita"}`)
			}))
			defer orSrv.Close()

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			p := NewMessagesProxy(nil, nil, "real-key", "http://unused-primary", "" /*defaultModel*/, nil /*catalog*/, logger)
			p.store = &fakeProxyStore{}
			p.SetFallback("or-key", orSrv.URL, routing.NewBreaker(15*time.Minute))
			p.SetProviderGuard(g)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer client-token")
			p.ServeHTTP(rec, req)

			prov, _ := got["provider"].(map[string]any)
			ignore, _ := prov["ignore"].([]any)
			c.False(len(ignore) != 1 || ignore[0] != "coreweave", "provider.ignore = %v, want [coreweave]; full provider block = %v", ignore, prov)
			if strings.Contains(tc.name, "caller supplied") {
				only, _ := prov["only"].([]any)
				c.False(len(only) != 1 || only[0] != "novita", "caller-supplied provider fields must survive the merge, got %v", prov)
			}
		})
	}
}

// A slash model on a proxy with no OpenRouter key returns 502 AND must resolve
// the turn it began, or the row is stranded 'pending' forever (a false orphan).
func TestMessagesProxySlashWithoutOpenRouterFailsTurn(t *testing.T) {
	c := assert.NewCollecting(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", "http://unused", "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs // capturing, but no setFallback -> orKey == ""

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"openai/gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	p.ServeHTTP(rec, req)

	c.Eq(http.StatusBadGateway, rec.Code, "status")
	c.StrContains(rec.Body.String(), "openai/gpt-4o", "body should name the model, got")
	if fs.intents != 1 || fs.fails != 1 || fs.completes != 0 {
		t.Errorf("intents=%d fails=%d completes=%d, want 1/1/0 (turn must be failed, not stranded)", fs.intents, fs.fails, fs.completes)
	}
}

func TestProxyResolveModel(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "empty->default errored")
	c.Eq("claude-haiku-4-5", modelOf(t, body), "empty->default")
	c.Eq("claude-haiku-4-5", resolved, "empty->default resolved")
	// explicit sonnet-latest -> concrete
	body, resolved, err = p.resolveAndAdapt([]byte(`{"model":"sonnet-latest","max_tokens":1}`))
	c.Require().NoError(err, "sonnet-latest errored")
	c.Eq("claude-sonnet-5", modelOf(t, body), "sonnet-latest")
	c.Eq("claude-sonnet-5", resolved, "sonnet-latest resolved")
	// concrete id -> untouched
	body, resolved, err = p.resolveAndAdapt([]byte(`{"model":"claude-opus-4-8"}`))
	c.Require().NoError(err, "concrete errored")
	c.Eq("claude-opus-4-8", modelOf(t, body), "concrete")
	c.Eq("claude-opus-4-8", resolved, "concrete resolved")
	// short model alias -> the line's newest OR slash id (selectUpstream then
	// routes the slash id to OpenRouter, per the slash-routing tests above)
	body, resolved, err = p.resolveAndAdapt([]byte(`{"model":"kimi-k3","max_tokens":1}`))
	c.Require().NoError(err, "kimi-k3 errored")
	c.Eq("moonshotai/kimi-k3", modelOf(t, body), "kimi-k3")
	c.Eq("moonshotai/kimi-k3", resolved, "kimi-k3 resolved")
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
	c.False(err != nil || resolved != "" || string(body) != string(garbage), "malformed body = (%q,%q,%v), want passthrough+empty+nil", body, resolved, err)
}

// End-to-end: a real proxied turn decomposes into conversation_message rows
// (user + assistant) and leaves conversation_turn.request NULL — the bulky
// full-request JSONB is no longer written now that decomposition covers it.
func TestProxy_DecomposesConversation(t *testing.T) {
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
	name := fmt.Sprintf("rafiki_decompose_%d", time.Now().UnixNano())
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

	cs := capture.NewCaptureStore(pool)
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
	c.Require().Eq(http.StatusOK, rec.Code, "status = %d, body=%s", rec.Code, rec.Body.String())

	// user message + assistant response decomposed; turn.request left NULL
	var msgs int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message`).Scan(&msgs))
	c.Eq(2, msgs, "conversation_message count")
	var reqNull bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT request IS NULL FROM conversations.conversation_turn LIMIT 1`).Scan(&reqNull))
	c.True(reqNull, "conversation_turn.request should be NULL; decomposition replaces the full-JSONB write")
}

func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	var m struct {
		Model string `json:"model"`
	}
	assert.NewAborting(t).NoError(json.Unmarshal(body, &m), "unmarshal")
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
			c := assert.NewCollecting(t)
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

			c.Require().Eq(http.StatusOK, rec.Code, "status = %d, body=%s", rec.Code, rec.Body.String())
			if tc.wantEffort == "" {
				c.False(sawOutputConfig && gotEffort != "", "effort should be stripped, upstream saw %q", gotEffort)
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
	assert.NewCollecting(t).EqDiff(want, efforts, "upstream efforts")
}

// ConversationTokens satisfies proxyStore. Returns a fixed two-model rollup so
// the cost-logging path is exercised with a conversation that changed models
// mid-flight — the case a flat SUM would price wrong.
func (s *fakeProxyStore) ConversationTokens(context.Context, string) ([]capture.ModelTokens, error) {
	return s.convTokens, nil
}

// TestCostFieldsPricesTurnAndConversation proves the log line carries both
// numbers, that the running total includes the turn being logged (which is not
// yet in the store when this runs), and that a conversation spanning two models
// prices each at its own rate rather than billing everything at one.
func TestCostFieldsPricesTurnAndConversation(t *testing.T) {
	cat := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-sonnet-5", Created: 1, Pricing: &routing.ModelPricing{
			PromptUSD: 0.000003, CompletionUSD: 0.000015, CacheReadUSD: 0.0000003, CacheWriteUSD: 0.00000375,
		}},
		{ID: "deepseek/deepseek-v4-pro", Created: 2, Pricing: &routing.ModelPricing{
			PromptUSD: 0.000001, CompletionUSD: 0.000002,
		}},
	})
	fs := &fakeProxyStore{convTokens: []capture.ModelTokens{
		{Model: "claude-sonnet-5", InputTokens: 1_000_000},           // $3.00
		{Model: "deepseek/deepseek-v4-pro", OutputTokens: 1_000_000}, // $2.00
	}}
	p := &MessagesProxy{store: fs, catalog: cat, logger: slog.New(slog.DiscardHandler)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	// This turn: 1M output on sonnet = $15.00. Total = 15 + 3 + 2 = $20.00.
	got := p.costFields(req, "conv-1", "claude-sonnet-5", routing.CapturedUsage{OutputTokens: 1_000_000})
	want := []any{"cost_turn", "15.000000", "cost_total", "20.00"}
	assert.NewCollecting(t).EqDeep(want, got, "costFields")
}

// TestCostFieldsUnpricedModelIsSilent proves an unpriced model logs no cost at
// all. A confident "cost_turn=0.000000" would read as "this turn was free",
// which is a worse answer than saying nothing.
func TestCostFieldsUnpricedModelIsSilent(t *testing.T) {
	cat := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	cat.SeedForTest([]routing.CatalogEntry{{ID: "anthropic/claude-sonnet-5", Created: 1}})
	p := &MessagesProxy{store: &fakeProxyStore{}, catalog: cat, logger: slog.New(slog.DiscardHandler)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	assert.NewCollecting(t).Nil(p.costFields(req, "conv-1", "some/unknown-model", routing.CapturedUsage{OutputTokens: 5}), "costFields for an unpriced model")
}

// TestMessagesProxyCompactionRebases drives the REAL MessagesProxy over
// ServeHTTP against the REAL (Postgres-backed) *capture.CaptureStore — not the
// fakeProxyStore pattern the fixtures above use — because its whole point is
// the wiring: the proxy consuming DecomposeRequest's horizon-aware return
// value when Claude Code really drives it. pkg/capture's own tests cover
// DecomposeRequest's mechanics directly; this one covers the end-to-end path
// ServeHTTP → streamAndCapture → DecomposeRequest → AppendResponseMessage for
// a post-compaction request whose message 0 differs from the stored anchor.
//
// The pre-fix behavior being guarded against: the proxy used to append the
// response at captureRef.nextOrdinal (the request's message count, computed
// I/O-free in beginCapture). For a two-message post-compaction request that
// was ordinal 2 — colliding with the compaction_summary boundary row and
// putting the response before the horizon instead of after it.
func TestMessagesProxyCompactionRebases(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Require().Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.Require().NoError(err, "connect")
	t.Cleanup(pool.Close)

	c.Require().NoError(store.Migrate(ctx, pool), "Migrate")

	// Fake Anthropic upstream: the same minimal SSE stream
	// TestMessagesProxyStreamsAndCaptures uses — message_start carries input
	// usage, message_delta the stop reason and cumulative output tokens.
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
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = capture.NewCaptureStore(pool) // the REAL store, unlike the fakeProxyStore fixtures

	// Unique per run: EnsureConversationByExternalRef resolves conversations by
	// (external_ref, driven_by), so a fixed string would collide across runs
	// against the shared test DB.
	session := "compaction-test-" + time.Now().Format(time.RFC3339Nano)

	// Turn 1 — the pre-compaction conversation: "hello" lands at ordinal 0,
	// the turn's response at ordinal 1 (horizon 0 + 1 request message).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("X-Rafiki-Session", session)
	p.ServeHTTP(rec, req)
	c.Require().Eq(http.StatusOK, rec.Code, "first request: status = %d, body=%s", rec.Code, rec.Body.String())

	// Turn 2 — the post-compaction request, exactly how Claude Code drives the
	// proxy after its own context compaction: full history replaced by a
	// summary, re-sent under the same X-Rafiki-Session. Message 0 differs from
	// everything stored, so the anchor comparison diverges; the re-anchor guard
	// finds no positional rematch and the genuine boundary path runs. The
	// summary text is the prose Claude Code's real summaries open with: task
	// 5.1's classifier requires it, and the bare "[summary text]" fixture this
	// test carried before encoded the mis-tagging bug (any divergent message 0
	// was tagged a boundary, which is how 19 session preambles became
	// compaction_summary rows).
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[`+
			`{"role":"user","content":[{"type":"text","text":"This session is being continued from a previous conversation that ran out of context. The conversation is summarized below: Analysis: the user asked about the proxy"}]},`+
			`{"role":"user","content":"continue"}]}`))
	req2.Header.Set("X-Rafiki-Session", session)
	p.ServeHTTP(rec2, req2)
	c.Require().Eq(http.StatusOK, rec2.Code, "second request: status = %d, body=%s", rec2.Code, rec2.Body.String())

	// Resolve the conversation the session header created.
	var convID string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT id::text FROM conversations.conversation WHERE external_ref = $1`,
		session).Scan(&convID), "resolve conversation by external_ref %q", session)

	// A boundary was recorded: the horizon is non-zero.
	var horizon int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&horizon), "read resume_from_ordinal")
	c.Require().NotEq(0, horizon, "resume_from_ordinal = 0, want non-zero: the post-compaction request diverged from the anchor but no boundary was recorded")

	// Load through the same API the reattach display path uses, so the
	// assertions below are about what a reader of the conversation sees.
	msgs, err := store.NewMessages(pool).Load(ctx, convID)
	c.Require().NoError(err, "Load")
	c.Require().Len(msgs, 5, "Load returned %d messages, want 5 (ordinals 0..4 dense); ordinals seen: %v", len(msgs), func() []int {
		out := make([]int, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, m.Ordinal)
		}
		return out
	}())
	byOrdinal := make(map[int]store.Message, len(msgs))
	for _, m := range msgs {
		byOrdinal[m.Ordinal] = m
	}

	// The ORIGINAL first message is still present at its original ordinal with
	// its content intact — the exact regression design §5 exists to prevent
	// (Load serves GetHistory/reattach; it must never be horizon-filtered).
	// Checked two ways: the Load shape a reader sees, and the same JSONB
	// equality resolveHorizon's own anchor comparison uses.
	if m0 := byOrdinal[0]; m0.Param.Role != "user" {
		t.Errorf("ordinal 0 role = %q, want user", m0.Param.Role)
	} else if b, merr := json.Marshal(m0.Param.Content); merr != nil || !strings.Contains(string(b), "hello") {
		t.Errorf("ordinal 0 content = %s (marshal err %v), want the original %q message still present", b, merr, "hello")
	}
	var helloPresent bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conversations.conversation_message
		   WHERE conversation_id=$1::uuid AND ordinal=0 AND role='user' AND content='"hello"'::jsonb)`,
		convID).Scan(&helloPresent), "check original message survived")
	c.True(helloPresent, `original message "hello" no longer present at ordinal 0 — pre-compaction history was lost (design §5 regression)`)

	// The row at the horizon is the compaction summary boundary.
	mb := byOrdinal[horizon]
	c.False(mb.Kind == nil || *mb.Kind != "compaction_summary", "row at resume_from_ordinal %d: kind = %v, want compaction_summary", horizon, mb.Kind)
	c.NotNil(mb.InputTokens, "compaction_summary row at %d carries no input_tokens (the replaced-context size), want non-nil", horizon)

	// The second request's messages and response landed at horizon-relative
	// ordinals: continuation at horizon+1, response at horizon+2. The pre-fix
	// proxy would have appended the response at the request-message count (2),
	// colliding with the boundary row at the horizon.
	if mc := byOrdinal[horizon+1]; mc.Param.Role != "user" {
		t.Errorf("ordinal %d role = %q, want user (the continuation message)", horizon+1, mc.Param.Role)
	}
	if mr := byOrdinal[horizon+2]; mr.Param.Role != "assistant" {
		t.Errorf("ordinal %d role = %q, want assistant (the second request's response)", horizon+2, mr.Param.Role)
	}

	// Each turn recorded its response at the right ordinal — turn 1 at 1 (the
	// pre-compaction path unchanged), turn 2 at horizon+2. The turn row is the
	// direct witness that AppendResponseMessage consumed DecomposeRequest's
	// return value rather than a precomputed request-message count.
	var turnCount int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_turn WHERE conversation_id=$1`,
		convID).Scan(&turnCount), "count turns")
	c.Require().Eq(2, turnCount, "conversation has")
	rows, err := pool.Query(ctx,
		`SELECT coalesce(response_ordinal,-1) FROM conversations.conversation_turn
		  WHERE conversation_id=$1 ORDER BY created_at, id`, convID)
	c.Require().NoError(err, "read turn response ordinals")
	defer rows.Close()
	var respOrdinals []int
	for rows.Next() {
		var o int
		c.Require().NoError(rows.Scan(&o), "scan response_ordinal")
		respOrdinals = append(respOrdinals, o)
	}
	c.Require().NoError(rows.Err(), "read turn response ordinals")
	if len(respOrdinals) != 2 || respOrdinals[0] != 1 || respOrdinals[1] != horizon+2 {
		t.Errorf("turn response ordinals = %v, want [1 %d] (turn 1 pre-compaction; turn 2 horizon-relative, not the request-message count 2)", respOrdinals, horizon+2)
	}
}

type fakeRawTrace struct {
	mu       sync.Mutex
	inserted []rawtrace.RawHTTPRequest
}

func (f *fakeRawTrace) Insert(_ context.Context, r rawtrace.RawHTTPRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, r)
	return nil
}

func TestRawTraceRecordsAMalformedSuccess(t *testing.T) {
	c := assert.NewCollecting(t)
	// A 2xx wearing the wrong Content-Type is a gateway error page. It is
	// surfaced to the client as a 502 and, before this, recorded nowhere.
	rec := &fakeRawTrace{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>upstream is unwell</html>"))
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = &fakeProxyStore{} // fake makes the turn capturable; only the trace is asserted
	p.SetRawTrace(rec, true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	p.ServeHTTP(w, req)

	c.Require().Eq(http.StatusBadGateway, w.Code, "status")
	c.Require().Len(rec.inserted, 1, "raw traces recorded = %d, want 1", len(rec.inserted))
	c.StrContains(string(rec.inserted[0].RespBody), "upstream is unwell", "resp body")
}

func TestRawTraceRecordsATruncatedStream(t *testing.T) {
	c := assert.NewCollecting(t)
	// Upstream dies mid-stream: the partial body is the forensic value, and the
	// trace's Error should name the same read failure the turn reason does.
	rec := &fakeRawTrace{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close() // cut the stream before message_stop
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs
	p.SetRawTrace(rec, true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	p.ServeHTTP(w, req)

	// The bytes streamed before the cut were already forwarded, so the client
	// saw a 200 head; the turn must still be failed and the trace still stored.
	c.Require().Eq(http.StatusOK, w.Code, "status")
	if fs.fails != 1 || !strings.Contains(fs.lastFailMsg, "mid-stream read error") {
		t.Fatalf("fail msg = %q, want the mid-stream read error", fs.lastFailMsg)
	}
	c.Require().Len(rec.inserted, 1, "raw traces recorded = %d, want 1", len(rec.inserted))
	c.StrContains(rec.inserted[0].Error, "mid-stream read error", "trace error")
	c.StrContains(string(rec.inserted[0].RespBody), "message_start", "resp body")
}

func TestRawTraceRecordsACaptureParseFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	// A stream the parser cannot reassemble fails the turn; the stored body
	// prefix is the only forensic record of what the upstream actually sent.
	rec := &fakeRawTrace{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "gateway says: nothing resembling an SSE stream here")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs
	p.SetRawTrace(rec, true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	p.ServeHTTP(w, req)

	c.Require().Eq(http.StatusOK, w.Code, "status")
	if fs.fails != 1 || !strings.Contains(fs.lastFailMsg, "capture parse failed") {
		t.Fatalf("fail msg = %q, want the capture parse failure", fs.lastFailMsg)
	}
	c.Require().Len(rec.inserted, 1, "raw traces recorded = %d, want 1", len(rec.inserted))
	c.StrContains(rec.inserted[0].Error, "capture parse failed", "trace error")
	c.StrContains(string(rec.inserted[0].RespBody), "nothing resembling an SSE stream", "resp body")
}

func TestSubagentTurnsAreAttributedToAnAgent(t *testing.T) {
	// 1782 subagent turns in the investigated database are recorded
	// author_kind='human'. Claude Code states which it is in every request.
	for _, tc := range []struct {
		name       string
		systemText string
		wantKind   string
		wantSource string
	}{
		{
			name:       "main thread",
			systemText: "x-anthropic-billing-header: cc_version=2.1.259.b07; cc_entrypoint=cli; cch=42c51;",
			wantKind:   "human",
			wantSource: "claude",
		},
		{
			name:       "task subagent",
			systemText: "x-anthropic-billing-header: cc_version=2.1.267.019; cc_entrypoint=sdk-cli; cch=6d579; cc_is_subagent=true;",
			wantKind:   "agent",
			wantSource: "claude-subagent",
		},
		{
			name:       "not claude code at all",
			systemText: "You are a helpful assistant.",
			wantKind:   "human",
			wantSource: "claude",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			body := mustMarshal(t, map[string]any{
				"model":    "claude-sonnet-5",
				"system":   []any{map[string]any{"type": "text", "text": tc.systemText}},
				"messages": []any{},
			})
			kind, source := authorAttribution(body, "claude")
			c.Eq(tc.wantKind, kind, "author_kind")
			c.Eq(tc.wantSource, source, "source")
		})
	}
}

func TestAuthorAttributionKeepsANonClaudeSourceIntact(t *testing.T) {
	// X-Rafiki-Source is how one proxy serves a TUI, a slack bot and diagnose.
	// A subagent suffix must never overwrite an entrypoint it did not set.
	body := mustMarshal(t, map[string]any{"messages": []any{}})
	_, source := authorAttribution(body, "slack")
	assert.NewCollecting(t).Eq("slack", source, "source")
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	assert.NewAborting(t).NoError(err, "marshal")
	return b
}

// TestProxyRecordsTheThreadAfterAppendingTheResponse pins the one wiring call
// that makes thread observation live: without it, RecordThread exists and its
// store tests pass, and real traffic records nothing, erroring nowhere.
func TestProxyRecordsTheThreadAfterAppendingTheResponse(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_own","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[],
			"diagnostics":{"previous_message_id":"msg_prev"}}`))
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Require().Eq(1, fs.threads, "RecordThread calls")
	c.Eq("msg_prev", fs.lastPrevMsg, "prevMessageID")
	c.Eq("msg_own", fs.lastOwnMsg, "ownMessageID")
	c.Eq("", fs.lastSession, "session")
	c.False(fs.lastIsSubagent, "isSubagent = true, want false (no billing header on the request)")
}

// TestProxyClassifiesTheThreadFromTheBillingHeader pins the other half of the
// wiring: the X-Rafiki-Session value and the parsed cc_is_subagent flag reach
// RecordThread, because the routing decision depends on both and a proxy that
// drops either one silently routes every turn to the main thread.
func TestProxyClassifiesTheThreadFromTheBillingHeader(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_own","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[],
			"system":[{"type":"text","text":"x-anthropic-billing-header:cc_version=2.1.267.019;cc_entrypoint=sdk-cli;cc_is_subagent=true;cc_prev_req=req_1;cc_prompt_id=p_1"}],
			"diagnostics":{"previous_message_id":"msg_prev"}}`))
	req.Header.Set("X-Rafiki-Session", "c_wiring_test")
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Require().Eq(1, fs.threads, "RecordThread calls")
	c.Eq("c_wiring_test", fs.lastSession, "session")
	c.True(fs.lastIsSubagent, "isSubagent = false, want true (cc_is_subagent=true in the billing header)")
}

// TestProxyRoutesTheTurnIntentToTheResolvedThreadRow pins the beginCapture
// seam end to end: the threadID the lookup answers is what selects the
// conversation row the turn intent lands on, and the ref it resolves carries
// the thread suffix. Without this pin, reverting beginCapture to a bare
// EnsureConversationByExternalRef (the pre-3.1 single-row behavior) passes
// the suite silently.
func TestProxyRoutesTheTurnIntentToTheResolvedThreadRow(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_own","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{threadID: "thread-b"}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[],
			"diagnostics":{"previous_message_id":"msg_prev"}}`))
	req.Header.Set("X-Rafiki-Session", "c_route_test")
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Eq("thread-b", fs.lastThreadID, "ResolveThreadConversation threadID")
	c.Eq("c_route_test:thread-b", fs.lastResolvedRef, "resolved external_ref")
	c.Eq("conv-branch", fs.lastIntentConv, "turn intent landed on conversation")
}

// TestRecordThreadFailureIsLoggedAndSwallowed: thread observation must never be
// able to fail a turn that otherwise succeeded, so its own write failure is
// logged and dropped rather than routed through failTurn.
func TestRecordThreadFailureIsLoggedAndSwallowed(t *testing.T) {
	c := assert.NewAborting(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_own","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{threadErr: errors.New("record thread: update: boom")}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[],
			"diagnostics":{"previous_message_id":"msg_prev"}}`))
	p.ServeHTTP(rec, req)

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Eq(0, fs.fails, "FailTurn calls")
}

// stubThreadObserver records what beginCapture told it, so the synthetic
// child-record hook is pinned end to end.
type stubThreadObserver struct {
	calls []string // "parent|thread|conversation" per call
	err   error
}

func (s *stubThreadObserver) EnsureThreadChild(parentChildID, threadID, conversationID string) error {
	s.calls = append(s.calls, parentChildID+"|"+threadID+"|"+conversationID)
	return s.err
}

// TestThreadObserverIsToldAboutNonRootThreads pins the beginCapture hook that
// materializes a child record per captured Claude Code thread. If it silently
// stopped happening, native subagents would fall out of lineage, the rail,
// rafiki list and the cost rollup with nothing erroring anywhere.
//
// Every request here declares a client tool, because the hook fires only for a
// thread that can ACT (TestToolLessFounderForksWithoutASyntheticChild covers
// the other side). The tool is what makes these subagents rather than Claude
// Code's per-tool-call helpers.
func TestThreadObserverIsToldAboutNonRootThreads(t *testing.T) {
	const bodyWithTool = `{"model":"claude","stream":true,
		"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`

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

	t.Run("non-root thread", func(t *testing.T) {
		c := assert.NewCollecting(t)
		obs := &stubThreadObserver{}
		fs := &fakeProxyStore{threadID: "thread-9"}
		p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
		p.store = fs
		p.SetThreadObserver(obs)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyWithTool))
		req.Header.Set("X-Rafiki-Session", "c_parent")
		p.ServeHTTP(rec, req)

		c.Require().Eq(http.StatusOK, rec.Code, "status")
		c.Require().Len(obs.calls, 1, "observer calls = %d, want 1", len(obs.calls))
		c.Eq("c_parent|thread-9|conv-branch", obs.calls[0], "call")
	})

	t.Run("root thread is never observed", func(t *testing.T) {
		obs := &stubThreadObserver{}
		fs := &fakeProxyStore{} // threadID empty: the root thread
		p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
		p.store = fs
		p.SetThreadObserver(obs)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyWithTool))
		req.Header.Set("X-Rafiki-Session", "c_root")
		p.ServeHTTP(rec, req)

		assert.NewCollecting(t).Empty(obs.calls, "observer calls")
	})

	t.Run("observer failure does not fail the turn", func(t *testing.T) {
		c := assert.NewCollecting(t)
		obs := &stubThreadObserver{err: errors.New("parent not found")}
		fs := &fakeProxyStore{threadID: "thread-9"}
		p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
		p.store = fs
		p.SetThreadObserver(obs)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyWithTool))
		req.Header.Set("X-Rafiki-Session", "c_parent")
		p.ServeHTTP(rec, req)

		c.Require().Eq(http.StatusOK, rec.Code, "status")
		c.Eq(0, fs.fails, "FailTurn calls")
	})
}

// newStreamUpstream returns an Anthropic-shaped SSE upstream answering one
// assistant message with the given id, the shape every thread-routing test
// below replays.
func newStreamUpstream(msgID string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"`+msgID+`","usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n")
	}))
}

// TestMainThreadTurn1OnAFreshSessionKeepsTheBareRef pins routing case 2: a
// request with no resolvable predecessor on a session whose family does not
// exist yet is the session's MAIN founding turn. It keeps the bare session
// external_ref, mints nothing, and never notifies the observer (the root
// thread is already a real child).
func TestMainThreadTurn1OnAFreshSessionKeepsTheBareRef(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := newStreamUpstream("msg_own")
	defer upstream.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	obs := &stubThreadObserver{}
	fs := &fakeProxyStore{familyExists: false}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs
	p.SetThreadObserver(obs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "c_fresh_main")
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Eq("", fs.lastThreadID, "ResolveThreadConversation threadID = %q, want \"\" (main turn 1 keeps the bare ref)", fs.lastThreadID)
	c.Eq("c_fresh_main", fs.lastResolvedRef, "resolved external_ref")
	c.Eq("", fs.lastIntentID, "TurnIntent.ID")
	c.Empty(obs.calls, "observer calls")
}

// TestUnflaggedFoundingOnAnExistingFamilyStaysMainThread pins the
// discriminator's conservative arm: a request with no resolvable predecessor
// on an existing family that does NOT carry cc_is_subagent is a MAIN-thread
// turn, not an independent founder. This is Claude Code's post-compaction
// shape (history replaced by a summary, previous_message_id gone); routing it
// to a branch would strand every later main turn off the root row.
func TestUnflaggedFoundingOnAnExistingFamilyStaysMainThread(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := newStreamUpstream("msg_own")
	defer upstream.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	obs := &stubThreadObserver{}
	fs := &fakeProxyStore{familyExists: true} // family exists, flag absent
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs
	p.SetThreadObserver(obs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req.Header.Set("X-Rafiki-Session", "c_compacted")
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Eq("", fs.lastThreadID, "ResolveThreadConversation threadID")
	c.Eq("c_compacted", fs.lastResolvedRef, "resolved external_ref")
	c.Eq("", fs.lastIntentID, "TurnIntent.ID")
	c.Empty(obs.calls, "observer calls")
}

// TestIndependentFoundingRequestPreMintsItsTurnAndCallsTheObserver pins
// routing case 3, the reviewer's probe scenario: a request with no resolvable
// predecessor on a session whose family ALREADY exists is an independent
// thread's founding request (a Task subagent). The proxy mints the founding
// turn's id in Go, routes the request to the branch named after it from its
// first request, hands that id to InsertTurnIntent, and tells the observer
// HERE, so a single-turn subagent's synthetic child materializes on the
// founding request itself. Landed on the root row instead, the founding
// request's small message count collides with ordinals the main thread
// already occupies, the strict response append fails, and (before the
// pre-mint) the lost response destroyed the thread identity with it.
//
// The tool declaration is load-bearing, not scenery: the observer is called
// only for a founder that can ACT — see
// TestToolLessFounderForksWithoutASyntheticChild.
func TestIndependentFoundingRequestPreMintsItsTurnAndCallsTheObserver(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := newStreamUpstream("msg_own")
	defer upstream.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	obs := &stubThreadObserver{}
	fs := &fakeProxyStore{familyExists: true}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs
	p.SetThreadObserver(obs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true,
		"tools":[{"name":"Bash","input_schema":{"type":"object"}}],
		"system":[{"type":"text","text":"x-anthropic-billing-header:cc_version=2.1.267.019;cc_entrypoint=sdk-cli;cc_is_subagent=true"}]}`))
	req.Header.Set("X-Rafiki-Session", "c_found")
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	_, err := uuid.Parse(fs.lastIntentID)
	c.Require().NoError(err, "InsertTurnIntent received TurnIntent.ID = %q, want a pre-minted UUID", fs.lastIntentID)
	c.Eq("c_found:"+fs.lastIntentID, fs.lastResolvedRef, "resolved external_ref")
	c.Eq("conv-branch", fs.lastIntentConv, "turn intent landed on conversation")
	if len(obs.calls) != 1 || obs.calls[0] != "c_found|"+fs.lastIntentID+"|conv-branch" {
		t.Errorf("observer calls = %v, want [%s] on the founding request itself (single-turn subagents must still appear)", obs.calls, "c_found|"+fs.lastIntentID+"|conv-branch")
	}
}

// TestToolLessFounderForksWithoutASyntheticChild pins the two halves of the
// WebFetch/WebSearch fix, which pull in opposite directions.
//
// cc_is_subagent means "not the main thread", not "Task subagent": Claude Code
// stamps it on the haiku one-shots it fires to summarize a fetched page and to
// drive a web search. Those still have to FORK — a 2-message request landed on
// the root collides with ordinals the main thread occupies and the strict
// response append rejects it — but they are not agents and must not appear in
// the rail. Without the gate one measured session grew 299 single-turn
// synthetic children against 10 real subagents.
//
// The WebSearch shape is the sharp case: it DOES declare a tool, but a
// server-executed one with no input_schema, so "tools is non-empty" is the
// wrong test and "has a client tool" is the right one.
func TestToolLessFounderForksWithoutASyntheticChild(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
	}{
		{"webfetch summarizer declares no tools at all", `[]`},
		{"websearch helper declares only a server tool",
			`[{"name":"web_search","type":"web_search_20250305","max_uses":8}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			upstream := newStreamUpstream("msg_own")
			defer upstream.Close()
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			obs := &stubThreadObserver{}
			fs := &fakeProxyStore{familyExists: true}
			p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
			p.store = fs
			p.SetThreadObserver(obs)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
				`{"model":"claude-haiku-4-5-20251001","stream":true,"tools":`+tc.tools+`,
				"system":[{"type":"text","text":"x-anthropic-billing-header:cc_version=2.1.268.a5d;cc_entrypoint=sdk-cli;cc_is_subagent=true"}]}`))
			req.Header.Set("X-Rafiki-Session", "c_helper")
			p.ServeHTTP(rec, req)

			c.Require().Eq(http.StatusOK, rec.Code, "status")
			_, err := uuid.Parse(fs.lastIntentID)
			c.Require().NoError(err, "TurnIntent.ID = %q, want a pre-minted UUID: the helper still forks", fs.lastIntentID)
			c.Eq("c_helper:"+fs.lastIntentID, fs.lastResolvedRef, "resolved external_ref")
			c.Empty(obs.calls, "observer calls")
		})
	}
}

// TestRecordThreadRunsBeforeAFailedResponseAppend pins the insurance reorder:
// a capture-write failure after a successful stream must not destroy the
// thread identity. RecordThread runs before the append-error early return, so
// even a founding turn whose response could not be appended is stamped and
// the thread's next turn resolves to the branch instead of re-founding.
func TestRecordThreadRunsBeforeAFailedResponseAppend(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := newStreamUpstream("msg_own")
	defer upstream.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fs := &fakeProxyStore{
		appendErr: fmt.Errorf("%w: conversation c ordinal 5", capture.ErrOrdinalOccupied),
	}
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[],
			"diagnostics":{"previous_message_id":"msg_prev"}}`))
	p.ServeHTTP(rec, req)

	c.Require().NotEq(0, fs.fails, "FailTurn calls = 0, want 1 (the append failure is still loud)")
	c.Eq(1, fs.threads, "RecordThread calls")
}

// TestRawTraceCapturesRealHeaders pins the actual bug: upstreamReqHeaders and
// upstreamRespHeaders used to hand-pick a tiny allowlist (Content-Type,
// anthropic-version, anthropic-beta for the request; Content-Type,
// x-request-id for the response) and silently dropped everything else the
// upstream actually sent or received. A raw_http_request row should instead
// show the real credential REDACTED (present, not dropped) and any upstream
// response header at all, neither of which the old allowlist could produce.
func TestRawTraceCapturesRealHeaders(t *testing.T) {
	c := assert.NewCollecting(t)
	rec := &fakeRawTrace{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.Eq("real-key", r.Header.Get("x-api-key"), "upstream saw x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "42")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = &fakeProxyStore{}
	p.SetRawTrace(rec, true)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	p.ServeHTTP(w, req)

	c.Require().Eq(http.StatusOK, w.Code, "status")
	c.Require().Len(rec.inserted, 1, "raw traces recorded = %d, want 1", len(rec.inserted))

	var reqHeaders map[string]string
	if err := json.Unmarshal(rec.inserted[0].ReqHeaders, &reqHeaders); err != nil {
		t.Fatalf("ReqHeaders not valid JSON: %v (%s)", err, rec.inserted[0].ReqHeaders)
	}
	c.Eq("<redacted>", reqHeaders["X-Api-Key"], "req X-Api-Key")

	var respHeaders map[string]string
	err := json.Unmarshal(rec.inserted[0].RespHeaders, &respHeaders)
	c.Require().NoError(err, "RespHeaders not valid JSON: %v (%s)", err, rec.inserted[0].RespHeaders)
	c.Eq("42", respHeaders["Anthropic-Ratelimit-Requests-Remaining"], "resp Anthropic-Ratelimit-Requests-Remaining")
}
