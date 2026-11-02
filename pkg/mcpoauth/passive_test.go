// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The passive half's contract is negative: it must resolve one exact binding,
// consult the store on every acquisition, and never do anything interactive. Each
// test here pins one of those refusals.

func TestMcpOAuthPassiveMissingCredentialKeepsTheDial(t *testing.T) {
	fx := newEngineFixture(t)
	handler, client, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if handler == nil || client == nil {
		t.Fatal("the factory returned no handler or client")
	}
	ts, err := handler.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if ts != nil {
		t.Fatal("a login-less owner got a token source")
	}
	for _, call := range fx.store.callLog() {
		if call != "LookupBinding" {
			t.Fatalf("the factory made a non-lookup store call: %v", fx.store.callLog())
		}
	}
	// No browser, no discovery: the only requests are the caller's own.
	if got := len(fx.oauth.requestLog()); got != 0 {
		t.Fatalf("the factory made %d requests: %v", got, fx.oauth.requestLog())
	}
}

func TestMcpOAuthPassiveExplicitAuthorizationDisablesOAuth(t *testing.T) {
	for _, key := range []string{"Authorization", "authorization", "AUTHORIZATION"} {
		t.Run(key, func(t *testing.T) {
			fx := newEngineFixture(t)
			// A stored credential exists and is perfectly usable: the explicit
			// header still wins, even with an empty value.
			fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
				Token{AccessToken: "seeded", Expiry: time.Now().Add(time.Hour)})

			handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(),
				map[string]string{key: ""})
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			ts, err := handler.TokenSource(context.Background())
			if err != nil {
				t.Fatalf("TokenSource: %v", err)
			}
			if ts != nil {
				t.Fatal("an explicit Authorization header did not disable stored OAuth")
			}
			if calls := fx.store.callLog(); len(calls) != 0 {
				t.Fatalf("the factory consulted the store anyway: %v", calls)
			}
		})
	}
}

func TestMcpOAuthPassiveUnrelatedHeadersCoexistWithOAuth(t *testing.T) {
	fx := newEngineFixture(t)
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "seeded", Expiry: time.Now().Add(time.Hour)})

	handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(),
		map[string]string{"X-Tenant": "acme", "Accept": "application/json"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ts, err := handler.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource = %v, %v; unrelated headers must not disable OAuth", ts, err)
	}
}

func TestMcpOAuthPassiveStoreFailureRefusesTheDial(t *testing.T) {
	fx := newEngineFixture(t)
	fx.store.lookupErr = ErrUnavailable

	_, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("factory = %v, want the store failure", err)
	}
}

func TestMcpOAuthPassiveTokenIsAcquiredEveryTime(t *testing.T) {
	fx := newEngineFixture(t)
	grant := fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "first", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour), RefreshToken: "r1"})

	handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ts, err := handler.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource = %v, %v", ts, err)
	}
	first, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if first.AccessToken != "first" {
		t.Fatalf("token = %q, want first", first.AccessToken)
	}

	// Somebody else rotates the row. The next acquisition must see it: there is
	// no cache between the database and the request.
	fx.store.mu.Lock()
	stored := fx.store.grants[fakeKey(fx.owner, grant.Resource)]
	stored.Token.AccessToken = "second"
	fx.store.mu.Unlock()

	second, err := ts.Token()
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if second.AccessToken != "second" {
		t.Fatalf("token = %q, want second (the row is the authority)", second.AccessToken)
	}
	if got := fx.store.acquires; got != 2 {
		t.Fatalf("acquires = %d, want one per acquisition", got)
	}
}

func TestMcpOAuthPassiveStaleBindingAsksForALogin(t *testing.T) {
	fx := newEngineFixture(t)
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "seeded", Expiry: time.Now().Add(time.Hour)})

	handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ts, err := handler.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource = %v, %v", ts, err)
	}

	// A newer explicit login replaced the grant this source pinned.
	fx.store.rebaseGrant(fx.owner, fx.oauth.endpoint())

	_, err = ts.Token()
	requireLoginHint(t, err, "fixture")
}

func TestMcpOAuthPassiveRevokedGrantAsksForALogin(t *testing.T) {
	fx := newEngineFixture(t)
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "seeded", Expiry: time.Now().Add(time.Hour)})

	handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ts, err := handler.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource = %v, %v", ts, err)
	}
	if _, err := fx.store.InvalidateResource(context.Background(), fx.owner, fx.oauth.endpoint()); err != nil {
		t.Fatalf("InvalidateResource: %v", err)
	}
	_, err = ts.Token()
	requireLoginHint(t, err, "fixture")
}

func TestMcpOAuthPassiveAuthorizeIsNeverInteractive(t *testing.T) {
	fx := newEngineFixture(t)
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "seeded", Expiry: time.Now().Add(time.Hour)})

	endpoint := fx.oauth.endpoint()
	handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", endpoint, nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	before := len(fx.oauth.requestLog())
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	authErr := handler.Authorize(context.Background(), req, &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody})
	requireLoginHint(t, authErr, "fixture")
	if got := len(fx.oauth.requestLog()); got != before {
		t.Fatalf("Authorize made %d requests; it must make none", got-before)
	}
	// No login was begun either: a passive 401 cannot open a browser.
	if got := len(fx.eng.flows); got != 0 {
		t.Fatalf("Authorize started %d logins", got)
	}
}

func TestMcpOAuthPassiveSessionSendsTheStoredToken(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.issueToken("seeded-access", "read", "write")
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "seeded-access", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})

	handler, client, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "passive-test", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             fx.oauth.endpoint(),
			HTTPClient:           client,
			OAuthHandler:         handler,
			DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		t.Fatalf("connect with a stored credential: %v", err)
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("tools = %d, want the fixture's two", len(tools.Tools))
	}
}

func TestMcpOAuthPassiveSessionWithoutACredentialCarriesTheHint(t *testing.T) {
	fx := newEngineFixture(t)

	handler, client, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = mcp.NewClient(&mcp.Implementation{Name: "passive-test", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             fx.oauth.endpoint(),
			HTTPClient:           client,
			OAuthHandler:         handler,
			DisableStandaloneSSE: true,
		}, nil)
	if err == nil {
		t.Fatal("an unauthenticated session against a protected server succeeded")
	}
	if !strings.Contains(err.Error(), "needs a login: run `rafiki mcp login fixture`") {
		t.Fatalf("error = %v, want the login hint", err)
	}
	// Nothing discovery-shaped happened and no login was started.
	for _, req := range fx.oauth.requestLog() {
		if strings.HasPrefix(req, "GET ") {
			t.Fatalf("a passive 401 performed discovery: %v", fx.oauth.requestLog())
		}
	}
}

// requireLoginHint asserts the exact operator-facing instruction, both as text
// and as the sentinel a caller can branch on.
func requireLoginHint(t *testing.T, err error, server string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a login hint, got nil")
	}
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("error %v does not unwrap to ErrLoginRequired", err)
	}
	want := "server " + server + " needs a login: run `rafiki mcp login " + server + "`"
	if got := err.Error(); got != want {
		t.Fatalf("hint = %q, want %q", got, want)
	}
	var lr *LoginRequiredError
	if !errors.As(err, &lr) {
		t.Fatalf("error %v is not a *LoginRequiredError", err)
	}
	if lr.Server != server {
		t.Fatalf("hint names %q, want %q", lr.Server, server)
	}
}
