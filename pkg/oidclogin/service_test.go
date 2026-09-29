// SPDX-License-Identifier: Apache-2.0

package oidclogin

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/users"
)

// ─── fakes ──────────────────────────────────────────────────────────────────

// fakeResolver records the claims it was handed and replays a canned user or
// error — the answer/outage split the resolver interface encodes.
type fakeResolver struct {
	got  users.OIDCClaims
	user users.User
	err  error
}

func (f *fakeResolver) ResolveOIDC(_ context.Context, c users.OIDCClaims) (users.User, error) {
	f.got = c
	if f.err != nil {
		return users.User{}, f.err
	}
	return f.user, nil
}

// fakeMinter records the mint request and replays a canned token.
type fakeMinter struct {
	gotUserID string
	gotToken  users.NewToken
	token     users.Token
	secret    string
	err       error
}

func (f *fakeMinter) MintToken(_ context.Context, userID string, t users.NewToken) (users.Token, string, error) {
	f.gotUserID, f.gotToken = userID, t
	if f.err != nil {
		return users.Token{}, "", f.err
	}
	return f.token, f.secret, nil
}

// ─── fake issuer ────────────────────────────────────────────────────────────

const (
	fakeClientID   = "test-client"
	fakeSigningKID = "test-key"
)

// fakeIssuer is an in-process OIDC provider: a discovery document, a JWKS
// with one RSA key, and a token endpoint returning an RSA-signed ID token
// whose claims each test controls. It records what the token request carried
// (code, code_verifier, redirect_uri) so tests can assert the PKCE verifier
// and the redirect URI actually round-tripped.
type fakeIssuer struct {
	srv *httptest.Server
	url string

	mu         sync.Mutex
	key        *rsa.PrivateKey
	failNext   int // discovery GETs to answer 500 before serving the document
	discoveryN int // total discovery GETs seen
	claims     map[string]any

	// recorded from the most recent token request:
	lastCode, lastVerifier, lastRedirectURI string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{claims: map[string]any{}}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	f.key = key
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.serveDiscovery)
	mux.HandleFunc("/jwks", f.serveJWKS)
	mux.HandleFunc("/token", f.serveToken)
	f.srv = httptest.NewServer(mux)
	f.url = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.discoveryN++
	down := f.failNext > 0
	if down {
		f.failNext--
	}
	u := f.url
	f.mu.Unlock()
	if down {
		http.Error(w, "discovery down", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{
		"issuer": %q,
		"authorization_endpoint": %q,
		"token_endpoint": %q,
		"jwks_uri": %q,
		"id_token_signing_alg_values_supported": ["RS256"],
		"response_types_supported": ["code"]
	}`, u, u+"/authorize", u+"/token", u+"/jwks")
}

func (f *fakeIssuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	pub := f.key.Public().(*rsa.PublicKey)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w,
		`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`,
		fakeSigningKID,
		base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()))
}

func (f *fakeIssuer) serveToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.lastCode = r.PostFormValue("code")
	f.lastVerifier = r.PostFormValue("code_verifier")
	f.lastRedirectURI = r.PostFormValue("redirect_uri")
	claims := make(map[string]any, len(f.claims))
	for k, v := range f.claims {
		claims[k] = v
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w,
		`{"access_token":"fake-access","token_type":"Bearer","expires_in":3600,"id_token":%q,"scope":"openid email profile"}`,
		f.sign(claims))
}

// setClaims merges kv into the claims of the NEXT minted ID token.
func (f *fakeIssuer) setClaims(kv map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range kv {
		f.claims[k] = v
	}
}

// standardClaims is the baseline every test mutates: a valid token for the
// fake issuer, the test client, a subject, an email — no nonce, no
// email_verified until a test adds one.
func (f *fakeIssuer) standardClaims() map[string]any {
	return map[string]any{
		"iss":   f.url,
		"aud":   fakeClientID,
		"sub":   "sub-1",
		"email": "alice@graveland.dev",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
}

// resetClaims replaces the whole claim set.
func (f *fakeIssuer) resetClaims(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = claims
}

// sign returns an RS256-signed JWT over claims, exactly as a real IdP does.
func (f *fakeIssuer) sign(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	header := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"alg":"RS256","typ":"JWT","kid":%q}`, fakeSigningKID)))
	body := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := header + "." + body
	sum := sha256.Sum256([]byte(signingInput))
	f.mu.Lock()
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	f.mu.Unlock()
	if err != nil {
		panic(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// ─── harness ────────────────────────────────────────────────────────────────

// fixedNow is the deterministic clock every test service runs on, so token
// names and expiry windows are exact.
var fixedNow = time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

// newTestService builds a Service against iss with the standard config, a
// resolving fake resolver and a minting fake minter.
func newTestService(t *testing.T, iss *fakeIssuer, mutate func(*Config)) *Service {
	t.Helper()
	t.Setenv("RAFIKI_TEST_OIDC_SECRET", "sekrit")
	cfg := &Config{
		Issuer:          iss.url,
		ClientID:        fakeClientID,
		ClientSecretEnv: "RAFIKI_TEST_OIDC_SECRET",
		EmailDomains:    []string{"graveland.dev"},
	}
	if mutate != nil {
		mutate(cfg)
	}
	svc := New(cfg,
		&fakeResolver{user: users.User{ID: "u_1", Username: "alice"}},
		&fakeMinter{secret: "rfk_secret", token: users.Token{ID: "tok_1", Username: "alice"}})
	svc.now = func() time.Time { return fixedNow }
	return svc
}

// pendingSnapshot copies one pending entry out of the map. Exists only in
// tests: the nonce claim must be armed from the nonce Begin generated.
func (s *Service) pendingSnapshot(id string) *pendingLogin {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[id]
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// beginLogin drives Begin and returns the login id, the state the authorize
// URL carries, and the URL itself.
func beginLogin(t *testing.T, svc *Service, port uint32, host string) (loginID, state, authorizeURL string) {
	t.Helper()
	c := assert.NewAborting(t)
	resp, err := svc.Begin(context.Background(), port, host)
	c.NoError(err, "Begin")
	return resp.GetLoginId(), authorizeState(t, resp.GetAuthorizeUrl()), resp.GetAuthorizeUrl()
}

// authorizeState extracts the state query parameter from an authorize URL.
func authorizeState(t *testing.T, authorizeURL string) string {
	t.Helper()
	c := assert.NewAborting(t)
	u, err := url.Parse(authorizeURL)
	c.NoError(err, "parse authorize URL")
	return u.Query().Get("state")
}

// armNonce aims the issuer's nonce claim at the nonce Begin generated, so
// Complete's nonce check passes unless a test overrides it.
func armNonce(svc *Service, iss *fakeIssuer, loginID string) {
	iss.setClaims(map[string]any{"nonce": svc.pendingSnapshot(loginID).nonce})
}

// happyComplete runs a full Begin → Complete round trip and returns the
// Complete response. claims, when non-nil, mutates the issuer's claim set
// after the standard baseline is armed; the nonce is always aimed at the
// value Begin generated.
func happyComplete(t *testing.T, svc *Service, iss *fakeIssuer, claims func()) *rafikiv1.CompleteLoginResponse {
	t.Helper()
	iss.resetClaims(iss.standardClaims())
	if claims != nil {
		claims()
	}
	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	armNonce(svc, iss, loginID)
	resp, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

// ─── Begin ──────────────────────────────────────────────────────────────────

// TestBeginPinnedPortOverridesClient pins the response-redirect_port rule:
// the daemon's pinned port wins over the client's request port, the authorize
// URL bakes in the port ACTUALLY used, and an unpinned daemon uses the
// client's own port. The client rebinds to whatever the response reports.
func TestBeginPinnedPortOverridesClient(t *testing.T) {
	iss := newFakeIssuer(t)

	t.Run("pinned port wins", func(t *testing.T) {
		c := assert.NewAborting(t)
		svc := newTestService(t, iss, func(cfg *Config) { cfg.RedirectPort = 8765 })
		resp, err := svc.Begin(context.Background(), 12345, "laptop")
		c.NoError(err, "Begin")
		c.Eq(uint32(8765), resp.GetRedirectPort(), "response redirect_port")
		u, err := url.Parse(resp.GetAuthorizeUrl())
		c.NoError(err, "parse authorize URL")
		c.Eq("http://127.0.0.1:8765/oidc/callback", u.Query().Get("redirect_uri"),
			"authorize redirect_uri must use the pinned port")
	})

	t.Run("unpinned uses the client's port", func(t *testing.T) {
		c := assert.NewAborting(t)
		svc := newTestService(t, iss, nil)
		resp, err := svc.Begin(context.Background(), 12345, "laptop")
		c.NoError(err, "Begin")
		c.Eq(uint32(12345), resp.GetRedirectPort(), "response redirect_port")
		u, err := url.Parse(resp.GetAuthorizeUrl())
		c.NoError(err, "parse authorize URL")
		c.Eq("http://127.0.0.1:12345/oidc/callback", u.Query().Get("redirect_uri"),
			"authorize redirect_uri must use the client's port")
	})
}

// TestBeginRejectsPortZero pins the argument guard: port 0 and anything past
// 65535 are invalid — there is no loopback port 0 to redirect to.
func TestBeginRejectsPortZero(t *testing.T) {
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)
	c := assert.NewAborting(t)
	for _, port := range []uint32{0, 65536, 99999} {
		_, err := svc.Begin(context.Background(), port, "laptop")
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "port %d", port)
	}
}

// TestBeginBoundsPending pins the unauthenticated-surface cap: 1024 live
// logins are the limit, the 1025th Begin is refused, and expired entries are
// swept on every Begin so abandoned logins reclaim the budget.
func TestBeginBoundsPending(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	for i := 0; i < maxPendingLogins; i++ {
		if _, err := svc.Begin(context.Background(), 8080, "laptop"); err != nil {
			t.Fatalf("Begin %d: %v", i, err)
		}
	}
	_, err := svc.Begin(context.Background(), 8080, "laptop")
	c.Eq(connect.CodeResourceExhausted, connect.CodeOf(err), "the 1025th login")
	c.StrContains(err.Error(), "too many logins in progress", "cap message")

	// Advance past pendingTTL: the next Begin sweeps every expired entry and
	// the budget is reclaimed.
	svc.now = func() time.Time { return fixedNow.Add(pendingTTL + time.Minute) }
	resp, err := svc.Begin(context.Background(), 8080, "laptop")
	c.NoError(err, "Begin after the sweep")
	c.Eq(1, len(svc.pending), "only the fresh login survives the sweep")

	// The fresh entry is completable at the advanced clock.
	iss.resetClaims(iss.standardClaims())
	armNonce(svc, iss, resp.GetLoginId())
	state := authorizeState(t, resp.GetAuthorizeUrl())
	_, err = svc.Complete(context.Background(), resp.GetLoginId(), "code=good-code&state="+state)
	c.NoError(err, "Complete at the advanced clock")
}

// TestBeginRetriesDiscoveryAfterFailure pins lazy discovery: the first Begin
// answers Unavailable when the IdP is down, nothing is cached, the next Begin
// retries and succeeds, and a third Begin rides the cache (no further
// discovery GETs) — an IdP down at daemon start must not break startup.
func TestBeginRetriesDiscoveryAfterFailure(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	iss.failNext = 1
	svc := newTestService(t, iss, nil)

	_, err := svc.Begin(context.Background(), 8080, "laptop")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "Begin while the IdP is down")
	c.StrContains(err.Error(), "identity provider unreachable", "Begin message")

	svc.mu.Lock()
	cached := svc.idp != nil
	svc.mu.Unlock()
	c.False(cached, "a failed discovery must not be cached")

	resp, err := svc.Begin(context.Background(), 8080, "laptop")
	c.NoError(err, "Begin retries discovery")
	c.NotEmpty(resp.GetAuthorizeUrl(), "authorize URL")

	_, err = svc.Begin(context.Background(), 8080, "laptop")
	c.NoError(err, "third Begin")
	c.Eq(2, iss.discoveryN, "discovery GETs: one failure, one success, then the cache")
}

// ─── Complete: guards ───────────────────────────────────────────────────────

// TestCompleteSingleUse pins single use: the pending entry is removed FIRST,
// so a second Complete with the same login id — success or failure in
// between — can never mint a second token.
func TestCompleteSingleUse(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	iss.resetClaims(iss.standardClaims())
	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	armNonce(svc, iss, loginID)
	resp, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.NoError(err, "first Complete")
	c.Eq("rfk_secret", resp.GetToken(), "first Complete token")

	// Same login id, same callback: the entry was consumed by the first.
	_, err = svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "second Complete")
	c.StrContains(err.Error(), "login expired or already used", "second Complete message")
}

