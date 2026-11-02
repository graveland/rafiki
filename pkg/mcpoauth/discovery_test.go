// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The properties here are the ones the SDK seam makes easy to get wrong: which
// metadata response became the binding, that no metadata was fetched twice, and
// that an operator's destination policy cannot be widened by a document.

func TestMcpOAuthDiscoveryCandidatesMirrorTheSDKOrder(t *testing.T) {
	got, err := prmCandidates("https://as.example/.well-known/oauth-protected-resource", "https://mcp.example/mcp")
	if err != nil {
		t.Fatalf("prmCandidates: %v", err)
	}
	want := []prmCandidate{
		{URL: "https://as.example/.well-known/oauth-protected-resource", Resource: "https://mcp.example/mcp"},
		{URL: "https://mcp.example/.well-known/oauth-protected-resource/mcp", Resource: "https://mcp.example/mcp"},
		{URL: "https://mcp.example/.well-known/oauth-protected-resource", Resource: "https://mcp.example"},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// No challenge metadata URL means the two well-known locations only.
	got, err = prmCandidates("", "https://mcp.example/mcp")
	if err != nil {
		t.Fatalf("prmCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates without a challenge URL = %+v, want two", got)
	}
}

func TestMcpOAuthDiscoveryRejectsMetadataForAnotherResource(t *testing.T) {
	fx := newEngineFixture(t)
	// The path location answers with a metadata document about a DIFFERENT
	// resource: RFC 9728 section 3.3 says it is not metadata for this endpoint,
	// so it must not become the binding.
	fx.oauth.set(func(f *fakeOAuthServer) { f.prmResourcePath = "https://somewhere-else.invalid/mcp" })

	d := fx.begin()
	res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if res.Continuation != nil {
		t.Fatal("unexpected continuation")
	}
	// What actually validated is the root document, which declares the root as
	// its resource.
	root, err := Canonicalize(fx.oauth.rootResource())
	if err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	if got, want := res.Resource, root; got != want {
		t.Fatalf("bound resource = %q, want the root %q", got, want)
	}
	if got := res.Resource; strings.Contains(got, "somewhere-else") {
		t.Fatalf("a rejected metadata document became the binding: %q", got)
	}
	grant, ok := fx.store.grant(fx.owner, root)
	if !ok {
		t.Fatal("no grant for the validated resource")
	}
	if strings.Contains(grant.Resource, "/mcp") {
		t.Fatalf("grant resource = %q, want the root spelling", grant.Resource)
	}
}

func TestMcpOAuthDiscoveryHonoursTheRootWellKnownResource(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.servePRMPath = false })

	d := fx.begin()
	authURL := d.AuthorizeURL
	if !strings.Contains(authURL, "resource="+url.QueryEscape(fx.oauth.rootResource())) {
		t.Fatalf("authorize URL %q does not carry the root resource", authURL)
	}
	root, err := Canonicalize(fx.oauth.rootResource())
	if err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	res := fx.mustComplete(d.LoginID, fx.oauth.callback(authURL))
	if got, want := res.Resource, root; got != want {
		t.Fatalf("resource = %q, want the root %q", got, want)
	}
	if hit := fx.oauth.prmRequestCount("/.well-known/oauth-protected-resource"); hit != 1 {
		t.Fatalf("root metadata hits = %d, want 1", hit)
	}
}

func TestMcpOAuthDiscoveryFallsBackToTheServerRootWithoutMetadata(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.servePRMPath = false
		f.servePRMRoot = false
	})

	d := fx.begin()
	// The 2025-03-26 fallback: the resource is the MCP endpoint itself and the
	// root is treated as the authorization server.
	res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if got, want := res.Resource, fx.oauth.endpoint(); got != want {
		t.Fatalf("resource = %q, want the endpoint %q", got, want)
	}
	grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("no grant")
	}
	if got, want := grant.Issuer, fx.oauth.rootResource(); got != want {
		t.Fatalf("issuer = %q, want the server root %q", got, want)
	}
	// Both documented locations really were tried; the fallback rests on that.
	// The challenge's resource_metadata URL and the path location are the same
	// URL here, so the SDK asks for it twice.
	if got := fx.oauth.prmRequestCount("/.well-known/oauth-protected-resource/mcp"); got < 1 {
		t.Fatalf("path metadata hits = %d, want at least one attempt", got)
	}
	if got := fx.oauth.prmRequestCount("/.well-known/oauth-protected-resource"); got != 1 {
		t.Fatalf("root metadata hits = %d, want 1", got)
	}
}

func TestMcpOAuthDiscoveryFallsBackToPredefinedEndpointsWithoutASMetadata(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.serveASM = false })

	d := fx.begin()
	res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if got, want := res.Resource, fx.oauth.endpoint(); got != want {
		t.Fatalf("resource = %q, want %q", got, want)
	}
	grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("no grant")
	}
	// The fallback endpoints are the issuer plus the conventional paths.
	if got, want := grant.Registration.AuthorizationEndpoint, fx.oauth.issuer()+"/authorize"; got != want {
		t.Fatalf("authorization endpoint = %q, want %q", got, want)
	}
	if got, want := grant.Registration.TokenEndpoint, fx.oauth.issuer()+"/token"; got != want {
		t.Fatalf("token endpoint = %q, want %q", got, want)
	}
	// No advertised revocation endpoint, so none is invented.
	if grant.Registration.RevocationEndpoint != "" {
		t.Fatalf("revocation endpoint = %q, want none", grant.Registration.RevocationEndpoint)
	}
}

