// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A refresh is the one upstream call that spends a single-use credential, so
// these tests are about what the request must carry (the resource and the stored
// client credentials) and what a response may and may not replace.

func refreshableGrant(fx *engineFixture, refreshToken string) Grant {
	return Grant{
		OwnerUserID: fx.owner,
		Resource:    fx.oauth.endpoint(),
		Issuer:      fx.oauth.issuer(),
		Generation:  1,
		Registration: Registration{
			ClientID:           "client-refresh",
			ClientSecret:       "not-really-secret",
			AuthStyle:          AuthStyleInParams,
			TokenEndpoint:      fx.oauth.srv.URL + "/token",
			RevocationEndpoint: fx.oauth.srv.URL + "/revoke",
		},
		Scopes: []string{"read"},
		Token: Token{
			AccessToken:  "old-access",
			RefreshToken: refreshToken,
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(-time.Minute),
		},
		RefreshState: RefreshIdle,
	}
}

func TestMcpOAuthRefreshSendsTheResourceAndStoredCredentials(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.refreshOK["refresh-1"] = true })
	grant := refreshableGrant(fx, "refresh-1")

	result, err := fx.eng.refresh(context.Background(), grant)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if result.AccessToken == "" || result.AccessToken == "old-access" {
		t.Fatalf("access token = %q, want the rotated one", result.AccessToken)
	}
	if result.Expiry.IsZero() {
		t.Fatal("refresh result has no expiry although expires_in was reported")
	}
	if !sameScopes(result.Scopes, "read", "write") {
		t.Fatalf("scopes = %v, want the reported read write", result.Scopes)
	}

	form := fx.oauth.tokenForm(0)
	if form == nil {
		t.Fatal("the refresh made no request")
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := form.Get("refresh_token"); got != "refresh-1" {
		t.Fatalf("refresh_token = %q", got)
	}
	// RFC 8707: the token is asked for this exact resource.
	if got, want := form.Get("resource"), grant.Resource; got != want {
		t.Fatalf("resource = %q, want %q", got, want)
	}
	// The stored client credentials, in the style the registration recorded.
	if got, want := form.Get("client_id"), "client-refresh"; got != want {
		t.Fatalf("client_id = %q, want %q", got, want)
	}
	if got, want := form.Get("client_secret"), "not-really-secret"; got != want {
		t.Fatalf("client_secret was not sent in the body as recorded")
	}
}

func TestMcpOAuthRefreshHonoursTheHeaderAuthStyle(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.refreshOK["refresh-1"] = true })
	grant := refreshableGrant(fx, "refresh-1")
	grant.Registration.AuthStyle = AuthStyleInHeader

	if _, err := fx.eng.refresh(context.Background(), grant); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	form := fx.oauth.tokenForm(0)
	if form.Get("client_secret") != "" {
		t.Fatal("the client secret was placed in the body under the header style")
	}
	if form.Get("client_id") != "" {
		t.Fatal("the client id was placed in the body under the header style")
	}
	if !fx.oauth.lastTokenHadBasicAuth {
		t.Fatal("the refresh request carried no Basic authorization")
	}
}

func TestMcpOAuthRefreshKeepsTheStoredTokenWhenNoneIsIssued(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.refreshOK["refresh-1"] = true })
	grant := refreshableGrant(fx, "refresh-1")

	result, err := fx.eng.refresh(context.Background(), grant)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// An omitted replacement is "unchanged", never "revoked": the store keeps
	// what it has, which is what this empty field tells it to do.
	if result.RefreshToken != "" {
		t.Fatalf("refresh token = %q, want empty so the store keeps the stored one", result.RefreshToken)
	}
	// The same token is still usable for the next refresh.
	if _, err := fx.eng.refresh(context.Background(), grant); err != nil {
		t.Fatalf("second refresh with the kept token: %v", err)
	}
}

func TestMcpOAuthRefreshInvalidGrantNeedsALogin(t *testing.T) {
	fx := newEngineFixture(t)
	grant := refreshableGrant(fx, "unknown-refresh")

	_, err := fx.eng.refresh(context.Background(), grant)
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("refresh = %v, want ErrLoginRequired", err)
	}
}

func TestMcpOAuthRefreshRejectsANonBearerTokenType(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.refreshOK["refresh-1"] = true
		f.tokenType = "mac"
	})
	grant := refreshableGrant(fx, "refresh-1")

	_, err := fx.eng.refresh(context.Background(), grant)
	if err == nil {
		t.Fatal("refresh accepted a token type this client cannot use")
	}
	if strings.Contains(err.Error(), "access-") {
		t.Fatalf("the refusal quoted token material: %v", err)
	}
}

