// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"fmt"
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

	// Replaying the SDK's own validation must not cost a single request beyond the
	// ones the SDK itself made, counted per URL: a total leaves room for one URL to
	// be fetched twice while another is skipped, which is exactly the failure this
	// pins.
	fx.oauth.mu.Lock()
	prmPathHits := fx.oauth.prmHits["/.well-known/oauth-protected-resource/mcp"]
	prmRootHits := fx.oauth.prmHits["/.well-known/oauth-protected-resource"]
	asmOAuthHits := fx.oauth.asmHits["/.well-known/oauth-authorization-server"]
	asmOIDCHits := fx.oauth.asmHits["/.well-known/openid-configuration"]
	fx.oauth.mu.Unlock()
	if prmPathHits != 1 {
		t.Fatalf("path metadata hits = %d, want 1", prmPathHits)
	}
	if prmRootHits != 0 {
		t.Fatalf("root metadata hits = %d, want 0 (the path location answered)", prmRootHits)
	}
	if asmOAuthHits != 1 {
		t.Fatalf("authorization server metadata hits = %d, want 1", asmOAuthHits)
	}
	if asmOIDCHits != 0 {
		t.Fatalf("OIDC discovery hits = %d, want 0", asmOIDCHits)
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

	entries, err := recorder.since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
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
	// The failure is authored and coarse: the step, never the SDK's own text about
	// the document it could not use.
	if !strings.Contains(err.Error(), "authenticating to") {
		t.Fatalf("error = %v, want an authored step description", err)
	}
	if strings.Contains(err.Error(), "authorization server metadata") {
		t.Fatalf("error repeats the SDK's own text: %v", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// scriptedExchange is one response the SDK is pretended to have been handed.
type scriptedExchange struct {
	method string
	url    string
	status int
	header http.Header
	body   string
}

// recordScript records a scripted set of responses, in order, and returns the
// recorder holding them. It is how a test states exactly what the SDK saw,
// without a live server in the way.
func recordScript(t *testing.T, ctx context.Context, script []scriptedExchange) *recordRequest {
	t.Helper()
	queue := map[string][]scriptedExchange{}
	for _, ex := range script {
		key := ex.method + " " + ex.url
		queue[key] = append(queue[key], ex)
	}
	rec := newRecordRequest(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		key := req.Method + " " + req.URL.String()
		pending := queue[key]
		if len(pending) == 0 {
			return nil, fmt.Errorf("unscripted request %s", key)
		}
		ex := pending[0]
		queue[key] = pending[1:]
		header := ex.header
		if header == nil {
			header = http.Header{"Content-Type": []string{"application/json"}}
		}
		return &http.Response{
			StatusCode: ex.status,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(ex.body)),
			Request:    req,
		}, nil
	}), ctx)

	for _, ex := range script {
		req, err := http.NewRequestWithContext(ctx, ex.method, ex.url, nil)
		if err != nil {
			t.Fatalf("scripted request: %v", err)
		}
		resp, err := rec.RoundTrip(req)
		if err != nil {
			t.Fatalf("recording %s: %v", ex.method+" "+ex.url, err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("reading %s: %v", ex.method+" "+ex.url, err)
		}
		_ = resp.Body.Close()
	}
	return rec
}

const scriptedASM = `{"issuer":"https://as.example","authorization_endpoint":"https://as.example/authorize",` +
	`"token_endpoint":"https://as.example/token","code_challenge_methods_supported":["S256"]}`

// publicPolicy resolves the scripted metadata hosts to public addresses, so the
// destination gate is exercised rather than bypassed.
func publicPolicy(t *testing.T) *Policy {
	t.Helper()
	resolver := newResolver().
		answer("mcp.example", "93.184.216.34").
		answer("as.example", "93.184.216.35").
		answer("other.example", "93.184.216.36")
	return testPolicy(t, Config{}, resolver, nil)
}

func TestMcpOAuthDiscoveryRequiresTheResourceTheSDKRequested(t *testing.T) {
	ctx := context.Background()
	policy := publicPolicy(t)

	script := func() []scriptedExchange {
		return []scriptedExchange{
			{
				method: "POST", url: "https://mcp.example/mcp", status: http.StatusUnauthorized,
				header: http.Header{"Www-Authenticate": []string{
					`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`,
				}},
			},
			{
				method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource/mcp",
				status: http.StatusOK,
				body:   `{"resource":"https://mcp.example/mcp","authorization_servers":["https://as.example"]}`,
			},
			{method: "GET", url: "https://as.example/.well-known/oauth-authorization-server", status: http.StatusOK, body: scriptedASM},
		}
	}

	t.Run("the resource the SDK asked for", func(t *testing.T) {
		rec := recordScript(t, ctx, script())
		got, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         "https://mcp.example/mcp",
			AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape("https://mcp.example/mcp"),
			Recorder:         rec,
			Policy:           policy,
		})
		if err != nil {
			t.Fatalf("captureDiscovery: %v", err)
		}
		if got.RawResource != "https://mcp.example/mcp" {
			t.Fatalf("raw resource = %q", got.RawResource)
		}
		if got.Issuer != "https://as.example" {
			t.Fatalf("issuer = %q", got.Issuer)
		}
	})

	t.Run("a different resource", func(t *testing.T) {
		// The mirror's replay accepts this document (it is valid metadata for the
		// endpoint), but the SDK requested authorization for another resource. That
		// divergence is the whole reason the capture exists: it must refuse.
		rec := recordScript(t, ctx, script())
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         "https://mcp.example/mcp",
			AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape("https://other.example/resource"),
			Recorder:         rec,
			Policy:           policy,
		})
		if err == nil {
			t.Fatal("captureDiscovery bound metadata for a resource the SDK never requested")
		}
		if !strings.Contains(err.Error(), "other.example") {
			t.Fatalf("error = %v, want it to name the resource the SDK asked for", err)
		}
	})
}

