// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// This file is the in-memory implementation of the durable contract the engine
// is written against. pkg/mcpauthdb is tested against a real database by its own
// package; what the engine needs here is the SEMANTICS it depends on — persisted
// generations, a pending fence that a second Begin supersedes, a grant CAS at
// commit, a binding that a refresh never moves, and the sentinel identities that
// tell "nothing stored" apart from "needs a login" apart from "the database is
// down".
//
// It is deliberately strict about the rules the engine must not lean around: an
// unbound endpoint still fences, a stale generation is refused rather than
// merged, and a deleted owner is refused by every path except the destructive
// one.

type fakePending struct {
	endpointGeneration int64
	resource           string
	issuer             string
	grantGeneration    int64
}

type fakeStore struct {
	mu sync.Mutex

	// deleted marks owners whose user row is tombstoned.
	deleted map[string]bool
	// endpointGeneration is the persisted endpoint fence.
	endpointGeneration map[string]int64
	// pending is the login fence a CommitLogin must match.
	pending map[string]*fakePending
	// bindings is owner+endpoint -> live binding.
	bindings map[string]Binding
	// grants is owner+resource -> grant (invalidated grants are marked).
	grants map[string]*Grant

	calls []string

	beginErr      error
	bindErr       error
	commitErr     error
	lookupErr     error
	acquireErr    error
	invalidateErr error

	acquires  int
	refreshes int

	// revocations is what a logout detached, for tests that assert the
	// best-effort revoke upstream.
	revocations []Revocation
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		deleted:            map[string]bool{},
		endpointGeneration: map[string]int64{},
		pending:            map[string]*fakePending{},
		bindings:           map[string]Binding{},
		grants:             map[string]*Grant{},
	}
}

func fakeKey(owner, other string) string { return owner + "\x00" + other }

func (s *fakeStore) note(call string) { s.calls = append(s.calls, call) }

// callLog returns the calls made so far.
func (s *fakeStore) callLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *fakeStore) BeginFence(ctx context.Context, owner, endpoint string) (LoginFence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note("BeginFence")
	if s.beginErr != nil {
		return LoginFence{}, s.beginErr
	}
	if s.deleted[owner] {
		return LoginFence{}, ErrLoginRequired
	}
	key := fakeKey(owner, endpoint)
	gen := s.endpointGeneration[key]
	if gen == 0 {
		gen = 1
	}
	s.endpointGeneration[key] = gen
	s.pending[key] = &fakePending{endpointGeneration: gen}
	return LoginFence{OwnerUserID: owner, Endpoint: endpoint, EndpointGeneration: gen, OwnerLive: true}, nil
}

func (s *fakeStore) BindFence(ctx context.Context, fence LoginFence, resource, issuer string) (LoginFence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note("BindFence")
	if s.bindErr != nil {
		return fence, s.bindErr
	}
	if s.deleted[fence.OwnerUserID] {
		return fence, ErrLoginRequired
	}
	key := fakeKey(fence.OwnerUserID, fence.Endpoint)
	if s.endpointGeneration[key] != fence.EndpointGeneration {
		return fence, ErrStaleFence
	}
	p := s.pending[key]
	if p == nil || p.endpointGeneration != fence.EndpointGeneration {
		return fence, ErrStaleFence
	}
	var grantGeneration int64
	if g := s.grants[fakeKey(fence.OwnerUserID, resource)]; g != nil {
		grantGeneration = g.Generation
	}
	p.resource, p.issuer, p.grantGeneration = resource, issuer, grantGeneration
	out := fence
	out.OwnerLive = true
	out.Resource = resource
	out.Issuer = issuer
	out.GrantGeneration = grantGeneration
	return out, nil
}

func (s *fakeStore) CommitLogin(ctx context.Context, fence LoginFence, grant Grant) (Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note("CommitLogin")
	if s.commitErr != nil {
		return Grant{}, s.commitErr
	}
	if s.deleted[fence.OwnerUserID] {
		return Grant{}, ErrLoginRequired
	}
	if fence.Resource == "" || grant.Resource != fence.Resource || grant.Issuer != fence.Issuer {
		return Grant{}, ErrStaleFence
	}
	key := fakeKey(fence.OwnerUserID, fence.Endpoint)
	if s.endpointGeneration[key] != fence.EndpointGeneration {
		return Grant{}, ErrStaleFence
	}
	p := s.pending[key]
	if p == nil || p.resource != fence.Resource || p.issuer != fence.Issuer || p.grantGeneration != fence.GrantGeneration {
		return Grant{}, ErrStaleFence
	}
	gkey := fakeKey(fence.OwnerUserID, fence.Resource)
	next := grant
	next.OwnerUserID = fence.OwnerUserID
	next.Resource = fence.Resource
	next.Issuer = fence.Issuer
	var current int64
	if cur := s.grants[gkey]; cur != nil {
		current = cur.Generation
	}
	if current != fence.GrantGeneration {
		return Grant{}, ErrStaleFence
	}
	next.Generation = current + 1
	next.RefreshState = RefreshIdle
	s.grants[gkey] = &next
	s.bindings[key] = Binding{
		OwnerUserID:     fence.OwnerUserID,
		Endpoint:        fence.Endpoint,
		Resource:        fence.Resource,
		Issuer:          fence.Issuer,
		GrantGeneration: next.Generation,
	}
	delete(s.pending, key)
	return next, nil
}

func (s *fakeStore) LookupBinding(ctx context.Context, owner, endpoint string) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note("LookupBinding")
	if s.lookupErr != nil {
		return Binding{}, s.lookupErr
	}
	if s.deleted[owner] {
		return Binding{}, ErrLoginRequired
	}
	b, ok := s.bindings[fakeKey(owner, endpoint)]
	if !ok {
		return Binding{}, ErrMissing
	}
	g := s.grants[fakeKey(owner, b.Resource)]
	if g == nil || g.Generation != b.GrantGeneration {
		return Binding{}, ErrLoginRequired
	}
	return b, nil
}

func (s *fakeStore) Acquire(ctx context.Context, owner, endpoint string, expectedGeneration int64, refresh RefreshFunc) (Grant, error) {
	s.mu.Lock()
	if s.acquireErr != nil {
		err := s.acquireErr
		s.mu.Unlock()
		return Grant{}, err
	}
	s.note("Acquire")
	s.acquires++
	if s.deleted[owner] {
		s.mu.Unlock()
		return Grant{}, ErrLoginRequired
	}
	b, ok := s.bindings[fakeKey(owner, endpoint)]
	if !ok {
		s.mu.Unlock()
		return Grant{}, ErrMissing
	}
	if b.GrantGeneration != expectedGeneration {
		s.mu.Unlock()
		return Grant{}, ErrStaleFence
	}
	g := s.grants[fakeKey(owner, b.Resource)]
	if g == nil {
		s.mu.Unlock()
		return Grant{}, ErrLoginRequired
	}
	if g.Generation != expectedGeneration {
		s.mu.Unlock()
		return Grant{}, ErrStaleFence
	}
	if g.RefreshState == RefreshRefreshing {
		s.mu.Unlock()
		return Grant{}, ErrLoginRequired
	}
	if !g.Token.Expiry.IsZero() && g.Token.Expiry.After(time.Now()) {
		out := *g
		s.mu.Unlock()
		return out, nil
	}
	if refresh == nil || g.Token.RefreshToken == "" {
		s.mu.Unlock()
		return Grant{}, ErrLoginRequired
	}
	g.RefreshState = RefreshRefreshing
	current := *g
	s.mu.Unlock()

	// Outside every lock, like the real store: the intent is committed first.
	result, err := refresh(ctx, current)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	g.RefreshState = RefreshIdle
	if err != nil {
		return Grant{}, errors.Join(ErrLoginRequired, err)
	}
	g.Token.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		g.Token.RefreshToken = result.RefreshToken
	}
	if result.TokenType != "" {
		g.Token.TokenType = result.TokenType
	}
	g.Token.Expiry = result.Expiry
	if result.Scopes != nil {
		g.Scopes = result.Scopes
	}
	return *g, nil
}