// TestCompleteExpired pins the 10-minute pending TTL: a callback arriving
// after it is refused as expired, and the entry is gone.
func TestCompleteExpired(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	svc.now = func() time.Time { return fixedNow.Add(pendingTTL + time.Second) }
	armNonce(svc, iss, loginID)
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "Complete after the TTL")
	c.StrContains(err.Error(), "login expired or already used", "expired message")
}

// TestCompleteStateMismatch pins the anti-CSRF state check and its
// constant-time compare.
func TestCompleteStateMismatch(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	loginID, _, _ := beginLogin(t, svc, 8080, "laptop")
	armNonce(svc, iss, loginID)
	_, err := svc.Complete(context.Background(), loginID,
		"code=good-code&state=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Complete with a foreign state")
	c.StrContains(err.Error(), "state mismatch", "state message")
}

// TestCompleteNonceMismatch pins the replay binding: a validly-signed ID
// token whose nonce claim does not match the one Begin generated is refused.
func TestCompleteNonceMismatch(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	iss.resetClaims(iss.standardClaims())
	iss.setClaims(map[string]any{"nonce": "attacker-chosen-nonce"})
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Complete with a foreign nonce")
	c.StrContains(err.Error(), "nonce mismatch", "nonce message")
}

// TestCompleteIdPError pins the IdP's own refusal path: the error and
// description ride the message, truncated.
func TestCompleteIdPError(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	loginID, _, _ := beginLogin(t, svc, 8080, "laptop")
	_, err := svc.Complete(context.Background(), loginID,
		"error=access_denied&error_description="+url.QueryEscape("the user said no"))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Complete with an IdP error")
	c.StrContains(err.Error(), "identity provider refused: access_denied: the user said no", "refusal message")

	long := strings.Repeat("x", 500)
	loginID2, _, _ := beginLogin(t, svc, 8080, "laptop")
	_, err = svc.Complete(context.Background(), loginID2,
		"error="+url.QueryEscape(strings.Repeat("e", 500))+"&error_description="+url.QueryEscape(long))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Complete with an oversized IdP error")
	c.StrContains(err.Error(), "identity provider refused: ", "refusal message")
	if strings.Contains(err.Error(), long) {
		t.Fatal("error_description was not truncated to 200 bytes")
	}
}