func TestMcpOAuthDiscoveryRefusesADuplicateRecording(t *testing.T) {
	ctx := context.Background()
	// One URL, two different answers inside one round: the recorder cannot say
	// which one the SDK read.
	rec := recordScript(t, ctx, []scriptedExchange{
		{method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource", status: http.StatusOK,
			body: `{"resource":"https://mcp.example","authorization_servers":["https://as.example"]}`},
		{method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource", status: http.StatusOK,
			body: `{"resource":"https://mcp.example/mcp","authorization_servers":["https://as.example"]}`},
	})

	entries, serr := rec.since(0)
	if serr != nil {
		t.Fatalf("since: %v", serr)
	}
	client := newReplayClient(nil, entries)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example/.well-known/oauth-protected-resource", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("replay picked one of two recorded responses for the same URL")
	}
	if !errors.Is(err, errorReplayAmbiguous) {
		t.Fatalf("replay = %v, want errorReplayAmbiguous", err)
	}
}

func TestMcpOAuthDiscoveryRefusesAVaryingDuplicateRecording(t *testing.T) {
	fx := newEngineFixture(t)
	// The challenge's resource_metadata URL and the path well-known URL are the
	// same URL here. The first answer describes the ROOT resource (which the SDK
	// rejects for that candidate) and the second describes the endpoint (which it
	// accepts). One URL, two answers, one round: nothing about the round can be
	// replayed, so no binding may be made.
	// rootResource() takes the fixture's lock, so it must be read outside set().
	rootResource := fx.oauth.rootResource()
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.prmVaryOnSecondHit = true
		f.prmResourcePath = rootResource
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpoint()})
	if err == nil {
		t.Fatal("Begin published a consent URL from an ambiguous recording")
	}
	if !strings.Contains(err.Error(), "different answers") && !strings.Contains(err.Error(), "cannot be replayed") {
		t.Fatalf("error = %v, want it to name the duplicate recording", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was bound from an ambiguous recording")
	}
	if got := fx.oauth.prmRequestCount("/.well-known/oauth-protected-resource/mcp"); got < 2 {
		t.Fatalf("the fixture only answered %d times; the duplicate was not exercised", got)
	}
}