func (s *fakeStore) InvalidateResource(ctx context.Context, owner, resource string) (Revocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note("InvalidateResource")
	if s.invalidateErr != nil {
		return Revocation{}, s.invalidateErr
	}
	gkey := fakeKey(owner, resource)
	g := s.grants[gkey]
	if g == nil {
		return Revocation{}, ErrMissing
	}
	rev := Revocation{
		OwnerUserID:  owner,
		Resource:     resource,
		Issuer:       g.Issuer,
		Registration: g.Registration,
		RefreshToken: g.Token.RefreshToken,
		AccessToken:  g.Token.AccessToken,
	}
	g.Generation++
	g.Token = Token{}
	rev.Generation = g.Generation
	for key, b := range s.bindings {
		if b.OwnerUserID == owner && b.Resource == resource {
			s.endpointGeneration[key]++
			delete(s.bindings, key)
		}
	}
	for key, p := range s.pending {
		if p.resource == resource && len(key) > len(owner) && key[:len(owner)] == owner {
			s.endpointGeneration[key]++
			delete(s.pending, key)
		}
	}
	s.revocations = append(s.revocations, rev)
	return rev, nil
}

func (s *fakeStore) InvalidateEndpoint(ctx context.Context, owner, endpoint string) (Revocation, error) {
	s.mu.Lock()
	s.note("InvalidateEndpoint")
	if s.invalidateErr != nil {
		err := s.invalidateErr
		s.mu.Unlock()
		return Revocation{}, err
	}
	key := fakeKey(owner, endpoint)
	s.endpointGeneration[key]++
	delete(s.pending, key)
	b, ok := s.bindings[key]
	if !ok {
		s.mu.Unlock()
		return Revocation{}, nil
	}
	delete(s.bindings, key)
	s.mu.Unlock()
	return s.InvalidateResource(ctx, owner, b.Resource)
}

func (s *fakeStore) InvalidateOwner(ctx context.Context, owner string) ([]Revocation, error) {
	s.mu.Lock()
	s.note("InvalidateOwner")
	var resources []string
	for key, g := range s.grants {
		if len(key) > len(owner) && key[:len(owner)] == owner && key[len(owner)] == 0 {
			resources = append(resources, g.Resource)
		}
	}
	for key := range s.endpointGeneration {
		if len(key) > len(owner) && key[:len(owner)] == owner && key[len(owner)] == 0 {
			s.endpointGeneration[key]++
			delete(s.pending, key)
			delete(s.bindings, key)
		}
	}
	s.mu.Unlock()

	var out []Revocation
	for _, resource := range resources {
		rev, err := s.InvalidateResource(ctx, owner, resource)
		if err != nil && !errors.Is(err, ErrMissing) {
			return out, err
		}
		out = append(out, rev)
	}
	return out, nil
}

// grant returns a copy of the stored grant for owner+resource.
func (s *fakeStore) grant(owner, resource string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[fakeKey(owner, resource)]
	if g == nil {
		return Grant{}, false
	}
	return *g, true
}

// seed installs a live binding and grant, as a completed login would.
func (s *fakeStore) seed(owner, endpoint, resource, issuer string, tok Token) Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	gkey := fakeKey(owner, resource)
	gen := int64(1)
	if cur := s.grants[gkey]; cur != nil {
		gen = cur.Generation + 1
	}
	g := Grant{
		OwnerUserID: owner,
		Resource:    resource,
		Issuer:      issuer,
		Generation:  gen,
		Registration: Registration{
			ClientID:              "seeded-client",
			AuthStyle:             AuthStyleInParams,
			AuthorizationEndpoint: issuer + "/authorize",
			TokenEndpoint:         issuer + "/token",
			RevocationEndpoint:    issuer + "/revoke",
		},
		Scopes:       []string{"read"},
		RedirectURL:  CallbackURL,
		Token:        tok,
		RefreshState: RefreshIdle,
	}
	s.grants[gkey] = &g
	key := fakeKey(owner, endpoint)
	s.endpointGeneration[key] = gen
	s.bindings[key] = Binding{
		OwnerUserID:     owner,
		Endpoint:        endpoint,
		Resource:        resource,
		Issuer:          issuer,
		GrantGeneration: gen,
	}
	return g
}

// bindEndpoint binds an endpoint to an existing grant generation, as a completed
// login would have.
func (s *fakeStore) bindEndpoint(owner, endpoint, resource string, generation int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[fakeKey(owner, endpoint)] = Binding{
		OwnerUserID:     owner,
		Endpoint:        endpoint,
		Resource:        resource,
		Issuer:          s.grants[fakeKey(owner, resource)].Issuer,
		GrantGeneration: generation,
	}
}

// deleteOwner tombstones an owner, as usersdb would.
func (s *fakeStore) deleteOwner(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted[owner] = true
}

// rebaseGrant installs a newer generation of a grant and rebinds the endpoint to
// it, exactly as a newer explicit login would: the old generation's pinned
// token source is stale from then on.
func (s *fakeStore) rebaseGrant(owner, endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fakeKey(owner, endpoint)
	b, ok := s.bindings[key]
	if !ok {
		return
	}
	g := s.grants[fakeKey(owner, b.Resource)]
	if g == nil {
		return
	}
	g.Generation++
	b.GrantGeneration = g.Generation
	s.bindings[key] = b
}

// advanceEndpoint moves an endpoint fence, as a logout or a newer login would.
func (s *fakeStore) advanceEndpoint(owner, endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpointGeneration[fakeKey(owner, endpoint)]++
	delete(s.pending, fakeKey(owner, endpoint))
}

// ---------------------------------------------------------------------------
// Fixture: one loopback origin that speaks the whole OAuth dance.
// ---------------------------------------------------------------------------

// fakeOAuthServer serves protected-resource metadata, authorization-server
// metadata, dynamic client registration, the token endpoint, a revocation
// endpoint and an authenticated MCP endpoint from one origin. Every test in this
// package that needs a real discovery/registration/exchange interaction uses it,
// so a behaviour change is a knob here rather than a second fixture elsewhere.
type fakeOAuthServer struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex

	// Metadata shape.
	prmScopes       []string
	servePRMPath    bool
	servePRMRoot    bool
	prmResourcePath string
	prmResourceRoot string
	// prmVaryOnSecondHit makes the path location answer with the ENDPOINT as its
	// resource after its first hit, which is how a server makes one URL answer
	// differently twice inside a single consent round.
	prmVaryOnSecondHit bool
	serveASM           bool
	asmScopes          []string
	// authorizationEndpoint overrides the authorization endpoint the AS metadata
	// advertises, which is how a consent URL outside the operator's policy is
	// produced.
	authorizationEndpoint string
	// dcrFailure makes dynamic client registration fail: "error" answers 400 with
	// dcrSentinel embedded in the error body, "malformed" answers 201 with an
	// undecodable body that carries it.
	dcrFailure  string
	dcrSentinel string
	// tokenRefuseStatus/tokenRefuseBody answer every token request with a refusal,
	// which is how a refresh is made to fail at the endpoint.
	tokenRefuseStatus int
	tokenRefuseBody   string
	// omitRefreshExpiry drops expires_in from refresh answers.
	omitRefreshExpiry bool
	serveASMRevoke    bool
	issueIss          bool
	asmIssuer         string
	// issuerURL overrides the authorization server the metadata names, which is
	// how a changed issuer is exercised.
	issuerURL string
	// emptyFirstAuthorizationServer serves an empty first entry, which no
	// client may treat as an issuer.
	emptyFirstAuthorizationServer bool

	// MCP behaviour.
	tools         []string
	dropCodes     bool
	requireScopes []string
	stepUpLeft    int
	failTools     bool
	tokenType     string

	// Barriers: a non-nil channel holds the handler until it is closed or the
	// request's context ends.
	prmBlock   chan struct{}
	tokenBlock chan struct{}
	mcpBlock   chan struct{}
	// Arrival signals are set alongside a barrier: they let a test wait until the
	// request is genuinely in flight without polling.
	prmArrived   chan struct{}
	tokenArrived chan struct{}

	// Recorded traffic.
	requests     []string
	prmHits      map[string]int
	asmHits      map[string]int
	dcrBodies    []map[string]any
	dcrClients   []string
	tokenForms   []url.Values
	codes        map[string]string
	codeScopes   map[string]string
	tokens       map[string][]string
	refreshOK    map[string]bool
	revoked      []string
	tokenCalls   int
	revokeHits   int
	revokeStatus int
	// lastTokenHadBasicAuth records how the most recent token request presented
	// its client credentials.
	lastTokenHadBasicAuth bool
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()
	f := &fakeOAuthServer{
		t:              t,
		prmScopes:      []string{"read", "write"},
		servePRMPath:   true,
		servePRMRoot:   true,
		serveASM:       true,
		asmScopes:      []string{"read", "write", "offline_access"},
		serveASMRevoke: true,
		tools:          []string{"zeta", "alpha"},
		prmHits:        map[string]int{},
		asmHits:        map[string]int{},
		codes:          map[string]string{},
		codeScopes:     map[string]string{},
		tokens:         map[string][]string{},
		refreshOK:      map[string]bool{},
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	for _, name := range f.tools {
		name := name
		mcpServer.AddTool(&mcp.Tool{
			Name:        name,
			Description: "fixture tool " + name,
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil
		})
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", f.handlePRMPath)
	mux.HandleFunc("/.well-known/oauth-protected-resource", f.handlePRMRoot)
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.handleASM)
	mux.HandleFunc("/.well-known/openid-configuration", f.handleASM)
	mux.HandleFunc("/register", f.handleRegister)
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/revoke", f.handleRevoke)
	mux.Handle("/mcp", f.guard(f.intercept(mcpHandler)))
	mux.Handle("/mcp/", f.guard(f.intercept(mcpHandler)))
	f.srv = httptest.NewServer(f.record(mux))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuthServer) endpoint() string { return f.srv.URL + "/mcp" }