// TestCompleteSendsVerifierAndSameRedirect pins the PKCE and redirect
// round-trip: the token request must carry the SAME verifier Begin generated
// (matching the S256 challenge in the authorize URL) and the SAME redirect
// URI — providers refuse a mismatched redirect_uri as a replay.
func TestCompleteSendsVerifierAndSameRedirect(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	loginID, state, authorizeURL := beginLogin(t, svc, 8080, "laptop")
	p := svc.pendingSnapshot(loginID)
	if p == nil {
		t.Fatal("pending entry missing after Begin")
	}
	iss.resetClaims(iss.standardClaims())
	armNonce(svc, iss, loginID)
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.NoError(err, "Complete")

	// The authorize URL challenged the S256 hash of the verifier...
	au, err := url.Parse(authorizeURL)
	c.NoError(err, "parse authorize URL")
	sum := sha256.Sum256([]byte(p.verifier))
	c.Eq(base64.RawURLEncoding.EncodeToString(sum[:]), au.Query().Get("code_challenge"),
		"code_challenge must be S256(verifier)")
	c.Eq("S256", au.Query().Get("code_challenge_method"), "challenge method")
	c.NotEmpty(au.Query().Get("nonce"), "authorize nonce")

	// ...and the token request sent back that exact verifier and redirect.
	c.Eq(p.verifier, iss.lastVerifier, "code_verifier sent to the token endpoint")
	c.Eq(p.redirectURI, iss.lastRedirectURI, "redirect_uri sent to the token endpoint")
	c.Eq("good-code", iss.lastCode, "authorization code sent")
}

