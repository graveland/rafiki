// SPDX-License-Identifier: Apache-2.0

package chatgptauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// engineFor points a freshly built engine at a fake issuer.
func engineFor(srv *httptest.Server) *Engine {
	return NewWithEndpoints(srv.Client(),
		srv.URL+"/oauth/authorize", srv.URL+"/oauth/token", srv.URL+"/oauth/revoke")
}

// queryOf parses an authorize URL into its query parameters.
func queryOf(t *testing.T, raw string) url.Values {
	t.Helper()
	c := assert.NewAborting(t)
	u, err := url.Parse(raw)
	c.NoError(err, "parse authorize URL")
	return u.Query()
}

// mkJWT builds an unsigned JWT carrying claims. Nothing in this package
// verifies a signature, so the third segment is a placeholder.
func mkJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	c := assert.NewAborting(t)
	payload, err := json.Marshal(claims)
	c.NoError(err, "encode JWT payload")
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// codeExchangeServer answers the token endpoint with a fixed body and records
// the form it was sent, so a test can assert the verifier round-tripped.
type codeExchangeServer struct {
	srv          *httptest.Server
	body         string
	gotVerifier  string
	gotRedirect  string
	gotGrantType string
	gotCode      string
}

func newCodeExchangeServer(t *testing.T, body string) *codeExchangeServer {
	t.Helper()
	f := &codeExchangeServer{body: body}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Not t.Fatal here: this runs on the server's goroutine. The test
		// asserts on what was recorded once the call has returned.
		_ = r.ParseForm()
		f.gotVerifier = r.PostForm.Get("code_verifier")
		f.gotRedirect = r.PostForm.Get("redirect_uri")
		f.gotGrantType = r.PostForm.Get("grant_type")
		f.gotCode = r.PostForm.Get("code")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// ─── Begin ──────────────────────────────────────────────────────────────────

func TestBeginAuthorizeURL(t *testing.T) {
	c := assert.NewAborting(t)
	e := New(nil)

	loginID, raw, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")
	c.NotEmpty(loginID, "login id")

	u, err := url.Parse(raw)
	c.NoError(err, "parse authorize URL")
	c.Eq("https://auth.openai.com/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)

	q := u.Query()
	c.Eq("code", q.Get("response_type"))
	c.Eq(ClientID, q.Get("client_id"))
	c.Eq("http://127.0.0.1:1455/auth/callback", q.Get("redirect_uri"))
	c.Eq(Scopes, q.Get("scope"))
	c.Eq("S256", q.Get("code_challenge_method"))
	c.NotEmpty(q.Get("code_challenge"), "code challenge")
	c.NotEmpty(q.Get("state"), "state")
	c.Eq("true", q.Get("id_token_add_organizations"))
	c.Eq("true", q.Get("codex_cli_simplified_flow"))
	c.Eq("codex_cli_rs", q.Get("originator"))

	// Exactly these ten, none extra and none missing: the issuer registered
	// this client against a fixed parameter set, and a stray one is a change
	// nobody reviewed.
	want := []string{
		"client_id",
		"code_challenge",
		"code_challenge_method",
		"codex_cli_simplified_flow",
		"id_token_add_organizations",
		"originator",
		"redirect_uri",
		"response_type",
		"scope",
		"state",
	}
	c.EqDeep(want, slices.Sorted(maps.Keys(q)), "authorize URL parameters")
}

// ─── Complete ───────────────────────────────────────────────────────────────

func TestCompleteExchangesWithVerifier(t *testing.T) {
	c := assert.NewAborting(t)
	idToken := mkJWT(t, map[string]any{
		authClaim: map[string]any{"chatgpt_account_id": "acct-123"},
	})
	fake := newCodeExchangeServer(t, fmt.Sprintf(
		`{"access_token":"access-1","refresh_token":"refresh-1","id_token":%q,"expires_in":3600,"token_type":"Bearer"}`,
		idToken))
	e := engineFor(fake.srv)

	loginID, authorizeURL, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")
	q := queryOf(t, authorizeURL)

	provider, tok, err := e.Complete(context.Background(), "owner-1", loginID,
		"code=auth-code&state="+url.QueryEscape(q.Get("state")))
	c.NoError(err, "Complete")

	c.Eq("chatgpt", provider)
	c.Eq("access-1", tok.AccessToken)
	c.Eq("refresh-1", tok.RefreshToken)
	c.Eq(idToken, tok.IDToken)
	c.Eq("acct-123", tok.AccountID)
	c.True(!tok.Expiry.IsZero(), "expiry from expires_in")

	c.Eq("authorization_code", fake.gotGrantType)
	c.Eq("auth-code", fake.gotCode)
	c.Eq(redirectURI(), fake.gotRedirect)
	// The verifier is only replayable against the challenge Begin published.
	sum := sha256.Sum256([]byte(fake.gotVerifier))
	c.Eq(q.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(sum[:]))
}

func TestCompleteStateMismatch(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newCodeExchangeServer(t, `{"access_token":"access-1"}`)
	e := engineFor(fake.srv)

	loginID, _, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")

	_, _, err = e.Complete(context.Background(), "owner-1", loginID, "code=auth-code&state=forged")
	c.Error(err, "Complete with a forged state")
	c.StrContains(err.Error(), "state mismatch")
	c.Eq("", fake.gotCode, "the exchange must not run on a state mismatch")
}

func TestCompleteWrongOwner(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newCodeExchangeServer(t, `{"access_token":"access-1","refresh_token":"refresh-1"}`)
	e := engineFor(fake.srv)

	loginID, authorizeURL, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")
	callback := "code=auth-code&state=" + url.QueryEscape(queryOf(t, authorizeURL).Get("state"))

	_, _, foreignErr := e.Complete(context.Background(), "owner-2", loginID, callback)
	_, _, absentErr := e.Complete(context.Background(), "owner-1", "no-such-login", callback)

	c.Error(foreignErr, "Complete by another owner")
	c.ErrorIs(foreignErr, errUnknownLogin)
	c.EqualError(foreignErr, absentErr.Error(),
		"a foreign login id must be indistinguishable from an absent one")
	c.Eq("", fake.gotCode, "a foreign owner must not reach the exchange")

	// Knowing a login id must not let anyone CANCEL someone else's login, so
	// the foreign attempt above had to leave the entry alone.
	_, tok, err := e.Complete(context.Background(), "owner-1", loginID, callback)
	c.NoError(err, "the real owner's login still completes after a foreign attempt")
	c.Eq("access-1", tok.AccessToken)
}

func TestCompleteConsumesEntry(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newCodeExchangeServer(t, `{"access_token":"access-1","refresh_token":"refresh-1"}`)
	e := engineFor(fake.srv)

	loginID, authorizeURL, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")
	callback := "code=auth-code&state=" + url.QueryEscape(queryOf(t, authorizeURL).Get("state"))

	_, _, err = e.Complete(context.Background(), "owner-1", loginID, callback)
	c.NoError(err, "first Complete")

	_, _, err = e.Complete(context.Background(), "owner-1", loginID, callback)
	c.ErrorIs(err, errUnknownLogin, "a replayed callback must not mint a second credential")
}

func TestCompleteConsumesEntryOnFailedExchange(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newJSONServer(t, 500, `{"error":"server_error"}`)
	e := engineFor(fake.srv)

	loginID, authorizeURL, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")
	callback := "code=auth-code&state=" + url.QueryEscape(queryOf(t, authorizeURL).Get("state"))

	_, _, err = e.Complete(context.Background(), "owner-1", loginID, callback)
	c.Error(err, "the exchange must fail")
	c.NotErrorIs(err, errUnknownLogin, "the entry was found; the exchange is what failed")

	// Consumed on use, not on success: a login that reached the exchange never
	// works twice, even when the exchange itself failed.
	_, _, err = e.Complete(context.Background(), "owner-1", loginID, callback)
	c.ErrorIs(err, errUnknownLogin, "a failed exchange must still consume the login")
}

func TestCompleteIdPError(t *testing.T) {
	c := assert.NewAborting(t)
	e := New(nil)
	loginID, _, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")

	_, _, err = e.Complete(context.Background(), "owner-1", loginID,
		"error=access_denied&error_description=user+said+no")
	c.Error(err, "Complete with an issuer refusal")
	c.StrContains(err.Error(), "access_denied")
	c.StrContains(err.Error(), "user said no")
}

func TestBeginReplacesSameOwnerProvider(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newCodeExchangeServer(t, `{"access_token":"access-1","refresh_token":"refresh-1"}`)
	e := engineFor(fake.srv)

	firstID, _, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "first Begin")
	secondID, secondURL, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "second Begin")
	c.NotEq(firstID, secondID)

	_, _, err = e.Complete(context.Background(), "owner-1", firstID, "")
	c.ErrorIs(err, errUnknownLogin, "the replaced login must be gone")

	callback := "code=auth-code&state=" + url.QueryEscape(queryOf(t, secondURL).Get("state"))
	_, tok, err := e.Complete(context.Background(), "owner-1", secondID, callback)
	c.NoError(err, "Complete the replacement")
	c.Eq("access-1", tok.AccessToken)
}