func TestMcpOAuthDiscoveryReplaysAnIdenticalDuplicate(t *testing.T) {
	ctx := context.Background()
	// The same URL answered twice with the same bytes is not ambiguous: either
	// recording is what the SDK read.
	rec := recordScript(t, ctx, []scriptedExchange{
		{method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource", status: http.StatusOK,
			body: `{"resource":"https://mcp.example","authorization_servers":["https://as.example"]}`},
		{method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource", status: http.StatusOK,
			body: `{"resource":"https://mcp.example","authorization_servers":["https://as.example"]}`},
	})

	entries, serr := rec.since(0)
	if serr != nil {
		t.Fatalf("since: %v", serr)
	}
	client := newReplayClient(nil, entries)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example/.well-known/oauth-protected-resource", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("replay of an identical duplicate: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `"https://mcp.example"`) {
		t.Fatalf("replayed body = %q", body)
	}
}

func TestMcpOAuthDiscoveryRedactsURLCredentials(t *testing.T) {
	const (
		password   = "hunter2-in-the-userinfo"
		queryToken = "query-token-8c31"
	)
	credentialed := "https://user:" + password + "@mcp.example/mcp?access_token=" + queryToken
	secretFree := func(t *testing.T, hay string) {
		t.Helper()
		if strings.Contains(hay, password) {
			t.Fatalf("a password in the URL reached the message: %s", hay)
		}
		if strings.Contains(hay, queryToken) {
			t.Fatalf("a token in the URL query reached the message: %s", hay)
		}
	}

	ctx := context.Background()
	policy := publicPolicy(t)

	t.Run("a consent capture whose resource carries a query token", func(t *testing.T) {
		// A resource keeps its query through canonicalisation, so a token can live
		// there. The endpoint here is the resource, and the SDK asked for another
		// one: the cross-check prints both, and neither may carry the credential.
		endpoint := "https://mcp.example/mcp?access_token=" + queryToken
		rec := recordScript(t, ctx, []scriptedExchange{
			{
				method: "POST", url: endpoint, status: http.StatusUnauthorized,
				header: http.Header{"Www-Authenticate": []string{
					`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`,
				}},
			},
			{
				method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource/mcp",
				status: http.StatusOK,
				body:   `{"resource":"` + endpoint + `","authorization_servers":["https://as.example"]}`,
			},
			{method: "GET", url: "https://as.example/.well-known/oauth-authorization-server", status: http.StatusOK, body: scriptedASM},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         endpoint,
			AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape("https://other.example/resource"),
			Recorder:         rec,
			Policy:           policy,
		})
		if err == nil {
			t.Fatal("captureDiscovery accepted metadata for the wrong resource")
		}
		secretFree(t, err.Error())
		if !strings.Contains(err.Error(), "mcp.example") {
			t.Fatalf("the message lost the endpoint it is about: %v", err)
		}
	})

	t.Run("an authorization server URL carrying a query token", func(t *testing.T) {
		// Metadata may name an authorization server whose URL carries a credential.
		// The SDK's own scheme check refuses neither userinfo nor a query, so this
		// engine's message is the only thing that can redact it.
		issuer := "https://as.example?access_token=" + queryToken
		rec := recordScript(t, ctx, []scriptedExchange{
			{
				method: "POST", url: "https://mcp.example/mcp", status: http.StatusUnauthorized,
				header: http.Header{"Www-Authenticate": []string{
					`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`,
				}},
			},
			{
				method: "GET", url: "https://mcp.example/.well-known/oauth-protected-resource/mcp",
				status: http.StatusOK,
				body:   `{"resource":"https://mcp.example/mcp","authorization_servers":["` + issuer + `"]}`,
			},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         "https://mcp.example/mcp",
			AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape("https://mcp.example/mcp"),
			Recorder:         rec,
			Policy:           policy,
		})
		if err == nil {
			t.Fatal("captureDiscovery accepted an authorization server it could not verify")
		}
		secretFree(t, err.Error())
	})

	t.Run("a duplicated recording of a URL with a query token", func(t *testing.T) {
		// The ambiguity error names the request it could not resolve, and that
		// request is a real URL — with the endpoint's query token on it.
		endpoint := "https://mcp.example/mcp?access_token=" + queryToken
		prmURL := "https://mcp.example/.well-known/oauth-protected-resource/mcp?access_token=" + queryToken
		rec := recordScript(t, ctx, []scriptedExchange{
			{
				method: "POST", url: endpoint, status: http.StatusUnauthorized,
				header: http.Header{"Www-Authenticate": []string{
					`Bearer resource_metadata="` + prmURL + `"`,
				}},
			},
			{method: "GET", url: prmURL, status: http.StatusOK, body: `{"resource":"` + endpoint + `","authorization_servers":["https://as.example"]}`},
			{method: "GET", url: prmURL, status: http.StatusForbidden, body: `{"resource":"` + endpoint + `"}`},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         endpoint,
			AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape(endpoint),
			Recorder:         rec,
			Policy:           policy,
		})
		if err == nil {
			t.Fatal("captureDiscovery accepted a round with two answers for one request")
		}
		if !errors.Is(err, errorReplayAmbiguous) {
			t.Fatalf("error = %v, want errorReplayAmbiguous", err)
		}
		secretFree(t, err.Error())
	})

	t.Run("the replay transport's own refusal", func(t *testing.T) {
		// The transport's errors carry the sentinel a caller branches on, so a
		// caller may wrap one: its message must be redacted even in isolation.
		target := "https://mcp.example/.well-known/oauth-protected-resource?access_token=" + queryToken
		rec := recordScript(t, ctx, []scriptedExchange{
			{method: "GET", url: target, status: http.StatusOK, body: `{"resource":"https://mcp.example"}`},
			{method: "GET", url: target, status: http.StatusForbidden, body: `{}`},
		})
		entries, serr := rec.since(0)
		if serr != nil {
			t.Fatalf("since: %v", serr)
		}
		transport := newReplayClient(nil, entries).Transport
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		// The transport directly, not through http.Client: net/http wraps any
		// transport error in a *url.Error that quotes the whole URL, and that wrapper
		// is not this package's to redact. What must hold is that OUR message — the
		// one a caller's errors.Is/errors.As sees — carries no credential.
		resp, err := transport.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("replay resolved an ambiguous recording")
		}
		secretFree(t, err.Error())
		if !errors.Is(err, errorReplayAmbiguous) {
			t.Fatalf("replay = %v, want errorReplayAmbiguous", err)
		}
		if !strings.Contains(err.Error(), "mcp.example") {
			t.Fatalf("the refusal lost the request it is about: %v", err)
		}
	})

	t.Run("an unparsable consent URL", func(t *testing.T) {
		// The consent URL arrives from the SDK; one that does not parse must not be
		// quoted back, because it carries the PKCE challenge and the state. url.Parse
		// reports a control character with the whole URL in its error.
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         "https://mcp.example/mcp",
			AuthorizationURL: "https://as.example/authorize?code_challenge=" + queryToken + "\x00",
			Recorder:         recordScript(t, ctx, nil),
			Policy:           policy,
		})
		if err == nil {
			t.Fatal("captureDiscovery accepted an unparsable consent URL")
		}
		secretFree(t, err.Error())
	})

	t.Run("a discovered endpoint with userinfo", func(t *testing.T) {
		// validateDiscovered names each target it refuses. url.go redacts the parse
		// error it wraps, and this message must not put the raw URL back beside it.
		err := validateDiscovered(ctx, policy, validatedDiscovery{
			RawResource:           "https://mcp.example/mcp",
			Issuer:                "https://as.example",
			AuthorizationEndpoint: "https://as.example/authorize",
			TokenEndpoint:         credentialed,
		})
		if err == nil {
			t.Fatal("validateDiscovered accepted a token endpoint with userinfo")
		}
		secretFree(t, err.Error())
	})
}