// endpointAt is a second MCP endpoint on the same origin, which is what the
// per-owner pending cap needs: one owner may hold only one pending login per
// endpoint.
func (f *fakeOAuthServer) endpointAt(suffix string) string { return f.srv.URL + "/mcp/" + suffix }
func (f *fakeOAuthServer) issuer() string                  { return f.srv.URL }
func (f *fakeOAuthServer) rootResource() string {
	u, err := url.Parse(f.endpoint())
	if err != nil {
		f.t.Fatalf("fixture endpoint: %v", err)
	}
	u.Path = ""
	return u.String()
}

// record logs every request the flow makes, which is how the tests prove that
// nothing was fetched twice and that no second discovery happened.
func (f *fakeOAuthServer) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (f *fakeOAuthServer) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeOAuthServer) prmRequestCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prmHits[path]
}

func (f *fakeOAuthServer) tokenForm(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.tokenForms) {
		return nil
	}
	return f.tokenForms[i]
}

// waitBarrier blocks a handler on a barrier channel, or returns false when the
// caller's context ended first — which is how a cancellation test observes that
// the daemon really did abandon the request.
func waitBarrier(ctx context.Context, ch chan struct{}, arrived chan struct{}) bool {
	if ch == nil {
		return true
	}
	if arrived != nil {
		select {
		case arrived <- struct{}{}:
		default:
		}
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// authorizationServers is the metadata's authorization_servers value.
func (f *fakeOAuthServer) authorizationServers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.emptyFirstAuthorizationServer {
		return []string{"", f.issuer()}
	}
	if f.issuerURL != "" {
		return []string{f.issuerURL}
	}
	return []string{f.issuer()}
}

func (f *fakeOAuthServer) handlePRMPath(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.prmHits[r.URL.Path]++
	block := f.prmBlock
	arrived := f.prmArrived
	serve := f.servePRMPath
	resource := f.prmResourcePath
	if resource == "" {
		resource = f.endpoint()
	}
	if f.prmVaryOnSecondHit && f.prmHits[r.URL.Path] > 1 {
		resource = f.endpoint()
	}
	scopes := append([]string(nil), f.prmScopes...)
	f.mu.Unlock()
	if !serve {
		http.Error(w, "no metadata here", http.StatusNotFound)
		return
	}
	if !waitBarrier(r.Context(), block, arrived) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": f.authorizationServers(),
		"scopes_supported":      scopes,
	})
}

func (f *fakeOAuthServer) handlePRMRoot(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.prmHits[r.URL.Path]++
	block := f.prmBlock
	arrived := f.prmArrived
	serve := f.servePRMRoot
	resource := f.prmResourceRoot
	if resource == "" {
		resource = f.rootResource()
	}
	scopes := append([]string(nil), f.prmScopes...)
	f.mu.Unlock()
	if !serve {
		http.Error(w, "no metadata here", http.StatusNotFound)
		return
	}
	if !waitBarrier(r.Context(), block, arrived) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": f.authorizationServers(),
		"scopes_supported":      scopes,
	})
}

func (f *fakeOAuthServer) handleASM(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asmHits[r.URL.Path]++
	block := f.prmBlock
	arrived := f.prmArrived
	f.mu.Unlock()
	if !waitBarrier(r.Context(), block, arrived) {
		return
	}
	f.mu.Lock()
	serve, issuer, scopes := f.serveASM, f.asmIssuer, append([]string(nil), f.asmScopes...)
	revoke := f.serveASMRevoke
	if issuer == "" {
		issuer = f.issuer()
	}
	f.mu.Unlock()
	if !serve {
		http.Error(w, "no metadata here", http.StatusNotFound)
		return
	}
	f.mu.Lock()
	authorizationEndpoint := f.authorizationEndpoint
	f.mu.Unlock()
	if authorizationEndpoint == "" {
		authorizationEndpoint = f.srv.URL + "/authorize"
	}
	meta := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                authorizationEndpoint,
		"token_endpoint":                        f.srv.URL + "/token",
		"registration_endpoint":                 f.srv.URL + "/register",
		"scopes_supported":                      scopes,
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	}
	if revoke {
		meta["revocation_endpoint"] = f.srv.URL + "/revoke"
	}
	if f.issueIss {
		meta["authorization_response_iss_parameter_supported"] = true
	}
	writeJSON(w, http.StatusOK, meta)
}

func (f *fakeOAuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad registration body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.dcrBodies = append(f.dcrBodies, body)
	clientID := fmt.Sprintf("client-%d", len(f.dcrBodies))
	f.dcrClients = append(f.dcrClients, clientID)
	failure, sentinel := f.dcrFailure, f.dcrSentinel
	f.mu.Unlock()
	switch failure {
	case "error":
		// The shape oauthex.RegisterClient reports verbatim, with a sentinel where a
		// real server would put anything it likes.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "invalid_client_metadata",
			"error_description": sentinel,
		})
		return
	case "malformed":
		// A "successful" registration the SDK cannot decode. oauthex quotes the raw
		// body in that error, which is exactly how a client secret would leak.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_secret":"` + sentinel + `"`))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              body["redirect_uris"],
		"grant_types":                body["grant_types"],
		"token_endpoint_auth_method": "none",
	})
}