func TestMcpOAuthDiscoveryFetchesMetadataOnlyOnce(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()
	fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))

	// Replaying the SDK's own validation must not cost a single request beyond
	// the ones the SDK itself made.
	if got := fx.oauth.prmRequestCount("/.well-known/oauth-protected-resource/mcp"); got != 1 {
		t.Fatalf("path metadata hits = %d, want 1", got)
	}
	if got := fx.oauth.prmRequestCount("/.well-known/oauth-protected-resource"); got > 1 {
		t.Fatalf("root metadata hits = %d, want at most 1", got)
	}
	if got := fx.oauth.asmHitCount(); got != 1 {
		t.Fatalf("authorization server metadata hits = %d, want 1", got)
	}
	gets := 0
	for _, req := range fx.oauth.requestLog() {
		if strings.HasPrefix(req, "GET ") {
			gets++
		}
	}
	if gets > 3 {
		t.Fatalf("the login made %d metadata GETs: %v", gets, fx.oauth.requestLog())
	}
}

func TestMcpOAuthDiscoveryReplayServesOnlyRecordedBytes(t *testing.T) {
	ctx := context.Background()
	recorder := newRecordRequest(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/one":
			body = `{"resource":"https://mcp.example/mcp"}`
		case "/two":
			body = `{"error":"nope"}`
		default:
			body = `{}`
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}), ctx)

	for _, path := range []string{"/one", "/two"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example"+path, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp, err := recorder.RoundTrip(req)
		if err != nil {
			t.Fatalf("record %s: %v", path, err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	entries := recorder.since(0)
	if len(entries) != 2 {
		t.Fatalf("recorded %d responses, want 2", len(entries))
	}
	client := newReplayClient(nil, entries)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example/one", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("replay of a recorded URL: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("replay body: %v", err)
	}
	_ = resp.Body.Close()
	if got, want := string(body), `{"resource":"https://mcp.example/mcp"}`; got != want {
		t.Fatalf("replayed body = %q, want the recorded %q", got, want)
	}

	// The same URL is never served twice: replaying an observation is not
	// re-fetching it, and a second answer would not be the bytes the SDK saw.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example/one", nil)
	if _, err := client.Do(req); !errors.Is(err, errorReplayUnavailable) {
		t.Fatalf("second replay = %v, want errorReplayUnavailable", err)
	}
	// An unrecorded URL is an error, never a request.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example/three", nil)
	if _, err := client.Do(req); !errors.Is(err, errorReplayUnavailable) {
		t.Fatalf("unrecorded replay = %v, want errorReplayUnavailable", err)
	}
}

func TestMcpOAuthDiscoveryHonoursTheDestinationPolicy(t *testing.T) {
	// An operator policy with no allowances covers public HTTPS only.
	strict := testPolicy(t, Config{}, nil, nil)
	cases := []struct {
		name string
		disc validatedDiscovery
	}{
		{
			name: "resource",
			disc: validatedDiscovery{
				RawResource:           "http://127.0.0.1/mcp",
				Issuer:                "https://as.example",
				AuthorizationEndpoint: "https://as.example/authorize",
				TokenEndpoint:         "https://as.example/token",
			},
		},
		{
			name: "issuer",
			disc: validatedDiscovery{
				RawResource:           "https://mcp.example/mcp",
				Issuer:                "http://10.0.0.5",
				AuthorizationEndpoint: "https://as.example/authorize",
				TokenEndpoint:         "https://as.example/token",
			},
		},
		{
			name: "authorization endpoint",
			disc: validatedDiscovery{
				RawResource:           "https://mcp.example/mcp",
				Issuer:                "https://as.example",
				AuthorizationEndpoint: "http://127.0.0.1/authorize",
				TokenEndpoint:         "https://as.example/token",
			},
		},
		{
			name: "revocation endpoint",
			disc: validatedDiscovery{
				RawResource:           "https://mcp.example/mcp",
				Issuer:                "https://as.example",
				AuthorizationEndpoint: "https://as.example/authorize",
				TokenEndpoint:         "https://as.example/token",
				RevocationEndpoint:    "http://10.1.2.3/revoke",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDiscovered(context.Background(), strict, tc.disc)
			if !errors.Is(err, ErrDestinationDenied) && !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("validateDiscovered = %v, want a destination refusal", err)
			}
		})
	}
}

func TestMcpOAuthDiscoveryRefusesMetadataWithoutAnIssuer(t *testing.T) {
	fx := newEngineFixture(t)
	// A metadata document with an empty first authorization server cannot
	// identify an issuer, so no consent URL may be published for it.
	fx.oauth.set(func(f *fakeOAuthServer) { f.emptyFirstAuthorizationServer = true })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpoint()})
	if err == nil {
		t.Fatal("Begin published a consent URL for metadata with no issuer")
	}
	if !strings.Contains(err.Error(), "authorization server") {
		t.Fatalf("error = %v, want it to name the missing authorization server", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