// TestMcpOAuthDiscoveryRedactsEachMessage walks the discovery messages that can
// name a URL, one at a time.
//
// A configured MCP endpoint may carry a credential in its query — canonicalisation
// keeps the query on purpose — and every candidate URL is derived from it. So each
// of these messages is a place the token would be repeated, and each is asserted
// separately: a single test that only exercised one of them would leave the rest
// free to regress.
func TestMcpOAuthDiscoveryRedactsEachMessage(t *testing.T) {
	const (
		password   = "message-password-1a2b"
		queryToken = "message-token-3c4d"
	)
	secretFree := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), queryToken) {
			t.Fatalf("a URL credential reached the message: %v", err)
		}
	}
	ctx := context.Background()
	policy := publicPolicy(t)
	endpoint := "https://mcp.example/mcp?access_token=" + queryToken
	prmPath := "https://mcp.example/.well-known/oauth-protected-resource/mcp?access_token=" + queryToken
	prmRoot := "https://mcp.example/.well-known/oauth-protected-resource?access_token=" + queryToken
	challenge := func(url string) scriptedExchange {
		return scriptedExchange{
			method: "POST", url: endpoint, status: http.StatusUnauthorized,
			header: http.Header{"Www-Authenticate": []string{`Bearer resource_metadata="` + url + `"`}},
		}
	}
	okAuthURL := "https://as.example/authorize?resource=" + url.QueryEscape(endpoint)

	t.Run("an endpoint that is not a URL", func(t *testing.T) {
		_, err := prmCandidates("", "https://user:"+password+"@mcp.example/%zz?access_token="+queryToken)
		secretFree(t, err)
	})

	t.Run("a round with no authentication challenge", func(t *testing.T) {
		// The recorded 401 is for another endpoint, so this round has no challenge to
		// read and the message names the endpoint it never found one for.
		rec := recordScript(t, ctx, []scriptedExchange{
			{method: "POST", url: "https://mcp.example/elsewhere", status: http.StatusUnauthorized},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint: endpoint, AuthorizationURL: okAuthURL, Recorder: rec, Policy: policy,
		})
		secretFree(t, err)
		if !strings.Contains(err.Error(), "no authentication challenge") {
			t.Fatalf("error = %v, want the missing-challenge refusal", err)
		}
	})

	t.Run("metadata with no authorization servers", func(t *testing.T) {
		rec := recordScript(t, ctx, []scriptedExchange{
			challenge(prmPath),
			{method: "GET", url: prmPath, status: http.StatusOK, body: `{"resource":"` + endpoint + `"}`},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint: endpoint, AuthorizationURL: okAuthURL, Recorder: rec, Policy: policy,
		})
		secretFree(t, err)
		if !strings.Contains(err.Error(), "no authorization servers") {
			t.Fatalf("error = %v, want the empty-authorization-servers refusal", err)
		}
	})

	t.Run("a fallback that the recordings do not establish", func(t *testing.T) {
		// Only the challenge was recorded: neither well-known location was fetched,
		// so the fallback cannot be claimed and the message names the candidate that
		// is missing — a URL derived from the credentialed endpoint.
		rec := recordScript(t, ctx, []scriptedExchange{challenge(prmPath)})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint: endpoint, AuthorizationURL: okAuthURL, Recorder: rec, Policy: policy,
		})
		secretFree(t, err)
		if !strings.Contains(err.Error(), "fallback is not established") {
			t.Fatalf("error = %v, want the unestablished-fallback refusal", err)
		}
	})

	t.Run("a fallback for a resource nobody described", func(t *testing.T) {
		// Both locations answered 404, so the fallback applies — but the SDK asked
		// for a resource that is not this endpoint, and that resource is named.
		other := "https://other.example/resource?access_token=" + queryToken
		rec := recordScript(t, ctx, []scriptedExchange{
			challenge(prmPath),
			{method: "GET", url: prmPath, status: http.StatusNotFound},
			{method: "GET", url: prmRoot, status: http.StatusNotFound},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint:         endpoint,
			AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape(other),
			Recorder:         rec,
			Policy:           policy,
		})
		secretFree(t, err)
		if !strings.Contains(err.Error(), "no validated metadata describes") {
			t.Fatalf("error = %v, want the undescribed-resource refusal", err)
		}
	})

	t.Run("metadata whose first authorization server is empty", func(t *testing.T) {
		// The SDK's scheme check lets an empty string through, so a document can name
		// an issuer-less resource. This refusal is the only thing that stops it — and
		// it names the resource, which is the credentialed endpoint.
		rec := recordScript(t, ctx, []scriptedExchange{
			challenge(prmPath),
			{method: "GET", url: prmPath, status: http.StatusOK,
				body: `{"resource":"` + endpoint + `","authorization_servers":[""]}`},
		})
		_, err := captureDiscovery(ctx, discoveryInput{
			Endpoint: endpoint, AuthorizationURL: okAuthURL, Recorder: rec, Policy: policy,
		})
		secretFree(t, err)
		if !strings.Contains(err.Error(), "names an empty authorization server") {
			t.Fatalf("error = %v, want the empty-authorization-server refusal", err)
		}
	})

	t.Run("an authorization server with no endpoints", func(t *testing.T) {
		issuer := "https://as.example?access_token=" + queryToken
		err := validateDiscovered(ctx, policy, validatedDiscovery{
			RawResource: "https://mcp.example/mcp",
			Issuer:      issuer,
		})
		secretFree(t, err)
		if !strings.Contains(err.Error(), "advertises no authorization or token endpoint") {
			t.Fatalf("error = %v, want the missing-endpoint refusal", err)
		}
	})
}