// TestCompleteWrongAudience pins the aud check: a token minted for a
// different client is refused even though the signature is ours.
func TestCompleteWrongAudience(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	iss.resetClaims(iss.standardClaims())
	iss.setClaims(map[string]any{"aud": "other-client"})
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Complete with a foreign audience")
	c.StrContains(err.Error(), "invalid ID token", "audience message")
}

// ─── Complete: claims ───────────────────────────────────────────────────────

// TestCompleteDomainExactMatch pins EXACT domain matching: the configured
// domain matches only itself — no subdomain, no suffix.
func TestCompleteDomainExactMatch(t *testing.T) {
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	t.Run("exact domain accepted", func(t *testing.T) {
		resp := happyComplete(t, svc, iss, nil)
		c := assert.NewAborting(t)
		c.Eq("rfk_secret", resp.GetToken(), "token")
		c.Eq("alice", resp.GetUsername(), "username")
	})
	t.Run("subdomain refused", func(t *testing.T) {
		c := assert.NewAborting(t)
		iss.resetClaims(iss.standardClaims())
		iss.setClaims(map[string]any{"email": "alice@evil.graveland.dev"})
		loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
		armNonce(svc, iss, loginID)
		_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "subdomain email")
		c.StrContains(err.Error(),
			"email domain evil.graveland.dev is not accepted by this daemon", "domain message")
	})
	t.Run("suffix refused", func(t *testing.T) {
		c := assert.NewAborting(t)
		iss.resetClaims(iss.standardClaims())
		iss.setClaims(map[string]any{"email": "alice@graveland.dev.evil.com"})
		loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
		armNonce(svc, iss, loginID)
		_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "suffix email")
		c.StrContains(err.Error(),
			"email domain graveland.dev.evil.com is not accepted by this daemon", "domain message")
	})
}

