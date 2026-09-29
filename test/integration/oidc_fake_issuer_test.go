package integration_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// The fake OIDC identity provider the login tests run against. It is a real
// HTTP server with the four surfaces the daemon's login engine actually walks:
// discovery, JWKS, /authorize and /token — so the code under test does the
// full authorization-code + PKCE round trip, including a real RS256 signature
// check, against something that behaves like an IdP. Nothing here is stubbed
// at the Go seam: the daemon discovers, redirects, exchanges and verifies
// exactly as it would against a live provider.
//
// What it deliberately does NOT do: open any browser-side surface on its own.
// The tests fetch /authorize with plain http.Get — the "browser" in these
// flows is the test itself.

// oidcTestClientID is the client_id every test writes into oidc.toml; the fake
// refuses any other.
const oidcTestClientID = "rafiki-test-client"

// oidcTestSecretEnv is the env var oidc.toml's client_secret_env names. The
// value differs per test daemon, so one name can be reused across tests
// without the daemons sharing anything.
const oidcTestSecretEnv = "RAFIKI_TEST_OIDC_CLIENT_SECRET"

// fakeUserClaims are the claims the IdP asserts about the NEXT authorize
// request. Tests set them before driving a login; the claims ride the issued
// authorization code into the minted ID token.
type fakeUserClaims struct {
	Sub, Email string
	// EmailVerified nil omits the claim entirely (the daemon treats absent as
	// "not asserted"); set it to encode true or false.
	EmailVerified *bool
}

// codeGrant is one issued authorization code: the claims it will mint, plus
// everything /token must replay-check against the daemon's exchange request.
// The challenge method needs no recording: /authorize only accepts S256, so
// the verifier check below hashes the same way.
type codeGrant struct {
	claims        fakeUserClaims
	nonce         string
	codeChallenge string
	redirectURI   string
	clientID      string
	expires       time.Time
}

// fakeIssuer is an in-process OpenID Connect provider. One instance per test;
// the issuer URL is the server's own loopback address, which pkg/oidclogin's
// config validation accepts for http.
type fakeIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	secret string

	mu     sync.Mutex
	claims fakeUserClaims
	codes  map[string]*codeGrant
	tokens int // exchange counter; the access_token varies so no two look alike
}

