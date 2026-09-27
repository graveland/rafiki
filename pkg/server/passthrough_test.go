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

	"github.com/multigres/testkit/assert"
)

// A caller that puts rafiki's token in X-Rafiki-Token is declaring that its
// Authorization header holds its own upstream credential.
func TestUserTokenAuth_XRafikiTokenMarksPassthrough(t *testing.T) {
	c := assert.NewCollecting(t)
	auth := newTestUserAuth(map[string]string{"rafiki-token": "cli"})
	var gotIdentity, gotCred string
	h := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if id := IdentityFromContext(r.Context()); id != nil {
			gotIdentity = id.Username
		}
		gotCred = PassthroughCredential(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Eq("cli", gotIdentity, "identity")
	c.Eq("Bearer sk-ant-oat01-client", gotCred, "passthrough credential")
}

// The ordinary path must be untouched: an Authorization-authenticated request
// has no passthrough credential, or every existing caller would start leaking
// its bearer upstream.
func TestUserTokenAuth_BearerIsNotPassthrough(t *testing.T) {
	c := assert.NewCollecting(t)
	auth := newTestUserAuth(map[string]string{"rafiki-token": "cli"})
	var gotCred string
	h := auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotCred = PassthroughCredential(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rafiki-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Eq("", gotCred, "passthrough credential")
}

// X-Rafiki-Token is a credential like any other: an unknown value is a 401,
// not an anonymous pass.
func TestUserTokenAuth_XRafikiTokenUnknownRejected(t *testing.T) {
	auth := newTestUserAuth(map[string]string{"rafiki-token": "cli"})
	h := auth.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached with an unknown token")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "not-a-real-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.NewCollecting(t).Eq(http.StatusUnauthorized, rec.Code, "status")
}

// X-Rafiki-Token declares "Authorization is mine"; arriving without one means
// the caller cannot self-bill after all. Serving it would charge the daemon's
// key silently — the precise outcome passthrough exists to avoid — so it fails
// closed. Reachable whenever Claude Code has no OAuth credential to send: the
// user is logged out, or CLAUDE_CODE_USE_BEDROCK/VERTEX is set.
func TestUserTokenAuth_XRafikiTokenWithoutAuthorizationIsRejected(t *testing.T) {
	auth := newTestUserAuth(map[string]string{"rafiki-token": "cli"})
	h := auth.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached; the request must be refused, not billed to the daemon")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.NewCollecting(t).Eq(http.StatusUnauthorized, rec.Code, "status")
}

// A client that puts rafiki's own token in BOTH headers must not have that
// token relayed to Anthropic: it is our secret, it buys nothing there, and the
// turn would die on an opaque upstream 401.
func TestUserTokenAuth_RefusesToForwardOwnToken(t *testing.T) {
	auth := newTestUserAuth(map[string]string{"rafiki-token": "cli"})
	h := auth.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached; rafiki's own token must never be forwarded upstream")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	req.Header.Set("Authorization", "Bearer rafiki-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.NewCollecting(t).Eq(http.StatusUnauthorized, rec.Code, "status")
}

// The whole point of the feature: the client's own credential reaches
// Anthropic and the daemon's key is not attached. Sending both Authorization
// and x-api-key is a 400 upstream, so the daemon key must be absent, not just
// unused.
func TestMessagesProxy_PassthroughForwardsClientCredential(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotAuth, gotAPIKey, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "daemon-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	h := newTestUserAuth(map[string]string{"rafiki-token": "cli"}).Middleware(p)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	c.Eq("Bearer sk-ant-oat01-client", gotAuth, "upstream Authorization")
	c.Eq("", gotAPIKey, "daemon key leaked as x-api-key")
	// Dropping oauth-2025-04-20 makes Anthropic reject an OAuth bearer.
	c.Eq("oauth-2025-04-20,claude-code-20250219", gotBeta, "anthropic-beta")
}

// The inverse of the above, pinning the pre-existing invariant: an ordinary
// request still bills the daemon key and never leaks the caller's bearer.
func TestMessagesProxy_OrdinaryRequestStillUsesDaemonKey(t *testing.T) {
	c := assert.NewCollecting(t)
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "daemon-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	h := newTestUserAuth(map[string]string{"rafiki-token": "cli"}).Middleware(p)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	req.Header.Set("Authorization", "Bearer rafiki-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	c.Eq("daemon-key", gotAPIKey, "upstream x-api-key")
	c.Eq("", gotAuth, "client Authorization leaked upstream")
}

// An OAuth subscription credential cannot buy an OpenRouter model. Failing
// over would silently bill the daemon's key instead, which is exactly what the
// user asked not to happen — so this is a clean 400, not a fallback.
func TestMessagesProxy_PassthroughRejectsSlashModel(t *testing.T) {
	c := assert.NewCollecting(t)
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "daemon-key", upstream.URL, "" /*defaultModel*/, nil /*catalog*/, logger)
	p.SetFallback("or-key", upstream.URL, nil)
	h := newTestUserAuth(map[string]string{"rafiki-token": "cli"}).Middleware(p)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"openai/gpt-4o","messages":[]}`))
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c.Eq(http.StatusBadRequest, rec.Code, "status")
	c.False(called, "upstream was called; the request must be refused before any forward")
}

// The OpenAI face is wrapped by the same auth middleware, so it can receive a
// passthrough credential it has no way to honour — it authenticates to each
// upstream with that upstream's own key. Silently serving the request would
// bill the daemon while the caller believed otherwise.
func TestChatCompletionsProxy_RejectsPassthrough(t *testing.T) {
	c := assert.NewCollecting(t)
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer upstream.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewChatCompletionsProxy(nil, nil,
		[]OpenAIUpstream{{Name: "u", BaseURL: upstream.URL, APIKey: "daemon-key"}},
		nil, "u", logger)
	h := newTestUserAuth(map[string]string{"rafiki-token": "cli"}).Middleware(p)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("X-Rafiki-Token", "rafiki-token")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	c.Eq(http.StatusBadRequest, rec.Code, "status")
	c.False(called, "upstream was called; the request must be refused before any forward")
}