// TestCompleteEmailVerifiedFalse pins: email_verified false is refused.
func TestCompleteEmailVerifiedFalse(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	iss.resetClaims(iss.standardClaims())
	iss.setClaims(map[string]any{"email_verified": false})
	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	armNonce(svc, iss, loginID)
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "unverified email")
	c.StrContains(err.Error(), "is not verified", "verified message")
}

// TestCompleteEmailVerifiedString pins the string encodings IdPs actually
// send: "false" refused, "true" accepted, any other shape treated as absent
// (accepted) rather than silently refusing every user of the IdP.
func TestCompleteEmailVerifiedString(t *testing.T) {
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	t.Run(`string "false" refused`, func(t *testing.T) {
		c := assert.NewAborting(t)
		iss.resetClaims(iss.standardClaims())
		iss.setClaims(map[string]any{"email_verified": "false"})
		loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
		armNonce(svc, iss, loginID)
		_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "string false")
		c.StrContains(err.Error(), "is not verified", "verified message")
	})
	t.Run(`string "true" accepted`, func(t *testing.T) {
		c := assert.NewAborting(t)
		resp := happyComplete(t, svc, iss, func() { iss.setClaims(map[string]any{"email_verified": "true"}) })
		c.Eq("rfk_secret", resp.GetToken(), "token")
	})
	t.Run("other shape treated as absent", func(t *testing.T) {
		c := assert.NewAborting(t)
		resp := happyComplete(t, svc, iss, func() { iss.setClaims(map[string]any{"email_verified": 1}) })
		c.Eq("rfk_secret", resp.GetToken(), "token")
	})
}

