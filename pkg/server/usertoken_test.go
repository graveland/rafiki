package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/users"
)

type stubStore struct {
	users.Store
	calls  atomic.Int64
	tokens map[string]users.Identity
	err    error
}

func (s *stubStore) Authenticate(_ context.Context, token string) (users.Identity, error) {
	s.calls.Add(1)
	if s.err != nil {
		return users.Identity{}, s.err
	}
	id, ok := s.tokens[token]
	if !ok {
		return users.Identity{}, users.ErrNotFound
	}
	return id, nil
}

// newTestUserAuth builds a UserTokenAuth over an in-memory store, for porting
// tests that predate the users-table store. tokenToUsername maps token value
// -> username (the inverse of StaticTokenAuth's old name->token map).
func newTestUserAuth(tokenToUsername map[string]string) *UserTokenAuth {
	tokens := make(map[string]users.Identity, len(tokenToUsername))
	for token, name := range tokenToUsername {
		tokens[token] = users.Identity{Username: name}
	}
	st := &stubStore{tokens: tokens}
	return NewUserTokenAuth(st, "unused-child-secret-for-ported-tests", time.Minute)
}

func serve(a *UserTokenAuth, req *http.Request) (*httptest.ResponseRecorder, *Identity) {
	var got *Identity
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = IdentityFromContext(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got
}

func TestUserTokenAuthResolvesIdentity(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", time.Second)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rfk_good")
	rec, id := serve(a, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if id == nil || id.UserID != "u1" || id.Username != "brent" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestUnknownTokenIs401(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{}}
	a := NewUserTokenAuth(st, "childsecret", time.Second)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rfk_nope")
	rec, _ := serve(a, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The distinction that matters: a client told 401 discards its token and
// re-prompts. A database outage must not do that to every user at once.
func TestStoreOutageIs503Not401(t *testing.T) {
	st := &stubStore{err: errors.New("connection refused")}
	a := NewUserTokenAuth(st, "childsecret", time.Second)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rfk_good")
	rec, _ := serve(a, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("store error text leaked to an unauthenticated caller: %q", rec.Body.String())
	}
}

// The per-boot child token is a daemon-internal credential, not a user. It
// works even in bootstrap mode, and never touches the store.
func TestChildTokenIsAcceptedWithoutTouchingTheStore(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{}}
	a := NewUserTokenAuth(st, "childsecret", time.Second)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer childsecret")
	rec, id := serve(a, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if id == nil || id.UserID != "" {
		t.Fatalf("child identity must not carry a UserID: %+v", id)
	}
	if st.calls.Load() != 0 {
		t.Fatalf("child token hit the store %d times, want 0", st.calls.Load())
	}
}

// This is the whole reason the digest scheme replaced bcrypt: the face
// authenticates PER REQUEST. Repeated calls must not be repeated queries.
func TestRepeatedRequestsHitTheStoreOnce(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	for i := 0; i < 20; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer rfk_good")
		if rec, _ := serve(a, req); rec.Code != 200 {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	if n := st.calls.Load(); n != 1 {
		t.Fatalf("store calls = %d, want 1 (the cache is not working)", n)
	}
}

func TestRevocationTakesEffectAfterTheTTL(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", 10*time.Millisecond)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rfk_good")
	if rec, _ := serve(a, req); rec.Code != 200 {
		t.Fatal("first request should succeed")
	}

	delete(st.tokens, "rfk_good") // `rafiki user rm`
	time.Sleep(20 * time.Millisecond)

	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	req2.Header.Set("Authorization", "Bearer rfk_good")
	if rec, _ := serve(a, req2); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still accepted after the TTL: %d", rec.Code)
	}
}

// A cache keyed by plaintext puts every live token in the daemon's heap.
func TestCacheIsKeyedByDigestNotPlaintext(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rfk_good")
	serve(a, req)

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, bad := a.cache["rfk_good"]; bad {
		t.Fatal("cache is keyed by the plaintext token")
	}
	if _, ok := a.cache[users.HashToken("rfk_good")]; !ok {
		t.Fatal("cache is not keyed by the digest")
	}
}

// Ported from the retired StaticTokenAuth's test coverage: the face must
// accept all three header shapes Anthropic-protocol clients use.
func TestHeaderVariantsAllAuthenticate(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	cases := []struct {
		name   string
		header func(r *http.Request)
	}{
		{"Authorization Bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer rfk_good") }},
		{"x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "rfk_good") }},
		{"X-Rafiki-Token with upstream Authorization", func(r *http.Request) {
			r.Header.Set("X-Rafiki-Token", "rfk_good")
			r.Header.Set("Authorization", "Bearer upstream-provider-key")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			tc.header(req)
			rec, id := serve(a, req)
			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if id == nil || id.UserID != "u1" {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

// Ported: X-Rafiki-Token without a real Authorization credential to forward
// must fail closed, not silently bill the daemon's own key upstream.
func TestPassthroughWithoutAuthorizationFailsClosed(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rfk_good")
	rec, _ := serve(a, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// Ported: a caller that puts its OWN rafiki token in the Authorization header
// too (instead of an upstream credential) must be rejected, not have that
// token relayed to a third party.
func TestPassthroughRejectsOwnTokenInAuthorization(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{"rfk_good": {UserID: "u1", Username: "brent"}}}
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Rafiki-Token", "rfk_good")
	req.Header.Set("Authorization", "Bearer rfk_good")
	rec, _ := serve(a, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// Ported: missing credentials entirely is 401, not a panic or 500.
func TestMissingCredentialsIs401(t *testing.T) {
	st := &stubStore{tokens: map[string]users.Identity{}}
	a := NewUserTokenAuth(st, "childsecret", time.Minute)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	rec, _ := serve(a, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A nil store is a supported configuration (RAFIKI_DB unset): every user
// token is unknown, but the child token must still work.
func TestNilStoreRejectsUserTokensButAcceptsChildToken(t *testing.T) {
	a := NewUserTokenAuth(nil, "childsecret", time.Minute)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer rfk_good")
	rec, _ := serve(a, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a user token against a nil store", rec.Code)
	}

	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	req2.Header.Set("Authorization", "Bearer childsecret")
	rec2, id := serve(a, req2)
	if rec2.Code != 200 {
		t.Fatalf("status = %d, want 200 for the child token against a nil store", rec2.Code)
	}
	if id == nil || id.UserID != "" {
		t.Fatalf("child identity must not carry a UserID: %+v", id)
	}
}