func TestMcpOAuthDiscoveryUsesTheChallengeThatDroveDiscovery(t *testing.T) {
	ctx := context.Background()
	endpoint := "https://mcp.example/mcp"
	challenge := func(status int, metadata string) scriptedExchange {
		return scriptedExchange{
			method: "POST", url: endpoint, status: status,
			header: http.Header{"Www-Authenticate": []string{`Bearer resource_metadata="` + metadata + `"`}},
		}
	}
	driving := "https://mcp.example/.well-known/oauth-protected-resource/mcp"
	decoy := "https://mcp.example/.well-known/oauth-protected-resource/decoy"

	// A 403 that is not an insufficient_scope challenge makes the SDK answer "retry"
	// and re-send; the 401 on the retry is the response whose metadata drove
	// discovery. The decoy must not be used.
	rec := recordScript(t, ctx, []scriptedExchange{challenge(http.StatusForbidden, decoy), challenge(http.StatusUnauthorized, driving)})
	entries, err := rec.since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	got, err := recordedChallengeMetadataURL(entries, endpoint)
	if err != nil {
		t.Fatalf("recordedChallengeMetadataURL: %v", err)
	}
	if got != driving {
		t.Fatalf("challenge metadata URL = %q, want the one that drove discovery %q", got, driving)
	}

	// The single-challenge case a step-up round has: unchanged.
	rec = recordScript(t, ctx, []scriptedExchange{challenge(http.StatusForbidden, decoy)})
	entries, err = rec.since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if got, err := recordedChallengeMetadataURL(entries, endpoint); err != nil || got != decoy {
		t.Fatalf("single challenge = %q, %v; want %q", got, err, decoy)
	}
}