func (f *fakeOAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	f.mu.Lock()
	if f.tokenRefuseStatus != 0 {
		status, refuseBody := f.tokenRefuseStatus, f.tokenRefuseBody
		f.tokenForms = append(f.tokenForms, form)
		f.tokenCalls++
		f.lastTokenHadBasicAuth = strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "basic ")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(refuseBody))
		return
	}
	f.tokenForms = append(f.tokenForms, form)
	f.tokenCalls++
	f.lastTokenHadBasicAuth = strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "basic ")
	block := f.tokenBlock
	arrived := f.tokenArrived
	tokenType := f.tokenType
	f.mu.Unlock()
	if !waitBarrier(r.Context(), block, arrived) {
		return
	}
	if tokenType == "" {
		tokenType = "Bearer"
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		if f.dropCodes {
			delete(f.codes, form.Get("code"))
		}
		challenge, ok := f.codes[form.Get("code")]
		scopes := f.codeScopes[form.Get("code")]
		f.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "pkce mismatch"})
			return
		}
		if form.Get("code_verifier") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "no verifier"})
			return
		}
		f.mu.Lock()
		n := len(f.tokens) + 1
		access := fmt.Sprintf("access-%d", n)
		refresh := fmt.Sprintf("refresh-%d", n)
		f.tokens[access] = strings.Fields(scopes)
		f.refreshOK[refresh] = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    tokenType,
			"expires_in":    3600,
			"scope":         scopes,
		})
	case "refresh_token":
		refresh := form.Get("refresh_token")
		f.mu.Lock()
		ok := f.refreshOK[refresh]
		f.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		f.mu.Lock()
		n := len(f.tokens) + 1
		access := fmt.Sprintf("access-%d", n)
		f.tokens[access] = []string{"read", "write"}
		omitExpiry := f.omitRefreshExpiry
		// Deliberately omit a replacement refresh token: the store must keep the
		// stored one rather than stranding the grant.
		f.mu.Unlock()
		answer := map[string]any{
			"access_token": access,
			"token_type":   tokenType,
			"scope":        "read write",
		}
		if !omitExpiry {
			answer["expires_in"] = 3600
		}
		writeJSON(w, http.StatusOK, answer)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (f *fakeOAuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.revokeHits++
	f.revoked = append(f.revoked, r.PostForm.Get("token"))
	status := f.revokeStatus
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

// guard is the MCP endpoint's bearer check. It answers the shape the SDK's step-up
// logic understands: a 403 with error="insufficient_scope" and the scope it
// wants, which is what makes the handler ask for another round.
func (f *fakeOAuthServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		require := append([]string(nil), f.requireScopes...)
		f.mu.Unlock()
		scopes, ok := f.scopesOf(r)
		if !ok {
			f.challenge(w, http.StatusUnauthorized, "", "")
			return
		}
		missing := missingScopes(scopes, require)
		if len(missing) > 0 {
			f.challenge(w, http.StatusForbidden, "insufficient_scope", strings.Join(missing, " "))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fakeOAuthServer) scopesOf(r *http.Request) ([]string, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return nil, false
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	f.mu.Lock()
	defer f.mu.Unlock()
	scopes, ok := f.tokens[token]
	return scopes, ok
}

func (f *fakeOAuthServer) challenge(w http.ResponseWriter, status int, errCode, scope string) {
	v := `Bearer resource_metadata="` + f.srv.URL + `/.well-known/oauth-protected-resource/mcp"`
	if errCode != "" {
		v += `, error="` + errCode + `"`
	}
	if scope != "" {
		v += `, scope="` + scope + `"`
	}
	w.Header().Set("WWW-Authenticate", v)
	w.WriteHeader(status)
}

// intercept refuses tools/list on request, which is how the "a credential that
// cannot list tools is not committed" case is driven without a second server.
func (f *fakeOAuthServer) intercept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fail := f.failTools
		block := f.mcpBlock
		f.mu.Unlock()
		if !waitBarrier(r.Context(), block, nil) {
			return
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &msg)
			if msg.Method == "tools/list" {
				f.mu.Lock()
				stepUp := f.stepUpLeft > 0
				if stepUp {
					f.stepUpLeft--
				}
				f.mu.Unlock()
				// A step-up is only reachable on a request the SDK has not just
				// authorized: it re-authorizes once per request and then retries,
				// so a challenge on the retry is never re-authorized.
				if stepUp {
					f.challenge(w, http.StatusForbidden, "insufficient_scope", "write")
					return
				}
				if fail {
					http.Error(w, "fixture: tools are unavailable", http.StatusBadRequest)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func missingScopes(have, want []string) []string {
	var missing []string
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
}

// callback plays the browser and the authorization server: it issues a code for
// a consent URL and returns the query the client would hand back.
func (f *fakeOAuthServer) callback(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		f.t.Fatalf("fixture: authorize URL: %v", err)
	}
	q := u.Query()
	f.mu.Lock()
	code := fmt.Sprintf("code-%d", len(f.codes)+1)
	f.codes[code] = q.Get("code_challenge")
	f.codeScopes[code] = q.Get("scope")
	f.mu.Unlock()
	cb := url.Values{}
	cb.Set("code", code)
	cb.Set("state", q.Get("state"))
	if f.issueIss {
		cb.Set("iss", f.issuer())
	}
	return cb.Encode()
}

// issueToken registers an access token with scopes, as the token endpoint would.
func (f *fakeOAuthServer) issueToken(token string, scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = scopes
}

// ---------------------------------------------------------------------------
// Engine fixture and tests.
// ---------------------------------------------------------------------------

const testOwner = "11111111-1111-1111-1111-111111111111"

// loopbackPolicy is the operator policy a login test needs to reach a loopback
// fixture: an allow_hosts entry for the address AND an allow_http_hosts entry for
// the cleartext scheme. Neither widens any other destination.
func loopbackPolicy(t *testing.T) *Policy {
	t.Helper()
	return testPolicy(t, Config{
		AllowHosts:     []string{"127.0.0.1"},
		AllowHTTPHosts: []string{"127.0.0.1"},
	}, nil, nil)
}

type engineFixture struct {
	t     *testing.T
	eng   *Engine
	store *fakeStore
	oauth *fakeOAuthServer
	owner string
	timer chan time.Time
	logs  *lockedBuffer
}

// lockedBuffer collects log output for the tests that assert no secret reaches
// it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newEngineFixture(t *testing.T, tweaks ...func(*EngineOptions)) *engineFixture {
	t.Helper()
	oauth := newFakeOAuthServer(t)
	store := newFakeStore()
	logs := &lockedBuffer{}
	timer := make(chan time.Time, 8)
	opts := EngineOptions{
		Store:  store,
		Policy: loopbackPolicy(t),
		After:  func(time.Duration) <-chan time.Time { return timer },
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, tweak := range tweaks {
		tweak(&opts)
	}
	eng, err := NewEngine(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return &engineFixture{t: t, eng: eng, store: store, oauth: oauth, owner: testOwner, timer: timer, logs: logs}
}

func (fx *engineFixture) begin(scopes ...string) LoginDescriptor {
	fx.t.Helper()
	return fx.beginAt(fx.oauth.endpoint(), scopes...)
}

func (fx *engineFixture) beginAt(endpoint string, scopes ...string) LoginDescriptor {
	fx.t.Helper()
	return fx.beginFor(fx.owner, endpoint, scopes...)
}

func (fx *engineFixture) beginFor(owner, endpoint string, scopes ...string) LoginDescriptor {
	fx.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := fx.eng.Begin(ctx, BeginRequest{
		OwnerUserID: owner,
		Server:      "fixture",
		Endpoint:    endpoint,
		Scopes:      scopes,
	})
	if err != nil {
		fx.t.Fatalf("Begin: %v", err)
	}
	return d
}

// sameScopes compares scope lists as sets: the SDK unions discovered and
// challenged scopes through a map, so the order it asks for is its own business.
func sameScopes(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
		if seen[w] < 0 {
			return false
		}
	}
	return true
}

func (fx *engineFixture) complete(loginID, query string) (CompleteResult, error) {
	fx.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return fx.eng.Complete(ctx, fx.owner, loginID, query)
}

func (fx *engineFixture) mustComplete(loginID, query string) CompleteResult {
	fx.t.Helper()
	res, err := fx.complete(loginID, query)
	if err != nil {
		fx.t.Fatalf("Complete: %v", err)
	}
	return res
}

// waitForGroupDone asserts the engine's task group drains, which is how every
// cancellation test proves the work really ended rather than merely being
// unreachable.
func waitForGroupDone(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("engine tasks did not terminate")
	}
}

func TestMcpOAuthEngineLoginCommitsVerifiedTools(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	if d.LoginID == "" {
		t.Fatal("Begin returned no login id")
	}
	if d.RedirectPort != CallbackPort || d.CallbackPath != CallbackPath {
		t.Fatalf("redirect = %d %q, want %d %q", d.RedirectPort, d.CallbackPath, CallbackPort, CallbackPath)
	}
	if len(d.RedirectHosts) != 1 || d.RedirectHosts[0] != CallbackHost {
		t.Fatalf("redirect hosts = %v, want [%s]", d.RedirectHosts, CallbackHost)
	}

	authURL, err := url.Parse(d.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	q := authURL.Query()
	if got, want := q.Get("resource"), fx.oauth.endpoint(); got != want {
		t.Fatalf("authorize resource = %q, want %q", got, want)
	}
	if got := q.Get("redirect_uri"); got != CallbackURL {
		t.Fatalf("authorize redirect_uri = %q, want %q", got, CallbackURL)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if q.Get("code_challenge") == "" {
		t.Fatal("authorize URL carries no PKCE challenge")
	}
	// Defaults: the resource's advertised scopes, plus offline_access because the
	// authorization server advertises it and RequestRefreshToken is set.
	if got := strings.Fields(q.Get("scope")); !sameScopes(got, "read", "write", "offline_access") {
		t.Fatalf("authorize scope = %v, want read write offline_access", got)
	}

	res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if res.Continuation != nil {
		t.Fatalf("unexpected continuation: %+v", res.Continuation)
	}
	if res.Server != "fixture" {
		t.Fatalf("server = %q, want fixture", res.Server)
	}
	if got, want := res.Resource, fx.oauth.endpoint(); got != want {
		t.Fatalf("resource = %q, want %q", got, want)
	}
	if res.AccountID != "" {
		t.Fatalf("account id = %q, want empty (no validated source)", res.AccountID)
	}
	if got, want := strings.Join(res.Tools, ","), "alpha,zeta"; got != want {
		t.Fatalf("tools = %q, want %q (sorted names)", got, want)
	}

	grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("no grant was committed")
	}
	if got, want := grant.Issuer, fx.oauth.issuer(); got != want {
		t.Fatalf("grant issuer = %q, want %q", got, want)
	}
	if grant.Registration.ClientID == "" {
		t.Fatal("grant carries no client registration")
	}
	if grant.Registration.AuthStyle != AuthStyleInParams {
		t.Fatalf("auth style = %q, want in_params (from token_endpoint_auth_method none)", grant.Registration.AuthStyle)
	}
	if grant.Registration.RevocationEndpoint == "" {
		t.Fatal("grant did not keep the advertised revocation endpoint")
	}
	if grant.Token.AccessToken == "" || grant.Token.RefreshToken == "" {
		t.Fatal("grant is missing token material")
	}
	if !sameScopes(grant.Scopes, "read", "write", "offline_access") {
		t.Fatalf("granted scopes = %v, want read write offline_access", grant.Scopes)
	}

	// The registration asked for a native public client with both grants.
	fx.oauth.mu.Lock()
	dcr := fx.oauth.dcrBodies
	fx.oauth.mu.Unlock()
	if len(dcr) != 1 {
		t.Fatalf("registrations = %d, want exactly one per explicit login", len(dcr))
	}
	if got := dcr[0]["application_type"]; got != "native" {
		t.Fatalf("application_type = %v, want native (loopback redirect)", got)
	}
	if got := dcr[0]["token_endpoint_auth_method"]; got != "none" {
		t.Fatalf("token_endpoint_auth_method = %v, want none (public client)", got)
	}
	grants, _ := dcr[0]["grant_types"].([]any)
	var got []string
	for _, g := range grants {
		got = append(got, fmt.Sprint(g))
	}
	if strings.Join(got, ",") != "authorization_code,refresh_token" {
		t.Fatalf("grant_types = %v", got)
	}
	redirects, _ := dcr[0]["redirect_uris"].([]any)
	if len(redirects) != 1 || fmt.Sprint(redirects[0]) != CallbackURL {
		t.Fatalf("redirect_uris = %v, want [%s]", redirects, CallbackURL)
	}

	// The exchange carried the PKCE verifier and the resource.
	form := fx.oauth.tokenForm(0)
	if form.Get("grant_type") != "authorization_code" {
		t.Fatalf("first token call grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("code_verifier") == "" {
		t.Fatal("the exchange sent no code_verifier")
	}
	if got, want := form.Get("resource"), fx.oauth.endpoint(); got != want {
		t.Fatalf("exchange resource = %q, want %q", got, want)
	}
	// oauth2's AuthStyleInParams puts the public client id in the body.
	if form.Get("client_id") != grant.Registration.ClientID {
		t.Fatalf("exchange client_id = %q, want %q", form.Get("client_id"), grant.Registration.ClientID)
	}
}

func TestMcpOAuthEngineExplicitScopesOverrideDiscovery(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin("write")

	authURL, err := url.Parse(d.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}
	// The explicit override replaces the discovered scopes; offline_access is
	// still added by the SDK afterwards.
	if got := strings.Fields(authURL.Query().Get("scope")); !sameScopes(got, "write", "offline_access") {
		t.Fatalf("authorize scope = %v, want write offline_access", got)
	}
	res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if res.Continuation != nil {
		t.Fatalf("unexpected continuation")
	}
}

// set applies a knob under the fixture's lock, so a test may change behaviour
// while the fixture is serving.
func (f *fakeOAuthServer) set(fn func(*fakeOAuthServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeOAuthServer) dcrCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dcrBodies)
}

func (f *fakeOAuthServer) dcrClient(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.dcrClients) {
		return ""
	}
	return f.dcrClients[i]
}

func (f *fakeOAuthServer) revokeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokeHits
}

func (f *fakeOAuthServer) asmHitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.asmHits {
		total += n
	}
	return total
}