func TestMcpOAuthRefreshRefusesATokenEndpointOutsideThePolicy(t *testing.T) {
	fx := newEngineFixture(t)
	grant := refreshableGrant(fx, "refresh-1")
	grant.Registration.TokenEndpoint = "http://10.0.0.5/token"

	_, err := fx.eng.refresh(context.Background(), grant)
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("refresh = %v, want ErrDestinationDenied", err)
	}
	if got := fx.oauth.tokenCallCount(); got != 0 {
		t.Fatalf("the refresh reached %d token endpoints", got)
	}
}

func TestMcpOAuthRefreshRevocationFailureKeepsTheLocalInvalidation(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.issueToken("seeded-access", "read", "write")
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{
			AccessToken:  "seeded-access",
			RefreshToken: "seeded-refresh",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour),
		})
	fx.oauth.set(func(f *fakeOAuthServer) { f.revokeStatus = http.StatusInternalServerError })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Resource: fx.oauth.endpoint()}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The logout succeeded locally and tried the revoke once, even though the
	// server refused it.
	if got := fx.oauth.revokeCount(); got == 0 {
		t.Fatal("Delete did not attempt the upstream revocation")
	}
	if _, err := fx.store.LookupBinding(ctx, fx.owner, fx.oauth.endpoint()); !errors.Is(err, ErrMissing) {
		t.Fatalf("the binding survived a logout whose revoke failed: %v", err)
	}
	// A session holding the old generation cannot use it, and cannot reinstal it.
	if _, err := fx.store.Acquire(ctx, fx.owner, fx.oauth.endpoint(), 1, nil); !errors.Is(err, ErrMissing) {
		t.Fatalf("Acquire after logout = %v, want ErrMissing", err)
	}
	// Logging out twice is idempotent, and an absent resource is ErrMissing.
	if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Resource: fx.oauth.endpoint()}); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Resource: "http://127.0.0.1:1/absent"}); !errors.Is(err, ErrMissing) {
		t.Fatalf("Delete of an absent resource = %v, want ErrMissing", err)
	}
}

func TestMcpOAuthRefreshPublicClientSendsNoBasicAuth(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.refreshOK["refresh-1"] = true })
	// A public client: dynamic registration asked for token_endpoint_auth_method
	// "none", and the issuer issued no secret. AuthStyleAuto is what an issuer that
	// named no method leaves us with.
	grant := refreshableGrant(fx, "refresh-1")
	grant.Registration.ClientSecret = ""
	grant.Registration.AuthStyle = AuthStyleAuto

	result, err := fx.eng.refresh(context.Background(), grant)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if result.AccessToken == "" {
		t.Fatal("refresh returned no access token")
	}
	if got := fx.oauth.tokenCallCount(); got != 1 {
		t.Fatalf("token requests = %d, want exactly one", got)
	}
	if fx.oauth.lastTokenHadBasicAuth {
		t.Fatal("a public client was sent HTTP Basic with an empty secret")
	}
	form := fx.oauth.tokenForm(0)
	if got, want := form.Get("client_id"), grant.Registration.ClientID; got != want {
		t.Fatalf("client_id = %q, want %q in the body", got, want)
	}
	if form.Get("client_secret") != "" {
		t.Fatal("a public client sent a client_secret")
	}
}

func TestMcpOAuthRefreshNeverRetriesUnderAnotherAuthStyle(t *testing.T) {
	fx := newEngineFixture(t)
	// The endpoint refuses the credentials' placement. A retry would re-send a
	// single-use refresh token that the first attempt has already spent.
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.refreshOK["refresh-1"] = true
		f.tokenRefuseStatus = http.StatusUnauthorized
		f.tokenRefuseBody = `{"error":"invalid_client"}`
	})
	grant := refreshableGrant(fx, "refresh-1")
	grant.Registration.AuthStyle = AuthStyleAuto

	if _, err := fx.eng.refresh(context.Background(), grant); err == nil {
		t.Fatal("refresh succeeded against an endpoint that refused the client")
	}
	if got := fx.oauth.tokenCallCount(); got != 1 {
		t.Fatalf("token requests = %d, want exactly one (a retry would double-spend the rotation)", got)
	}
	if !fx.oauth.lastTokenHadBasicAuth {
		t.Fatal("a confidential client with no established style was not sent the RFC 7591 default (Basic)")
	}
}