func TestMcpOAuthDiscoveryRefusesToDeriveFromAnotherDialledSpelling(t *testing.T) {
	ctx := context.Background()
	endpoint := "https://mcp.example/mcp"
	// The candidates come from the canonical endpoint; the SDK's come from
	// req.URL.String(). A future change that dialled the configured spelling would
	// otherwise fail every login with "no authentication challenge", which blames the
	// server for our own mismatch.
	rec := recordScript(t, ctx, []scriptedExchange{{
		method: "POST", url: "https://MCP.example:443/mcp", status: http.StatusUnauthorized,
		header: http.Header{"Www-Authenticate": []string{`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`}},
	}})
	entries, err := rec.since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	_, err = recordedChallengeMetadataURL(entries, endpoint)
	if err == nil {
		t.Fatal("a differently spelled dial was accepted silently")
	}
	if !strings.Contains(err.Error(), "dialled") {
		t.Fatalf("error = %v, want the spelling mismatch", err)
	}
	if strings.Contains(err.Error(), "no authentication challenge") {
		t.Fatalf("the mismatch is reported as a missing challenge: %v", err)
	}
}

func TestMcpOAuthDiscoveryRefusesAnUnparsableChallenge(t *testing.T) {
	ctx := context.Background()
	endpoint := "https://mcp.example/mcp"
	// The challenge that drove discovery is the last one in the window. If it cannot
	// be parsed, the SDK's own Authorize fails outright and there is nothing to fall
	// back to.
	rec := recordScript(t, ctx, []scriptedExchange{
		{
			method: "POST", url: endpoint, status: http.StatusUnauthorized,
			header: http.Header{"Www-Authenticate": []string{`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`}},
		},
		{
			method: "POST", url: endpoint, status: http.StatusUnauthorized,
			header: http.Header{"Www-Authenticate": []string{"Bearer scope"}},
		},
	})
	entries, err := rec.since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if _, err := recordedChallengeMetadataURL(entries, endpoint); err == nil {
		t.Fatal("an unparsable challenge was skipped instead of failing the round")
	} else if !strings.Contains(err.Error(), "cannot be parsed") {
		t.Fatalf("error = %v, want the parse refusal", err)
	}

	// An earlier unparsable challenge is a different matter: the SDK answered it
	// with a retry, so the well-formed last one is what drove discovery.
	rec = recordScript(t, ctx, []scriptedExchange{
		{
			method: "POST", url: endpoint, status: http.StatusUnauthorized,
			header: http.Header{"Www-Authenticate": []string{"Bearer scope"}},
		},
		{
			method: "POST", url: endpoint, status: http.StatusUnauthorized,
			header: http.Header{"Www-Authenticate": []string{`Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`}},
		},
	})
	entries, err = rec.since(0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	got, err := recordedChallengeMetadataURL(entries, endpoint)
	if err != nil {
		t.Fatalf("recordedChallengeMetadataURL: %v", err)
	}
	if got != "https://mcp.example/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("challenge metadata URL = %q", got)
	}
}