func (f *fakeOAuthServer) tokenCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func TestMcpOAuthEngineStateMismatchIsRefused(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	cb := url.Values{}
	cb.Set("code", "code-1")
	cb.Set("state", "not-the-state-the-sdk-generated")
	if _, err := fx.complete(d.LoginID, cb.Encode()); err == nil {
		t.Fatal("Complete accepted a callback with the wrong state")
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed despite the state mismatch")
	}
	// The failed login is gone, not left half-open.
	if _, err := fx.complete(d.LoginID, cb.Encode()); !errors.Is(err, ErrMissing) {
		t.Fatalf("second Complete = %v, want ErrMissing", err)
	}
}

func TestMcpOAuthEngineIssuerParameterMustBePresentAndMatch(t *testing.T) {
	t.Run("missing iss", func(t *testing.T) {
		fx := newEngineFixture(t)
		fx.oauth.set(func(f *fakeOAuthServer) { f.issueIss = true })
		d := fx.begin()
		u, err := url.Parse(d.AuthorizeURL)
		if err != nil {
			t.Fatalf("authorize URL: %v", err)
		}
		cb := url.Values{}
		cb.Set("code", "code-1")
		cb.Set("state", u.Query().Get("state"))
		if _, err := fx.complete(d.LoginID, cb.Encode()); err == nil {
			t.Fatal("Complete accepted a callback without the required iss parameter")
		}
		if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
			t.Fatal("a grant was committed without the required iss parameter")
		}
	})
	t.Run("wrong iss", func(t *testing.T) {
		fx := newEngineFixture(t)
		fx.oauth.set(func(f *fakeOAuthServer) { f.issueIss = true })
		d := fx.begin()
		u, err := url.Parse(d.AuthorizeURL)
		if err != nil {
			t.Fatalf("authorize URL: %v", err)
		}
		cb := url.Values{}
		cb.Set("code", "code-1")
		cb.Set("state", u.Query().Get("state"))
		cb.Set("iss", "https://somewhere-else.invalid")
		if _, err := fx.complete(d.LoginID, cb.Encode()); err == nil {
			t.Fatal("Complete accepted a callback from another issuer")
		}
	})
	t.Run("matching iss", func(t *testing.T) {
		fx := newEngineFixture(t)
		fx.oauth.set(func(f *fakeOAuthServer) { f.issueIss = true })
		d := fx.begin()
		res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
		if res.Continuation != nil {
			t.Fatalf("unexpected continuation")
		}
	})
}

func TestMcpOAuthEngineStepUpRoundContinuesWithoutCommitting(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.stepUpLeft = 1 })

	first := fx.begin()
	firstURL, err := url.Parse(first.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize URL: %v", err)
	}

	res := fx.mustComplete(first.LoginID, fx.oauth.callback(first.AuthorizeURL))
	if res.Continuation == nil {
		t.Fatalf("a step-up round did not return a continuation: %+v dcr=%d tokens=%d requests=%v", res, fx.oauth.dcrCount(), fx.oauth.tokenCallCount(), fx.oauth.requestLog())
	}
	if res.Continuation.LoginID != first.LoginID {
		t.Fatalf("continuation login id = %q, want the same login", res.Continuation.LoginID)
	}
	if res.Continuation.AuthorizeURL == first.AuthorizeURL {
		t.Fatal("the continuation reused the first consent URL")
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed before the tools were verified")
	}

	secondURL, err := url.Parse(res.Continuation.AuthorizeURL)
	if err != nil {
		t.Fatalf("continuation URL: %v", err)
	}
	if secondURL.Query().Get("state") == firstURL.Query().Get("state") {
		t.Fatal("the continuation reused the first round's state")
	}
	// Step-up unions the newly challenged scope with what was already granted.
	if got := secondURL.Query().Get("scope"); !strings.Contains(got, "write") || !strings.Contains(got, "read") {
		t.Fatalf("continuation scope = %q, want the union of the granted and challenged scopes", got)
	}

	final := fx.mustComplete(res.Continuation.LoginID, fx.oauth.callback(res.Continuation.AuthorizeURL))
	if final.Continuation != nil {
		t.Fatalf("second round returned another continuation")
	}
	if got, want := strings.Join(final.Tools, ","), "alpha,zeta"; got != want {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("no grant after the step-up round")
	}
	// Each consent round registers afresh; the committed registration is the one
	// the final token was issued to.
	if got, want := fx.oauth.dcrCount(), 2; got != want {
		t.Fatalf("registrations = %d, want %d (one per consent round)", got, want)
	}
	if got, want := grant.Registration.ClientID, fx.oauth.dcrClient(1); got != want {
		t.Fatalf("committed client = %q, want the last round's %q", got, want)
	}
}

func TestMcpOAuthEngineCallbackIsSingleUse(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.stepUpLeft = 1 })

	d := fx.begin()
	query := fx.oauth.callback(d.AuthorizeURL)
	res := fx.mustComplete(d.LoginID, query)
	if res.Continuation == nil {
		t.Fatal("expected a continuation to test replay against")
	}
	if _, err := fx.complete(d.LoginID, query); !errors.Is(err, ErrCallbackReplayed) {
		t.Fatalf("replayed callback = %v, want ErrCallbackReplayed", err)
	}
	// The live round is untouched by the replay attempt.
	final := fx.mustComplete(res.Continuation.LoginID, fx.oauth.callback(res.Continuation.AuthorizeURL))
	if final.Continuation != nil {
		t.Fatal("the round after a replay attempt did not finish")
	}
}