// TestCompleteEmailVerifiedAbsent pins: no email_verified claim is accepted —
// the IdP does not assert verification.
func TestCompleteEmailVerifiedAbsent(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	iss.resetClaims(iss.standardClaims()) // no email_verified key at all
	resp := happyComplete(t, svc, iss, nil)
	c.Eq("rfk_secret", resp.GetToken(), "token")
}

// TestCompleteNoUser pins the no-user answer: PermissionDenied with the
// recovery instructions, not an outage.
func TestCompleteNoUser(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)
	svc.resolver.(*fakeResolver).err = users.ErrOIDCNoUser

	iss.resetClaims(iss.standardClaims())
	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	armNonce(svc, iss, loginID)
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "no user")
	c.StrContains(err.Error(), "no rafiki user for alice@graveland.dev", "no-user message")
	c.StrContains(err.Error(), "rafikid user create --email alice@graveland.dev", "recovery instructions")
}

// TestCompleteConflict pins the binding conflict answer: the email is bound
// to a different subject at the same issuer.
func TestCompleteConflict(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)
	svc.resolver.(*fakeResolver).err = users.ErrOIDCConflict

	iss.resetClaims(iss.standardClaims())
	loginID, state, _ := beginLogin(t, svc, 8080, "laptop")
	armNonce(svc, iss, loginID)
	_, err := svc.Complete(context.Background(), loginID, "code=good-code&state="+state)
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "conflict")
	c.StrContains(err.Error(),
		"already linked to a different account at this identity provider", "conflict message")
}

// TestCompleteMintsOIDCTokenWithTTL pins the mint: OriginOIDC, the config's
// session TTL, the "login <host> <date>" name, and the response fields.
func TestCompleteMintsOIDCTokenWithTTL(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, func(cfg *Config) { cfg.SessionTTL = Duration(2 * time.Hour) })

	exp := fixedNow.Add(2 * time.Hour)
	m := svc.minter.(*fakeMinter)
	m.token = users.Token{ID: "tok_9", Username: "alice", ExpiresAt: &exp}

	resp := happyComplete(t, svc, iss, nil)
	c.Eq("rfk_secret", resp.GetToken(), "token")
	c.Eq("alice", resp.GetUsername(), "username")
	c.Eq(exp.Unix(), resp.GetExpiresAtUnix(), "expires_at_unix")

	c.Eq("u_1", m.gotUserID, "minted for the resolved user")
	c.Eq(users.OriginOIDC, m.gotToken.Origin, "token origin")
	c.Eq(2*time.Hour, m.gotToken.TTL, "session TTL")
	c.Eq("login laptop 2026-05-04", m.gotToken.Name, "token name")

	r := svc.resolver.(*fakeResolver)
	c.Eq(iss.url, r.got.Issuer, "resolver issuer")
	c.Eq("sub-1", r.got.Subject, "resolver subject")
	c.Eq("alice@graveland.dev", r.got.Email, "resolver email")
}

// TestServiceClientHostSanitized pins the clientHost hygiene: control
// characters are stripped and the value is truncated to 64 bytes, since it
// becomes part of a token name and a log line.
func TestServiceClientHostSanitized(t *testing.T) {
	c := assert.NewAborting(t)
	iss := newFakeIssuer(t)
	svc := newTestService(t, iss, nil)

	host := "lap\x1b[31m" + strings.Repeat("p", 100) + "\ntop"
	loginID, _, _ := beginLogin(t, svc, 8080, host)
	p := svc.pendingSnapshot(loginID)
	if p == nil {
		t.Fatal("pending entry missing")
	}
	c.NotStrContains(p.clientHost, "\x1b", "escape character")
	c.NotStrContains(p.clientHost, "\n", "newline")
	if len(p.clientHost) > 64 {
		t.Fatalf("clientHost = %d bytes, want ≤ 64", len(p.clientHost))
	}
}