// newFakeIssuer starts the IdP. secret is the client secret /token will
// demand — the same value the test puts in the daemon's env under
// oidcTestSecretEnv.
func newFakeIssuer(t *testing.T, secret string) *fakeIssuer {
	t.Helper()
	c := assert.NewAborting(t)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	c.NoError(err, "generate the IdP's RSA signing key")

	f := &fakeIssuer{
		key:    key,
		kid:    "it-test-key",
		secret: secret,
		codes:  make(map[string]*codeGrant),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks", f.handleJWKS)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	mux.HandleFunc("/token", f.handleToken)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// URL is the issuer address to write into oidc.toml.
func (f *fakeIssuer) URL() string { return f.server.URL }

// setClaims fixes what the next authorize request asserts about its user.
func (f *fakeIssuer) setClaims(c fakeUserClaims) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = c
}

func (f *fakeIssuer) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleDiscovery serves the provider metadata. The issuer field must equal
// the URL the daemon was configured with — go-oidc refuses a mismatch.
func (f *fakeIssuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	f.writeJSON(w, map[string]any{
		"issuer":                                f.server.URL,
		"authorization_endpoint":                f.server.URL + "/authorize",
		"token_endpoint":                        f.server.URL + "/token",
		"jwks_uri":                              f.server.URL + "/jwks",
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

// handleJWKS publishes the signing key. go-oidc fetches it at Verify time and
// matches the ID token's kid against it.
func (f *fakeIssuer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	f.writeJSON(w, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": f.kid,
			"n":   base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e":   "AQAB",
		}},
	})
}

// loopbackRedirect matches the only redirect_uri shape the daemon builds:
// http on 127.0.0.1, the fixed /oidc/callback path, any port — the IdP must be
// port-agnostic here, exactly like Vault's own loopback redirect handling,
// because the daemon's redirect_uri is whatever port the CLI's listener bound.
var loopbackRedirect = regexp.MustCompile(`^http://127\.0\.0\.1:\d+/oidc/callback$`)

// handleAuthorize validates the daemon's authorize request, records the nonce
// and PKCE challenge against a freshly minted code, and answers with the
// 302 the browser would follow: back to the redirect_uri with code and state.
func (f *fakeIssuer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unparseable authorize request", http.StatusBadRequest)
		return
	}
	q := r.Form
	redirectURI := q.Get("redirect_uri")
	switch {
	case q.Get("response_type") != "code":
		http.Error(w, "response_type must be code", http.StatusBadRequest)
		return
	case q.Get("client_id") != oidcTestClientID:
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	case !loopbackRedirect.MatchString(redirectURI):
		http.Error(w, "redirect_uri must be http://127.0.0.1:<port>/oidc/callback", http.StatusBadRequest)
		return
	case q.Get("state") == "":
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	case q.Get("nonce") == "":
		http.Error(w, "missing nonce", http.StatusBadRequest)
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		http.Error(w, "PKCE S256 is required", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	claims := f.claims
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	f.codes[code] = &codeGrant{
		claims:        claims,
		nonce:         q.Get("nonce"),
		codeChallenge: q.Get("code_challenge"),
		redirectURI:   redirectURI,
		clientID:      q.Get("client_id"),
		expires:       time.Now().Add(10 * time.Minute),
	}
	f.mu.Unlock()

	target := redirectURI + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

// clientCredentials extracts the client_id/client_secret pair from either the
// form or HTTP Basic auth: x/oauth2 probes Basic first and only falls back to
// the form after a failure, so a fake that reads both shapes serves both.
func clientCredentials(form url.Values, r *http.Request) (id, secret string) {
	if u, p, ok := r.BasicAuth(); ok {
		return u, p
	}
	return form.Get("client_id"), form.Get("client_secret")
}

// handleToken answers the daemon's code exchange. It refuses the request the
// way a real IdP would when the client secret is wrong, the redirect_uri
// differs from the authorize request's, or the PKCE verifier does not hash to
// the recorded challenge — and on success returns the signed ID token carrying
// the recorded nonce and the test-controlled claims.
func (f *fakeIssuer) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request", "unparseable token request")
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		tokenError(w, "unsupported_grant_type", "grant_type must be authorization_code")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	grant, ok := f.codes[r.Form.Get("code")]
	if !ok {
		tokenError(w, "invalid_grant", "unknown authorization code")
		return
	}
	if time.Now().After(grant.expires) {
		delete(f.codes, r.Form.Get("code"))
		tokenError(w, "invalid_grant", "authorization code expired")
		return
	}
	id, secret := clientCredentials(r.Form, r)
	if grant.clientID != "" && id != grant.clientID {
		tokenError(w, "invalid_client", "client_id mismatch")
		return
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(f.secret)) != 1 {
		tokenError(w, "invalid_client", "client secret mismatch")
		return
	}
	if r.Form.Get("redirect_uri") != grant.redirectURI {
		tokenError(w, "invalid_grant", "redirect_uri differs from the authorize request")
		return
	}
	verifier := r.Form.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])),
		[]byte(grant.codeChallenge)) != 1 {
		tokenError(w, "invalid_grant", "PKCE verifier does not match the code challenge")
		return
	}

	// Single use, success or replay: the code is consumed once the exchange
	// has validated, so a second attempt fails as unknown.
	delete(f.codes, r.Form.Get("code"))
	f.tokens++
	f.writeJSON(w, map[string]any{
		"access_token": "fake-access-" + fmt.Sprint(f.tokens),
		"token_type":   "Bearer",
		"expires_in":   600,
		"scope":        "openid email profile",
		"id_token":     f.signIDToken(*grant),
	})
}

// signIDToken renders the RS256 JWT the daemon will verify: the recorded
// nonce, the recorded subject/email claims, and the audience the verifier
// demands.
func (f *fakeIssuer) signIDToken(g codeGrant) string {
	now := time.Now()
	claims := map[string]any{
		"iss":   f.server.URL,
		"sub":   g.claims.Sub,
		"aud":   g.clientID,
		"exp":   now.Add(10 * time.Minute).Unix(),
		"iat":   now.Unix(),
		"nonce": g.nonce,
	}
	if g.claims.Email != "" {
		claims["email"] = g.claims.Email
	}
	if g.claims.EmailVerified != nil {
		claims["email_verified"] = *g.claims.EmailVerified
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": f.kid}
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	signed := enc.EncodeToString(h) + "." + enc.EncodeToString(p)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		// Unreachable in practice; a signing failure must not hand the daemon
		// an unsigned string that would fail the signature check anyway.
		return signed + ".cc0ffbad"
	}
	return signed + "." + enc.EncodeToString(sig)
}

// tokenError writes the RFC 6749 error shape x/oauth2 knows how to surface.
func tokenError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}