func TestMcpOAuthEngineForeignLoginIDIsIndistinguishable(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Another owner's attempt on a live login answers exactly what an unknown id
	// does ...
	_, foreignErr := fx.eng.Complete(ctx, "22222222-2222-2222-2222-222222222222", d.LoginID, "code=x&state=y")
	_, unknownErr := fx.eng.Complete(ctx, "22222222-2222-2222-2222-222222222222", "no-such-login", "code=x&state=y")
	if !errors.Is(foreignErr, ErrMissing) || !errors.Is(unknownErr, ErrMissing) {
		t.Fatalf("foreign = %v, unknown = %v; both must be ErrMissing", foreignErr, unknownErr)
	}
	// ... and it consumed nothing, so the owner can still finish.
	res := fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if res.Continuation != nil {
		t.Fatal("the owner's login was disturbed by the foreign attempt")
	}
}

func TestMcpOAuthEnginePendingLimits(t *testing.T) {
	t.Run("per owner", func(t *testing.T) {
		fx := newEngineFixture(t, func(o *EngineOptions) { o.pendingPerOwner = 2; o.pendingTotal = 16 })
		first := fx.beginAt(fx.oauth.endpoint())
		fx.beginAt(fx.oauth.endpointAt("b"))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpointAt("c")})
		if !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("third Begin = %v, want ErrPendingLimit", err)
		}
		// Another owner is unaffected: one client cannot fill the table.
		other := newEngineFixtureOwner(fx, "33333333-3333-3333-3333-333333333333")
		if _, err := other(); err != nil {
			t.Fatalf("other owner Begin: %v", err)
		}
		// Finishing a login frees its slot.
		fx.mustComplete(first.LoginID, fx.oauth.callback(first.AuthorizeURL))
		if _, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpointAt("c")}); err != nil {
			t.Fatalf("Begin after a completed login: %v", err)
		}
	})

	t.Run("daemon wide", func(t *testing.T) {
		fx := newEngineFixture(t, func(o *EngineOptions) { o.pendingPerOwner = 8; o.pendingTotal = 1 })
		fx.begin()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := fx.eng.Begin(ctx, BeginRequest{
			OwnerUserID: "33333333-3333-3333-3333-333333333333",
			Server:      "fixture",
			Endpoint:    fx.oauth.endpoint(),
		})
		if !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("second owner Begin = %v, want ErrPendingLimit", err)
		}
	})
}

// newEngineFixtureOwner returns a Begin for a second owner against the same
// engine.
func newEngineFixtureOwner(fx *engineFixture, owner string) func() (LoginDescriptor, error) {
	return func() (LoginDescriptor, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return fx.eng.Begin(ctx, BeginRequest{OwnerUserID: owner, Server: "fixture", Endpoint: fx.oauth.endpoint()})
	}
}

func TestMcpOAuthEngineExpiryCancelsTheTask(t *testing.T) {
	fx := newEngineFixture(t, func(o *EngineOptions) { o.pendingPerOwner = 1 })
	d := fx.begin()

	fx.timer <- time.Now()
	waitForGroupDone(t, &fx.eng.wg)

	if _, err := fx.complete(d.LoginID, fx.oauth.callback(d.AuthorizeURL)); !errors.Is(err, ErrMissing) {
		t.Fatalf("Complete after expiry = %v, want ErrMissing", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("an expired login committed a grant")
	}
	// The slot is free again without anything else having to notice.
	if _, err := newEngineFixtureOwner(fx, fx.owner)(); err != nil {
		t.Fatalf("Begin after expiry: %v", err)
	}
}

func TestMcpOAuthEngineBeginCancellationAbandonsTheTask(t *testing.T) {
	fx := newEngineFixture(t)
	block := make(chan struct{})
	arrived := make(chan struct{}, 1)
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.prmBlock = block
		f.prmArrived = arrived
	})

	ctx, cancel := context.WithCancel(context.Background())
	type beginResult struct {
		d   LoginDescriptor
		err error
	}
	done := make(chan beginResult, 1)
	go func() {
		d, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpoint()})
		done <- beginResult{d: d, err: err}
	}()

	// Wait until the login is genuinely inside discovery, then hang up.
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the login never reached discovery")
	}
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Begin = %v, want context.Canceled", res.err)
		}
		if res.d.LoginID != "" {
			t.Fatalf("a cancelled Begin returned login id %q", res.d.LoginID)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Begin did not return after its context was cancelled")
	}
	close(block)
	waitForGroupDone(t, &fx.eng.wg)
	if len(fx.eng.flows) != 0 {
		t.Fatalf("a cancelled Begin left %d flows behind", len(fx.eng.flows))
	}
}

func TestMcpOAuthEngineCompleteCancellationAbandonsTheTask(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	block := make(chan struct{})
	arrived := make(chan struct{}, 1)
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.tokenBlock = block
		f.tokenArrived = arrived
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fx.eng.Complete(ctx, fx.owner, d.LoginID, fx.oauth.callback(d.AuthorizeURL))
		done <- err
	}()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the login never reached the token endpoint")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Complete did not return after its context was cancelled")
	}
	close(block)
	waitForGroupDone(t, &fx.eng.wg)
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("an abandoned login committed a grant")
	}
}

func TestMcpOAuthEngineCloseTerminatesTasks(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	if err := fx.eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitForGroupDone(t, &fx.eng.wg)
	if _, err := fx.complete(d.LoginID, fx.oauth.callback(d.AuthorizeURL)); !errors.Is(err, ErrMissing) {
		t.Fatalf("Complete after Close = %v, want ErrMissing", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpoint()}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Begin after Close = %v, want ErrClosed", err)
	}
}

func TestMcpOAuthEngineStaleFenceRefusesTheCommit(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	// Another daemon instance logged this endpoint out while the human was in the
	// browser: the fence moved, and this login must not recreate the grant.
	fx.store.advanceEndpoint(fx.owner, fx.oauth.endpoint())

	_, err := fx.complete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("Complete = %v, want ErrStaleFence", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a stale login recreated a grant")
	}
	// The token it had already obtained is offered back to the authorization
	// server, since nothing local will ever use it.
	if got := fx.oauth.revokeCount(); got == 0 {
		t.Fatal("provisional token material was not revoked after a refused commit")
	}
}

func TestMcpOAuthEngineTombstonedOwnerRefusesTheCommit(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	// The user was removed mid-login. The live-user check on the credential path
	// is the fail-closed authority even when cleanup never ran.
	fx.store.deleteOwner(fx.owner)

	if _, err := fx.complete(d.LoginID, fx.oauth.callback(d.AuthorizeURL)); err == nil {
		t.Fatal("Complete succeeded for a tombstoned owner")
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was stored for a tombstoned owner")
	}
}

func TestMcpOAuthEngineNoGrantWhenToolsCannotBeListed(t *testing.T) {
	fx := newEngineFixture(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.failTools = true })

	d := fx.begin()
	if _, err := fx.complete(d.LoginID, fx.oauth.callback(d.AuthorizeURL)); err == nil {
		t.Fatal("Complete succeeded although the tools could not be listed")
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed although the tools could not be listed")
	}
	if got := fx.oauth.revokeCount(); got == 0 {
		t.Fatal("the provisional token was not revoked")
	}
}

func TestMcpOAuthEngineRegistersAfreshForEveryLogin(t *testing.T) {
	fx := newEngineFixture(t)

	first := fx.begin()
	fx.mustComplete(first.LoginID, fx.oauth.callback(first.AuthorizeURL))
	grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("first login committed no grant")
	}
	if got, want := grant.Generation, int64(1); got != want {
		t.Fatalf("first grant generation = %d, want %d", got, want)
	}

	second := fx.begin()
	fx.mustComplete(second.LoginID, fx.oauth.callback(second.AuthorizeURL))
	next, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("second login committed no grant")
	}
	if got, want := next.Generation, int64(2); got != want {
		t.Fatalf("second grant generation = %d, want %d (a new login advances it)", got, want)
	}
	if got, want := fx.oauth.dcrCount(), 2; got != want {
		t.Fatalf("registrations = %d, want %d (never reused across logins)", got, want)
	}
	if next.Registration.ClientID == grant.Registration.ClientID {
		t.Fatal("the second login reused the first registration")
	}

	// A different owner gets its own registration and leaves this one alone.
	other := "44444444-4444-4444-4444-444444444444"
	d := fx.beginFor(other, fx.oauth.endpoint())
	_, _ = fx.eng.Complete(context.Background(), other, d.LoginID, fx.oauth.callback(d.AuthorizeURL))
	if got, want := fx.oauth.dcrCount(), 3; got != want {
		t.Fatalf("registrations = %d, want %d", got, want)
	}
	kept, _ := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if kept.Registration.ClientID != next.Registration.ClientID {
		t.Fatal("another owner's login changed this owner's registration")
	}
}