func TestMcpOAuthRefreshWithoutALifetimeExpiresImmediately(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.refreshOK["refresh-1"] = true
		f.omitRefreshExpiry = true
	})
	grant := refreshableGrant(fx, "refresh-1")

	result, err := fx.eng.refresh(context.Background(), grant)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// The store treats a zero expiry as "never refreshes again". An answer with no
	// lifetime must not become an immortal token: it is immediately expired, so the
	// next acquisition refreshes.
	if result.Expiry.IsZero() {
		t.Fatal("a refresh answer with no expires_in produced a zero (never-expiring) expiry")
	}
	if result.Expiry.After(time.Now()) {
		t.Fatalf("expiry = %v, want a lifetime that has already elapsed", result.Expiry)
	}

	// End to end through the passive source: every acquisition refreshes, because
	// the token it just received is already expired.
	fx.store.seed(fx.owner, fx.oauth.endpoint(), fx.oauth.endpoint(), fx.oauth.issuer(),
		Token{AccessToken: "expired", RefreshToken: "refresh-1", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)})
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
		t.Fatalf("first Token: %v", err)
	}
	second, err := ts.Token()
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if first.AccessToken == second.AccessToken {
		t.Fatalf("both acquisitions reused %q: a token with no lifetime was treated as valid", first.AccessToken)
	}
	if got := fx.store.refreshes; got != 2 {
		t.Fatalf("refreshes = %d, want one per acquisition", got)
	}
}

func TestMcpOAuthRefreshErrorTextIsAuthored(t *testing.T) {
	const sentinel = "sentinel-upstream-body-4b71"
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.tokenRefuseStatus = http.StatusBadRequest
		f.tokenRefuseBody = `{"error":"invalid_client","error_description":"` + sentinel + `"}`
	})
	grant := refreshableGrant(fx, "refresh-1")

	_, err := fx.eng.refresh(context.Background(), grant)
	if err == nil {
		t.Fatal("refresh succeeded against a refusing endpoint")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("the refresh error repeated the endpoint's body: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("the refresh error lost the protocol code: %v", err)
	}

	// A body that is not a token answer at all is refused without quoting it.
	fx.oauth.set(func(f *fakeOAuthServer) { f.tokenRefuseBody = sentinel })
	if _, err := fx.eng.refresh(context.Background(), grant); err == nil {
		t.Fatal("refresh accepted a body that is not a token answer")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("the refresh error repeated an undecodable body: %v", err)
	}
}

func TestMcpOAuthRefreshRedactsURLCredentials(t *testing.T) {
	const (
		queryToken = "query-token-2f4a"
		password   = "hunter2-in-the-userinfo"
	)
	secretFree := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), queryToken) {
			t.Fatalf("a token in the token endpoint's query reached the error: %v", err)
		}
		if strings.Contains(err.Error(), password) {
			t.Fatalf("a password in the token endpoint's userinfo reached the error: %v", err)
		}
	}

	t.Run("endpoint refused by the policy", func(t *testing.T) {
		fx := newEngineFixture(t)
		grant := refreshableGrant(fx, "refresh-1")
		grant.Registration.TokenEndpoint = "https://user:" + password + "@as.example/token?access_token=" + queryToken

		_, err := fx.eng.refresh(context.Background(), grant)
		// A credentialed URL is refused by the shape check (ErrInvalidURL) before the
		// destination check ever runs; either is a policy refusal.
		if !errors.Is(err, ErrDestinationDenied) && !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("refresh = %v, want a URL-policy refusal", err)
		}
		secretFree(t, err)
		if got := fx.oauth.tokenCallCount(); got != 0 {
			t.Fatalf("the refresh reached %d token endpoints", got)
		}
	})

	t.Run("endpoint that cannot be reached", func(t *testing.T) {
		fx := newEngineFixture(t)
		grant := refreshableGrant(fx, "refresh-1")
		// Loopback is allowed by this policy, so the refusal comes from the dial and
		// arrives as a *url.Error quoting the whole URL, query included.
		grant.Registration.TokenEndpoint = "https://127.0.0.1:1/token?access_token=" + queryToken

		_, err := fx.eng.refresh(context.Background(), grant)
		secretFree(t, err)
	})

	t.Run("a request that cannot be built", func(t *testing.T) {
		// The policy refuses a malformed endpoint before a request is built, so this
		// branch is reached directly: http.NewRequest quotes the URL it was given.
		fx := newEngineFixture(t)
		registration := Registration{
			ClientID:      "client-1",
			AuthStyle:     AuthStyleInParams,
			TokenEndpoint: "https://user:" + password + "@mcp.example/%zz?access_token=" + queryToken,
		}
		_, err := fx.eng.tokenRequest(context.Background(), registration.TokenEndpoint, registration,
			url.Values{"grant_type": {"refresh_token"}}, clientAuthParams)
		secretFree(t, err)
	})

	t.Run("endpoint that is not a usable URL", func(t *testing.T) {
		fx := newEngineFixture(t)
		grant := refreshableGrant(fx, "refresh-1")
		grant.Registration.TokenEndpoint = "https://127.0.0.1:1/token?access_token=" + queryToken + "\n"

		_, err := fx.eng.refresh(context.Background(), grant)
		secretFree(t, err)
	})
}