// ─── pending map bounds ─────────────────────────────────────────────────────

func TestPendingExpiry(t *testing.T) {
	c := assert.NewAborting(t)
	e := New(nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	loginID, _, err := e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin")
	// A state mismatch proves the entry was found AND live, which is what
	// makes the second half of this test about the TTL rather than about
	// Begin having stored nothing.
	now = now.Add(pendingTTL / 2)
	_, _, err = e.Complete(context.Background(), "owner-1", loginID, "code=auth-code&state=forged")
	c.NotErrorIs(err, errUnknownLogin, "halfway through the TTL the login is still known")
	c.StrContains(err.Error(), "state mismatch")

	loginID, _, err = e.Begin("owner-1", "chatgpt")
	c.NoError(err, "Begin again")
	now = now.Add(pendingTTL + time.Second)
	_, _, err = e.Complete(context.Background(), "owner-1", loginID, "code=auth-code&state=forged")
	c.ErrorIs(err, errUnknownLogin, "an entry past its TTL must be refused")
}

func TestPendingExpirySweptOnBegin(t *testing.T) {
	c := assert.NewAborting(t)
	e := New(nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	for i := range maxPendingLogins {
		_, _, err := e.Begin(fmt.Sprintf("owner-%d", i), "chatgpt")
		c.NoError(err, "Begin %d", i)
	}

	// Every entry is now past its TTL. A new Begin must still find room,
	// which only happens if the sweep clause (as opposed to the pair
	// replacement) actually deletes expired entries.
	now = now.Add(pendingTTL + time.Second)
	loginID, _, err := e.Begin("owner-after-expiry", "chatgpt")
	c.NoError(err, "an expired entry must not occupy a slot")
	c.NotEmpty(loginID)

	// And the swept entries are really gone, not merely uncounted: only the
	// one just begun is left.
	e.mu.Lock()
	remaining := len(e.pending)
	e.mu.Unlock()
	c.Eq(1, remaining, "Begin must delete expired entries, not just ignore them")
}

func TestPendingCap(t *testing.T) {
	c := assert.NewAborting(t)
	e := New(nil)

	for i := range maxPendingLogins {
		_, _, err := e.Begin(fmt.Sprintf("owner-%d", i), "chatgpt")
		c.NoError(err, "Begin %d", i)
	}
	_, _, err := e.Begin("owner-overflow", "chatgpt")
	c.Error(err, "Begin past the cap")
	c.StrContains(err.Error(), "too many logins")

	// Replacing an existing pair frees its slot, so a user restarting a login
	// is never locked out by other users' pending entries.
	_, _, err = e.Begin("owner-0", "chatgpt")
	c.NoError(err, "Begin replacing an existing pair at the cap")
}