func TestMcpOAuthEngineDeleteCancelsThePendingLogin(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Endpoint: fx.oauth.endpoint()}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitForGroupDone(t, &fx.eng.wg)
	if _, err := fx.complete(d.LoginID, fx.oauth.callback(d.AuthorizeURL)); !errors.Is(err, ErrMissing) {
		t.Fatalf("Complete after Delete = %v, want ErrMissing", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed after the endpoint was logged out")
	}
	// Logging out an endpoint that was never bound is a local success.
	if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Endpoint: fx.oauth.endpointAt("never")}); err != nil {
		t.Fatalf("Delete of an unbound endpoint: %v", err)
	}
}

func TestMcpOAuthEngineDeleteSelectorIsExclusive(t *testing.T) {
	fx := newEngineFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{}); err == nil {
		t.Fatal("Delete accepted an empty selector")
	}
	err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Endpoint: fx.oauth.endpoint(), Resource: fx.oauth.endpoint()})
	if err == nil {
		t.Fatal("Delete accepted both selectors")
	}
	if !errors.Is(err, ErrMissing) {
		// An absent resource-form grant is ErrMissing; a selector error is not.
		if err := fx.eng.Delete(ctx, fx.owner, DeleteSelector{Resource: "http://127.0.0.1:1/absent"}); err == nil {
			t.Fatal("Delete of an absent resource succeeded")
		}
	}
}

func TestMcpOAuthEngineCancelOwnerIsOwnerBound(t *testing.T) {
	fx := newEngineFixture(t)
	a := fx.begin()
	other := "55555555-5555-5555-5555-555555555555"
	b := fx.beginFor(other, fx.oauth.endpoint())

	if got := fx.eng.CancelOwner("66666666-6666-6666-6666-666666666666"); got != 0 {
		t.Fatalf("CancelOwner of an uninvolved owner cancelled %d flows", got)
	}
	if got := fx.eng.CancelOwner(fx.owner); got != 1 {
		t.Fatalf("CancelOwner cancelled %d flows, want 1", got)
	}
	if _, err := fx.complete(a.LoginID, fx.oauth.callback(a.AuthorizeURL)); !errors.Is(err, ErrMissing) {
		t.Fatalf("Complete after CancelOwner = %v, want ErrMissing", err)
	}
	// The other owner's login is untouched.
	if _, err := fx.eng.Complete(context.Background(), other, b.LoginID, fx.oauth.callback(b.AuthorizeURL)); err != nil {
		t.Fatalf("the other owner's login was disturbed: %v", err)
	}
}

func TestMcpOAuthEngineCallbackLimits(t *testing.T) {
	fx := newEngineFixture(t)
	d := fx.begin()

	big := strings.Repeat("a", MaxCallbackQueryBytes+1)
	if _, err := fx.complete(d.LoginID, "code="+big); !errors.Is(err, ErrCallbackTooLarge) {
		t.Fatalf("oversized callback = %v, want ErrCallbackTooLarge", err)
	}
	if _, err := fx.complete(d.LoginID, "code=%zz"); !errors.Is(err, ErrInvalidCallback) {
		t.Fatalf("malformed callback = %v, want ErrInvalidCallback", err)
	}
	if _, err := fx.complete(d.LoginID, "error=access_denied&error_description=the+user+refused"); err == nil {
		t.Fatal("Complete accepted an error callback")
	} else {
		var authErr *AuthorizationError
		if !errors.As(err, &authErr) {
			t.Fatalf("error callback = %v, want an AuthorizationError", err)
		}
		if authErr.Code != "access_denied" {
			t.Fatalf("authorization error code = %q, want access_denied", authErr.Code)
		}
		if strings.Contains(err.Error(), "the user refused") {
			t.Fatal("the error repeated the provider's free-text description")
		}
	}
}

func TestMcpOAuthEngineSecretsNeverReachErrors(t *testing.T) {
	fx := newEngineFixture(t)
	// Make the exchange fail the way a real refusal does: the token endpoint
	// answers 400 with an error body that quotes the request.
	fx.oauth.set(func(f *fakeOAuthServer) { f.dropCodes = true })

	d := fx.begin()
	query := fx.oauth.callback(d.AuthorizeURL)
	cb, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	code := cb.Get("code")

	_, completeErr := fx.complete(d.LoginID, query)
	if completeErr == nil {
		t.Fatal("Complete succeeded although the exchange was refused")
	}
	haystacks := []string{completeErr.Error(), fx.logs.String()}
	for _, hay := range haystacks {
		for _, secret := range []string{code, cb.Get("state"), "pkce mismatch", "access-1", "refresh-1", CallbackURL} {
			if strings.Contains(hay, secret) {
				t.Fatalf("response leaked %q: %s", secret, hay)
			}
		}
	}
}

func TestMcpOAuthEngineScrubsAnOAuthRetrieveError(t *testing.T) {
	inner := &oauth2.RetrieveError{ErrorCode: "invalid_grant", Body: []byte(`{"error":"invalid_grant","error_description":"secret-ish body"}`)}
	err := upstreamFailure("authenticating to https://mcp.example/mcp", fmt.Errorf("token exchange failed: %w", inner))
	if err == nil {
		t.Fatal("upstreamFailure dropped the error")
	}
	if strings.Contains(err.Error(), "secret-ish body") {
		t.Fatalf("classified error kept the response body: %v", err)
	}
	// A protocol code is a code, not a body: it survives, because it is the part a
	// human can act on.
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("classified error lost the protocol code: %v", err)
	}
}

func TestMcpOAuthEngineChangedIssuerIsAcceptedOnlyByANewLogin(t *testing.T) {
	fx := newEngineFixture(t)
	// A second origin that acts only as the authorization server: the resource
	// metadata names it, so the SDK discovers, registers and exchanges there.
	as := newFakeOAuthServer(t)
	fx.oauth.set(func(f *fakeOAuthServer) { f.issuerURL = as.issuer() })
	// The resource server accepts what its authorization server issued: in
	// production they share a token registry, and here they share this one.
	fx.oauth.issueToken("access-1", "read", "write", "offline_access")

	first := fx.begin()
	// The browser's code must be issued by the authorization server the metadata
	// named, not by the resource's origin.
	res := fx.mustComplete(first.LoginID, as.callback(first.AuthorizeURL))
	if res.Continuation != nil {
		t.Fatal("unexpected continuation")
	}
	grant, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("no grant")
	}
	if got, want := grant.Issuer, as.issuer(); got != want {
		t.Fatalf("issuer = %q, want the metadata's authorization server %q", got, want)
	}
	if got, want := as.dcrCount(), 1; got != want {
		t.Fatalf("registrations at the discovered issuer = %d, want %d", got, want)
	}
	if as.dcrCount() > 0 && grant.Registration.ClientID != as.dcrClient(0) {
		t.Fatalf("committed client = %q, want the discovered issuer's %q", grant.Registration.ClientID, as.dcrClient(0))
	}

	// The resource now names the original origin instead. Only an explicit login
	// may adopt a different issuer — nothing re-points the stored grant.
	fx.oauth.set(func(f *fakeOAuthServer) { f.issuerURL = "" })
	second := fx.begin()
	fx.mustComplete(second.LoginID, fx.oauth.callback(second.AuthorizeURL))
	next, ok := fx.store.grant(fx.owner, fx.oauth.endpoint())
	if !ok {
		t.Fatal("no grant after the second login")
	}
	if got, want := next.Issuer, fx.oauth.issuer(); got != want {
		t.Fatalf("issuer = %q, want %q", got, want)
	}
	if next.Generation <= grant.Generation {
		t.Fatalf("generation = %d, want it to advance past %d", next.Generation, grant.Generation)
	}
	if got, want := as.dcrCount(), 1; got != want {
		t.Fatalf("the old issuer was registered again (%d times)", got)
	}
}