func TestMcpOAuthDiscoveryValidatesTheRegistrationEndpoint(t *testing.T) {
	// DCR has already happened by the time metadata is captured, so this is a
	// post-hoc gate: metadata naming a registration endpoint outside the operator's
	// policy fails the login rather than being accepted as the round's truth.
	strict := testPolicy(t, Config{}, nil, nil)
	// IP literals so nothing here depends on a lookup: the registration endpoint is
	// the only target the policy refuses.
	err := validateDiscovered(context.Background(), strict, validatedDiscovery{
		RawResource:           "https://93.184.216.34/mcp",
		Issuer:                "https://93.184.216.34",
		AuthorizationEndpoint: "https://93.184.216.34/authorize",
		TokenEndpoint:         "https://93.184.216.34/token",
		RegistrationEndpoint:  "http://127.0.0.1/register",
	})
	if err == nil {
		t.Fatal("a registration endpoint outside the policy was accepted")
	}
	if !strings.Contains(err.Error(), "registration endpoint") {
		t.Fatalf("error = %v, want it to name the registration endpoint", err)
	}

	// The same metadata with a policy-allowed registration endpoint passes.
	if err := validateDiscovered(context.Background(), strict, validatedDiscovery{
		RawResource:           "https://93.184.216.34/mcp",
		Issuer:                "https://93.184.216.34",
		AuthorizationEndpoint: "https://93.184.216.34/authorize",
		TokenEndpoint:         "https://93.184.216.34/token",
		RegistrationEndpoint:  "https://93.184.216.34/register",
	}); err != nil {
		t.Fatalf("validateDiscovered: %v", err)
	}
}

func TestMcpOAuthDiscoveryReportsAnOverflowedRecorder(t *testing.T) {
	ctx := context.Background()
	var calls int
	rec := newRecordRequest(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"n":` + fmt.Sprint(calls) + `}`)),
			Request:    req,
		}, nil
	}), ctx)

	for i := 0; i < discoveryMaxRecordedResponses+1; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mcp.example/x", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp, err := rec.RoundTrip(req)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	// The window no longer holds this round's evidence, and it says so instead of
	// blaming a missing challenge.
	entries, err := rec.since(0)
	if err == nil {
		t.Fatalf("since returned %d entries for an overflowed recorder", len(entries))
	}
	if !errors.Is(err, errObservationOverflow) {
		t.Fatalf("since = %v, want errObservationOverflow", err)
	}
	if strings.Contains(err.Error(), "authentication challenge") {
		t.Fatalf("the failure blames the challenge: %v", err)
	}
	// And a capture over that recorder surfaces the same reason rather than the
	// misleading one.
	if _, capErr := captureDiscovery(ctx, discoveryInput{
		Endpoint:         "https://mcp.example/mcp",
		AuthorizationURL: "https://as.example/authorize?resource=" + url.QueryEscape("https://mcp.example/mcp"),
		Recorder:         rec,
		Policy:           publicPolicy(t),
	}); !errors.Is(capErr, errObservationOverflow) {
		t.Fatalf("captureDiscovery = %v, want errObservationOverflow", capErr)
	}
}