// The style a refresh uses is decided once, at login, from the authorization
// server's own advertised methods — so a confidential client whose issuer only
// accepts body credentials can refresh, and no refresh ever has to try a second
// placement (which would spend the single-use rotation twice).
func TestMcpOAuthRefreshUsesTheStyleResolvedAtLogin(t *testing.T) {
	cases := []struct {
		name       string
		placement  string
		advertised []string
		want       AuthStyle
		wantBasic  bool
	}{
		{
			name:       "issuer that offers only client_secret_basic",
			placement:  "basic",
			advertised: []string{"client_secret_basic"},
			want:       AuthStyleInHeader,
			wantBasic:  true,
		},
		{
			name:       "issuer that offers only client_secret_post",
			placement:  "post",
			advertised: []string{"client_secret_post"},
			want:       AuthStyleInParams,
			wantBasic:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEngineFixture(t)
			fx.oauth.set(func(f *fakeOAuthServer) {
				f.tokenAuthMethods = tc.advertised
				// A confidential registration that names no method: the SDK falls back
				// to its own default, so the login has to resolve the style itself.
				f.dcrClientSecret = "confidential-" + tc.placement
				f.refreshAuthPlacement = tc.placement
			})

			d := fx.begin()
			fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
			grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
			if !ok {
				t.Fatal("the login committed no grant")
			}
			if grant.Registration.ClientSecret == "" {
				t.Fatal("the registration issued no secret, so this is not the confidential case")
			}
			if got := grant.Registration.AuthStyle; got != tc.want {
				t.Fatalf("stored auth style = %q, want %q (resolved from the issuer's advertised methods)", got, tc.want)
			}

			// The endpoint refuses any placement but its own, so this only succeeds if
			// the stored style is the one the server accepts — and it must do so in one
			// request, because a second attempt would re-send a spent rotation.
			before := fx.oauth.tokenCallCount()
			if _, err := fx.eng.refresh(context.Background(), grant); err != nil {
				t.Fatalf("refresh = %v, want the style resolved at login to work", err)
			}
			if got := fx.oauth.tokenCallCount() - before; got != 1 {
				t.Fatalf("token requests = %d, want exactly one", got)
			}
			if fx.oauth.lastTokenHadBasicAuth != tc.wantBasic {
				t.Fatalf("basic auth = %v, want %v", fx.oauth.lastTokenHadBasicAuth, tc.wantBasic)
			}

			// And through the path a session actually takes: an expired access token
			// makes Store.Acquire call the engine's refresh with the STORED style, so
			// the credential continues to work without a new login.
			fx.store.mu.Lock()
			stored := fx.store.grants[fakeKey(fx.owner, fx.oauth.endpoint())]
			stored.Token.Expiry = time.Now().Add(-time.Minute)
			fx.store.mu.Unlock()

			handler, _, err := fx.eng.Passive(fx.owner)(context.Background(), "fixture", fx.oauth.endpoint(), nil)
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			ts, err := handler.TokenSource(context.Background())
			if err != nil || ts == nil {
				t.Fatalf("TokenSource = %v, %v", ts, err)
			}
			before = fx.oauth.tokenCallCount()
			tok, err := ts.Token()
			if err != nil {
				t.Fatalf("acquisition after expiry = %v, want the stored style to refresh", err)
			}
			if tok.AccessToken == "" {
				t.Fatal("acquisition returned no access token")
			}
			if got := fx.oauth.tokenCallCount() - before; got != 1 {
				t.Fatalf("token requests = %d, want exactly one", got)
			}
		})
	}
}

func TestMcpOAuthRefreshStyleResolutionPrefersTheIssuersMethods(t *testing.T) {
	cases := []struct {
		name       string
		secret     string
		sdkStyle   oauth2.AuthStyle
		advertised []string
		want       AuthStyle
	}{
		{"public client is always params", "", oauth2.AuthStyleInHeader, []string{"client_secret_basic"}, AuthStyleInParams},
		{"basic offered wins", "s", oauth2.AuthStyleInParams, []string{"client_secret_basic", "client_secret_post"}, AuthStyleInHeader},
		{"post only is honoured", "s", oauth2.AuthStyleInHeader, []string{"client_secret_post"}, AuthStyleInParams},
		{"unadvertised keeps the SDK style", "s", oauth2.AuthStyleInParams, nil, AuthStyleInParams},
		{"unadvertised auto falls to basic", "s", oauth2.AuthStyleAutoDetect, nil, AuthStyleInHeader},
		{"an unusable method list keeps the SDK style", "s", oauth2.AuthStyleInHeader, []string{"private_key_jwt"}, AuthStyleInHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAuthStyle(tc.secret, tc.sdkStyle, tc.advertised); got != tc.want {
				t.Fatalf("resolveAuthStyle = %q, want %q", got, tc.want)
			}
		})
	}
}