func TestMcpOAuthEngineANewLoginRetiresTheOtherEndpointsBinding(t *testing.T) {
	fx := newEngineFixture(t)
	resource := fx.oauth.endpoint()
	other := fx.oauth.endpointAt("b")
	// Both endpoints are bound to the same resource, as a second path serving the
	// same protected resource would be.
	fx.store.seed(fx.owner, other, resource, fx.oauth.issuer(),
		Token{AccessToken: "old", Expiry: time.Now().Add(time.Hour)})
	fx.store.bindEndpoint(fx.owner, fx.oauth.endpoint(), resource, 1)

	d := fx.begin()
	fx.mustComplete(d.LoginID, fx.oauth.callback(d.AuthorizeURL))

	// The login bound its own endpoint to the new generation ...
	if _, err := fx.store.LookupBinding(context.Background(), fx.owner, fx.oauth.endpoint()); err != nil {
		t.Fatalf("the logged-in endpoint has no binding: %v", err)
	}
	// ... and the other endpoint's binding is stale rather than silently moved.
	if _, err := fx.store.LookupBinding(context.Background(), fx.owner, other); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("the older binding = %v, want ErrLoginRequired (a new login must not adopt it)", err)
	}
}

func TestMcpOAuthEngineRefusesAConsentURLOutsideThePolicy(t *testing.T) {
	fx := newEngineFixture(t)
	// The SDK accepts this: "localhost" is a loopback address, so its own
	// HTTPS-or-loopback check passes. The daemon's policy does not — the operator
	// allowed 127.0.0.1 by address and allowed no cleartext for "localhost" — and
	// a browser must not be sent anywhere the daemon did not validate.
	fx.oauth.set(func(f *fakeOAuthServer) {
		f.authorizationEndpoint = "http://localhost" + portOf(f.srv.URL) + "/authorize"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpoint()})
	if err == nil {
		t.Fatalf("Begin returned consent URL %q for an endpoint outside the policy", d.AuthorizeURL)
	}
	if d.AuthorizeURL != "" {
		t.Fatalf("a refused login still returned %q", d.AuthorizeURL)
	}
	if !errors.Is(err, ErrDestinationDenied) && !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("Begin = %v, want a destination refusal", err)
	}
	if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
		t.Fatal("a grant was committed for a refused consent URL")
	}
	if got := fx.engineFlows(); got != 0 {
		t.Fatalf("%d flows survived a refused consent URL", got)
	}
}

// engineFlows reports how many flows the engine still tracks.
func (fx *engineFixture) engineFlows() int {
	fx.eng.mu.Lock()
	defer fx.eng.mu.Unlock()
	return len(fx.eng.flows)
}

// portOf returns the ":port" of an absolute URL, or "" when it has none.
func portOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return ""
	}
	return ":" + u.Port()
}

func TestMcpOAuthEngineValidatesTheConsentURLItPublishes(t *testing.T) {
	policy := publicPolicy(t)
	discovery := validatedDiscovery{AuthorizationEndpoint: "https://as.example/authorize"}

	cases := []struct {
		name    string
		url     string
		refused bool
	}{
		{
			// The query is the part a consent URL legitimately adds: client id, PKCE
			// challenge, state, resource, scopes.
			name: "same endpoint with consent parameters",
			url:  "https://as.example/authorize?client_id=c1&code_challenge=x&state=s&resource=https%3A%2F%2Fmcp.example%2Fmcp",
		},
		{
			name:    "another host",
			refused: true,
			url:     "https://elsewhere.example/authorize?client_id=c1",
		},
		{
			name:    "another path",
			refused: true,
			url:     "https://as.example/consent?client_id=c1",
		},
		{
			name:    "cleartext on a host the operator did not allow",
			refused: true,
			url:     "http://mcp.example/authorize?client_id=c1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConsentURL(context.Background(), policy, tc.url, discovery)
			if tc.refused {
				if err == nil {
					t.Fatal("validateConsentURL accepted a URL it must refuse")
				}
				return
			}
			if err != nil {
				t.Fatalf("validateConsentURL = %v, want the URL accepted", err)
			}
		})
	}

	// A divergence between what discovery validated and what the SDK built is
	// refused with both sides named, so the failure is diagnosable without
	// guessing which one moved.
	err := validateConsentURL(context.Background(), policy, "https://elsewhere.example/authorize", discovery)
	if err == nil {
		t.Fatal("validateConsentURL accepted a URL from another origin")
	}
	if !strings.Contains(err.Error(), "as.example") || !strings.Contains(err.Error(), "elsewhere.example") {
		t.Fatalf("error = %v, want it to name both endpoints", err)
	}

	// The destination check is the policy's own, including the address the host
	// resolves to: a public name that resolves to a private address is refused.
	private := newResolver().answer("mcp.example", "10.0.0.5")
	strict := testPolicy(t, Config{}, private, nil)
	if err := validateConsentURL(context.Background(), strict, "https://mcp.example/authorize", validatedDiscovery{
		AuthorizationEndpoint: "https://mcp.example/authorize",
	}); !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("validateConsentURL = %v, want ErrDestinationDenied for a private address", err)
	}
}

func TestMcpOAuthEngineNeverRepeatsARegistrationResponseBody(t *testing.T) {
	const sentinel = "sentinel-client-secret-9f2c"
	for _, mode := range []string{"error", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			fx := newEngineFixture(t)
			fx.oauth.set(func(f *fakeOAuthServer) {
				f.dcrFailure = mode
				f.dcrSentinel = sentinel
			})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := fx.eng.Begin(ctx, BeginRequest{OwnerUserID: fx.owner, Server: "fixture", Endpoint: fx.oauth.endpoint()})
			if err == nil {
				t.Fatal("Begin succeeded although registration failed")
			}
			// The SDK's DCR errors embed the raw registration response body — which
			// is where a client secret is issued. None of it may leave this engine.
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("the registration body reached the caller: %v", err)
			}
			if strings.Contains(fx.logs.String(), sentinel) {
				t.Fatalf("the registration body reached the log: %s", fx.logs.String())
			}
			// It is still a usable message: it names the step that failed.
			if !strings.Contains(err.Error(), "authenticating to") {
				t.Fatalf("error = %v, want an authored step description", err)
			}
			if _, ok := fx.store.grant(fx.owner, fx.oauth.endpoint()); ok {
				t.Fatal("a grant was committed after a failed registration")
			}
		})
	}
}

func TestMcpOAuthEngineRevokeDetachedIsBestEffort(t *testing.T) {
	fx := newEngineFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Nothing to revoke: an empty batch is not an error.
	if err := fx.eng.RevokeDetached(ctx, nil); err != nil {
		t.Fatalf("empty batch = %v, want nil", err)
	}

	revocation := Revocation{
		OwnerUserID: fx.owner,
		Resource:    fx.oauth.endpoint(),
		Issuer:      fx.oauth.issuer(),
		Registration: Registration{
			ClientID:           "client-1",
			AuthStyle:          AuthStyleInParams,
			RevocationEndpoint: fx.oauth.srv.URL + "/revoke",
		},
		RefreshToken: "refresh-secret-value",
		AccessToken:  "access-secret-value",
	}

	// A refusal is reported and changes nothing.
	fx.oauth.set(func(f *fakeOAuthServer) { f.revokeStatus = http.StatusInternalServerError })
	err := fx.eng.RevokeDetached(ctx, []Revocation{revocation})
	if err == nil {
		t.Fatal("RevokeDetached reported success for a refused revocation")
	}
	if strings.Contains(err.Error(), "refresh-secret-value") || strings.Contains(err.Error(), "access-secret-value") {
		t.Fatalf("the revocation error quoted token material: %v", err)
	}
	if got := fx.oauth.revokeCount(); got != 1 {
		t.Fatalf("revocations attempted = %d, want 1", got)
	}
	// It never touches the store: the local invalidation already committed.
	if calls := fx.store.callLog(); len(calls) != 0 {
		t.Fatalf("RevokeDetached touched the store: %v", calls)
	}

	// A revocation with no advertised endpoint is skipped, not guessed.
	silent := revocation
	silent.Registration.RevocationEndpoint = ""
	if err := fx.eng.RevokeDetached(ctx, []Revocation{silent}); err != nil {
		t.Fatalf("a revocation with no endpoint = %v, want nil", err)
	}
	if got := fx.oauth.revokeCount(); got != 1 {
		t.Fatalf("revocations attempted = %d, want the skipped one not to have been sent", got)
	}

	// The good one is attempted even when its neighbour is refused, and the
	// failure never stops the batch.
	fx.oauth.set(func(f *fakeOAuthServer) { f.revokeStatus = 0 })
	both := []Revocation{revocation, silent, revocation}
	if err := fx.eng.RevokeDetached(ctx, both); err != nil {
		t.Fatalf("batch with a refused neighbour = %v, want nil", err)
	}
	if got := fx.oauth.revokeCount(); got != 3 {
		t.Fatalf("revocations attempted = %d, want 2 more", got)
	}
}
