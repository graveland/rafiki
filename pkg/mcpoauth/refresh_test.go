// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
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
