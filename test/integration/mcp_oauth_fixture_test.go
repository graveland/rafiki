// SPDX-License-Identifier: Apache-2.0

package integration_test

// The in-process fake authorization server + MCP resource server the MCP OAuth
// end-to-end tests drive, plus the two harnesses built on it: a real daemon
// booted with an operator [mcp_oauth] allowance, and an in-process engine +
// store pair a test can drive without a daemon at all.
//
// Why a fake at all: a real authorization server (Linear, GitHub) is a live
// third party with a real account behind it, and this suite must not depend on
// one, leave a trace on one, or hold its credentials. Everything the daemon's
// flow actually does — RFC 9728 protected-resource discovery, RFC 8414
// authorization-server metadata, RFC 7591 dynamic client registration, the
// authorization redirect with PKCE S256 + RFC 8707 `resource` + RFC 9207 `iss`,
// the code exchange, single-use refresh with rotation, and RFC 7009 revocation
// — happens here over real HTTP against a real MCP SDK server.
//
// Loopback + cleartext is reachable only because the DAEMON is configured with
// an operator `[mcp_oauth]` allowance (allow_hosts/allow_http_hosts for
// 127.0.0.1), written into the isolated XDG config the harness boots with. No
// flag on any command widens the policy, which is the property under test: the
// fixture would be refused without that section.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/mcpauthdb"
	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/secretbox"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// ─── the fake server ─────────────────────────────────────────────────────────

// fakeMCPOAuth is one loopback origin that is simultaneously the protected
// resource (an MCP endpoint over streamable HTTP) and its authorization
// server (PRM, AS metadata, DCR, authorize, token, revocation).
//
// It records what it was asked and what credentials it was shown, because the
// assertions that matter are about traffic the daemon does NOT send: a bearer
// that must not follow a redirect, a configured header that must not reach a
// token endpoint, a discovery request that a passive session must not make.
type fakeMCPOAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex

	// ── metadata knobs ───────────────────────────────────────────────────
	servePRMPath  bool
	servePRMRoot  bool
	serveASM      bool
	malformedPRM  bool
	malformedASM  bool
	prmPathRes    string // overrides the resource the path document declares
	prmRootRes    string // overrides the resource the root document declares
	prmScopes     []string
	asmIssuer     string // overrides the issuer AS metadata advertises
	authorizeURL  string // overrides authorization_endpoint
	tokenURL      string // overrides token_endpoint
	advertiseIss  bool   // advertise + emit the RFC 9207 iss parameter
	authorizeFail string // non-empty: answer the authorize GET with this OAuth error
	dcrFail       bool
	// prmGate holds the path metadata response until it is closed, which is how
	// a test settles a login in mid-discovery. prmAbandoned receives a value
	// when a gated request's context ended before the gate opened, which is the
	// daemon abandoning the request rather than waiting it out.
	prmGate      chan struct{}
	prmAbandoned chan struct{}
	prmArrived   chan struct{}

	// ── MCP knobs ───────────────────────────────────────────────────────
	tools       []string
	requireAuth bool
	// requireScopes are scopes the MCP endpoint demands beyond the granted
	// set; a token without them gets a 403 insufficient_scope challenge.
	requireScopes []string

	// ── token knobs ─────────────────────────────────────────────────────
	expiresIn int

	// ── recorded traffic ────────────────────────────────────────────────
	requests    []string
	pathHeaders map[string][]http.Header
	prmHits     map[string]int
	asmHits     int
	dcrBodies   []map[string]any
	authParams  []url.Values
	tokenForms  []url.Values
	tokenCalls  int
	mcpHeaders  []http.Header
	mcpCalls    []string
	codeSeqs    []string
	mcpMethods  []string
	revoked     []string
	codes       map[string]*fakeAuthCode
	accessToken map[string][]string
	refreshOK   map[string]bool
	accessSeq   []string
	refreshSeq  []string
}

// fakeAuthCode is one issued authorization code, single use.
type fakeAuthCode struct {
	challenge   string
	resource    string
	redirectURI string
	clientID    string
	scope       string
}

// newFakeMCPOAuth builds the fixture. The returned server is closed with the
// test.
func newFakeMCPOAuth(t *testing.T, tools ...string) *fakeMCPOAuth {
	t.Helper()
	if len(tools) == 0 {
		tools = []string{"echo", "whoami"}
	}
	f := &fakeMCPOAuth{
		t:            t,
		servePRMPath: true,
		servePRMRoot: true,
		serveASM:     true,
		prmScopes:    []string{"read"},
		advertiseIss: true,
		tools:        tools,
		requireAuth:  true,
		expiresIn:    3600,
		pathHeaders:  map[string][]http.Header{},
		prmHits:      map[string]int{},
		codes:        map[string]*fakeAuthCode{},
		accessToken:  map[string][]string{},
		refreshOK:    map[string]bool{},
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "mcp-oauth-fixture", Version: "1"}, nil)
	for _, name := range f.tools {
		name := name
		mcpServer.AddTool(&mcp.Tool{
			Name:        name,
			Description: "fixture tool " + name,
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: name + " ok"},
			}}, nil
		})
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", f.handlePRMPath)
	// The same document at any deeper endpoint path: a test that needs a unique
	// resource per run names /mcp/<suffix>, and the metadata for it has to live
	// at the matching well-known path (RFC 9728's path location).
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp/", f.handlePRMPath)
	mux.HandleFunc("/.well-known/oauth-protected-resource", f.handlePRMRoot)
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.handleASM)
	mux.HandleFunc("/.well-known/openid-configuration", f.handleASM)
	mux.HandleFunc("/register", f.handleRegister)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/revoke", f.handleRevoke)
	mux.Handle("/mcp", f.guard(f.interceptMCP(mcpHandler)))
	mux.Handle("/mcp/", f.guard(f.interceptMCP(mcpHandler)))
	f.srv = httptest.NewServer(f.record(mux))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCPOAuth) set(change func(*fakeMCPOAuth)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// endpoint is the canonical MCP endpoint a config entry names.
func (f *fakeMCPOAuth) endpoint() string { return f.srv.URL + "/mcp" }

// endpointAt is a SECOND endpoint on the same origin — a different server
// name, a different configured URL, and (unless a document says otherwise)
// the same resource.
func (f *fakeMCPOAuth) endpointAt(suffix string) string { return f.srv.URL + "/mcp/" + suffix }

func (f *fakeMCPOAuth) issuer() string { return f.srv.URL }

// rootResource is the origin, which is what the ROOT metadata document
// declares as its resource and what a 2025-03-26 fallback would treat as the
// authorization server.
func (f *fakeMCPOAuth) rootResource() string {
	u, err := url.Parse(f.endpoint())
	if err != nil {
		f.t.Fatalf("fixture endpoint: %v", err)
	}
	u.Path = ""
	return u.String()
}

func (f *fakeMCPOAuth) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.pathHeaders[r.URL.Path] = append(f.pathHeaders[r.URL.Path], r.Header.Clone())
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// ── metadata ────────────────────────────────────────────────────────────────

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeMCPOAuth) handlePRMPath(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.prmHits[r.URL.Path]++
	serve, malformed, gate, abandoned, arrived := f.servePRMPath, f.malformedPRM, f.prmGate, f.prmAbandoned, f.prmArrived
	resource := f.prmPathRes
	if resource == "" {
		// This document describes the endpoint at the same path, so the resource
		// is the origin plus the path it was requested under: /mcp for the shared
		// endpoint, /mcp/<suffix> for a per-run one.
		resource = f.srv.URL + strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource")
	}
	scopes := slices.Clone(f.prmScopes)
	servers := []string{f.issuer()}
	f.mu.Unlock()
	if !serve {
		http.Error(w, "no metadata here", http.StatusNotFound)
		return
	}
	if malformed {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resource": `)
		return
	}
	if gate != nil {
		if arrived != nil {
			select {
			case arrived <- struct{}{}:
			default:
			}
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			if abandoned != nil {
				select {
				case abandoned <- struct{}{}:
				default:
				}
			}
			return
		}
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": servers,
		"scopes_supported":      scopes,
	})
}

func (f *fakeMCPOAuth) handlePRMRoot(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.prmHits[r.URL.Path]++
	serve, malformed := f.servePRMRoot, f.malformedPRM
	resource := f.prmRootRes
	if resource == "" {
		resource = f.rootResource()
	}
	scopes := slices.Clone(f.prmScopes)
	f.mu.Unlock()
	if !serve {
		http.Error(w, "no metadata here", http.StatusNotFound)
		return
	}
	if malformed {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resource": `)
		return
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": []string{f.issuer()},
		"scopes_supported":      scopes,
	})
}

func (f *fakeMCPOAuth) handleASM(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asmHits++
	serve, malformed, issuer := f.serveASM, f.malformedASM, f.asmIssuer
	if issuer == "" {
		issuer = f.issuer()
	}
	authorize := f.authorizeURL
	if authorize == "" {
		authorize = f.srv.URL + "/authorize"
	}
	token := f.tokenURL
	if token == "" {
		token = f.srv.URL + "/token"
	}
	scopes := append(slices.Clone(f.prmScopes), "offline_access")
	issSupported := f.advertiseIss
	f.mu.Unlock()
	if !serve {
		http.Error(w, "no metadata here", http.StatusNotFound)
		return
	}
	if malformed {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer": `)
		return
	}
	meta := map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                authorize,
		"token_endpoint":                        token,
		"registration_endpoint":                 f.srv.URL + "/register",
		"revocation_endpoint":                   f.srv.URL + "/revoke",
		"scopes_supported":                      scopes,
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	}
	if issSupported {
		meta["authorization_response_iss_parameter_supported"] = true
	}
	writeFakeJSON(w, http.StatusOK, meta)
}

func (f *fakeMCPOAuth) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad registration body", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.dcrBodies = append(f.dcrBodies, body)
	clientID := fmt.Sprintf("client-%d", len(f.dcrBodies))
	fail := f.dcrFail
	f.mu.Unlock()
	if fail {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	writeFakeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              body["redirect_uris"],
		"grant_types":                body["grant_types"],
		"token_endpoint_auth_method": "none",
	})
}

// ── authorize ───────────────────────────────────────────────────────────────

// handleAuthorize is the browser-facing half: it validates the request the
// client built (PKCE S256, state, redirect_uri, RFC 8707 resource), stores a
// single-use code, and redirects back to the loopback callback with code,
// state and — when advertised — iss.
func (f *fakeMCPOAuth) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.authParams = append(f.authParams, q)
	fail := f.authorizeFail
	issuer := f.issuer()
	redirect := q.Get("redirect_uri")
	f.mu.Unlock()

	answer := func(values url.Values) {
		u, err := url.Parse(redirect)
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		u.RawQuery = values.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if fail != "" {
		answer(url.Values{"error": {fail}, "state": {q.Get("state")}})
		return
	}
	if q.Get("response_type") != "code" || q.Get("client_id") == "" || redirect == "" ||
		q.Get("state") == "" || q.Get("code_challenge") == "" ||
		!strings.EqualFold(q.Get("code_challenge_method"), "S256") {
		http.Error(w, "malformed authorization request: "+q.Encode(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	code := fmt.Sprintf("code-%d", len(f.codes)+1)
	f.codeSeqs = append(f.codeSeqs, code)
	f.codes[code] = &fakeAuthCode{
		challenge:   q.Get("code_challenge"),
		resource:    q.Get("resource"),
		redirectURI: redirect,
		clientID:    q.Get("client_id"),
		scope:       q.Get("scope"),
	}
	f.mu.Unlock()
	values := url.Values{"code": {code}, "state": {q.Get("state")}}
	if f.advertiseIss {
		values.Set("iss", issuer)
	}
	answer(values)
}

// browse plays the browser: it GETs the consent URL, refuses to follow the
// redirect, and returns the redirect URL the authorization server answered
// with. A test then hands its query to Complete (or lets the CLI's listener
// take it).
func (f *fakeMCPOAuth) browse(t *testing.T, authorizeURL string) *url.URL {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	resp, err := client.Get(authorizeURL)
	assert.NewAborting(t).NoError(err, "browse %s", authorizeURL)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	assert.NewAborting(t).Eq(http.StatusFound, resp.StatusCode, "authorize status (body may name a malformed request)")
	location := resp.Header.Get("Location")
	assert.NewAborting(t).NotEq("", location, "authorize answered no Location")
	u, err := url.Parse(location)
	assert.NewAborting(t).NoError(err, "parse redirect %q", location)
	return u
}

// callbackQuery is browse's answer as the query string Complete takes —
// without the leading "?".
func (f *fakeMCPOAuth) callbackQuery(t *testing.T, authorizeURL string) string {
	t.Helper()
	return f.browse(t, authorizeURL).RawQuery
}

// ── token ───────────────────────────────────────────────────────────────────

func (f *fakeMCPOAuth) issueLocked(scope string) (access, refresh string) {
	n := len(f.accessSeq) + 1
	access = fmt.Sprintf("access-%d", n)
	refresh = fmt.Sprintf("refresh-%d", n)
	f.accessSeq = append(f.accessSeq, access)
	f.refreshSeq = append(f.refreshSeq, refresh)
	f.accessToken[access] = strings.Fields(scope)
	f.refreshOK[refresh] = true
	return access, refresh
}

func (f *fakeMCPOAuth) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	f.mu.Lock()
	f.tokenCalls++
	f.tokenForms = append(f.tokenForms, form)
	expires := f.expiresIn
	f.mu.Unlock()

	switch form.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		code, ok := f.codes[form.Get("code")]
		delete(f.codes, form.Get("code"))
		f.mu.Unlock()
		if !ok {
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "pkce mismatch"})
			return
		}
		if form.Get("redirect_uri") != code.redirectURI {
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
			return
		}
		f.mu.Lock()
		access, refresh := f.issueLocked(code.scope)
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    "Bearer",
			"expires_in":    expires,
			"scope":         code.scope,
		})
	case "refresh_token":
		refresh := form.Get("refresh_token")
		f.mu.Lock()
		ok := f.refreshOK[refresh]
		f.mu.Unlock()
		if !ok {
			// Spent, revoked or never issued: the single-use token cannot be
			// replayed, and the store must turn this into a login.
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		f.mu.Lock()
		// ROTATION: the presented token dies with this answer, and only the new
		// one can refresh again. A client that kept the old one is stuck, which
		// is exactly what makes "the later request used the persisted rotation"
		// checkable.
		delete(f.refreshOK, refresh)
		access, next := f.issueLocked("read")
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"refresh_token": next,
			"token_type":    "Bearer",
			"expires_in":    expires,
			"scope":         "read",
		})
	default:
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (f *fakeMCPOAuth) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.revoked = append(f.revoked, r.PostForm.Get("token"))
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// ── the MCP endpoint ────────────────────────────────────────────────────────

func (f *fakeMCPOAuth) scopesOf(r *http.Request) ([]string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return nil, false
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	f.mu.Lock()
	defer f.mu.Unlock()
	scopes, ok := f.accessToken[token]
	return scopes, ok
}

func (f *fakeMCPOAuth) challenge(w http.ResponseWriter, status int, errCode, scope string) {
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

func (f *fakeMCPOAuth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		require := slices.Clone(f.requireScopes)
		needsAuth := f.requireAuth
		f.mu.Unlock()
		if !needsAuth {
			next.ServeHTTP(w, r)
			return
		}
		scopes, ok := f.scopesOf(r)
		if !ok {
			f.challenge(w, http.StatusUnauthorized, "", "")
			return
		}
		if missing := missingStrings(scopes, require); len(missing) > 0 {
			f.challenge(w, http.StatusForbidden, "insufficient_scope", strings.Join(missing, " "))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// interceptMCP records what the MCP endpoint was asked and with which
// headers, which is how a test proves WHICH bearer arrived on a real tool
// invocation rather than inspecting a token source.
func (f *fakeMCPOAuth) interceptMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var method string
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			_ = json.Unmarshal(body, &msg)
			method = msg.Method
			if msg.Method == "tools/call" {
				method += " " + msg.Params.Name
			}
		}
		f.mu.Lock()
		f.mcpHeaders = append(f.mcpHeaders, r.Header.Clone())
		f.mcpMethods = append(f.mcpMethods, method)
		f.mcpCalls = append(f.mcpCalls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func missingStrings(have, want []string) []string {
	var missing []string
	for _, w := range want {
		if !slices.Contains(have, w) {
			missing = append(missing, w)
		}
	}
	return missing
}

// ── recorded traffic, snapshot under the lock ───────────────────────────────

// prmHitsFor is how many times one discovery path was asked for.
func (f *fakeMCPOAuth) prmHitsFor(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prmHits[path]
}

func (f *fakeMCPOAuth) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// probeRequests are the OAuth legs — metadata, registration, authorize, token,
// revocation — with the MCP endpoint itself excluded. A passive session must
// make none of them.
func (f *fakeMCPOAuth) oauthRequests() []string {
	path := func(s string) string {
		if _, p, ok := strings.Cut(s, " "); ok {
			return p
		}
		return s
	}
	var out []string
	for _, r := range f.requestLog() {
		if strings.HasPrefix(path(r), "/mcp") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// headersOnPath is every header set a non-MCP path was shown, in arrival
// order. Discovery, registration, authorize, token and revocation requests must
// never carry a project's configured headers or a bearer.
// headerOnPath is one header's value on every request one path received, in
// arrival order. It is how a test states both halves of the containment rule: a
// configured header DOES reach the MCP endpoint and never reaches an OAuth leg.
func (f *fakeMCPOAuth) headerOnPath(path, key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.pathHeaders[path]))
	for _, h := range f.pathHeaders[path] {
		out = append(out, h.Get(key))
	}
	return out
}

// issuedCodes is every authorization code the fixture handed out, in order.
func (f *fakeMCPOAuth) issuedCodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.codeSeqs)
}

func (f *fakeMCPOAuth) headersOnPath(path string) []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pathHeaders[path])
}

func (f *fakeMCPOAuth) tokenFormSnapshot() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tokenForms)
}

func (f *fakeMCPOAuth) authorizeParamsSnapshot() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.authParams)
}

func (f *fakeMCPOAuth) dcrBodySnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dcrBodies)
}

// bearerOnMCP returns the Authorization header values the MCP endpoint was
// shown, in arrival order ("" for a request that carried none).
func (f *fakeMCPOAuth) bearerOnMCP() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.mcpHeaders))
	for _, h := range f.mcpHeaders {
		out = append(out, h.Get("Authorization"))
	}
	return out
}

// bearerOnPath is the Authorization header of every request one path was shown,
// in arrival order. Unlike bearerOnMCP it is keyed by path, which is how a test
// separates two endpoints of the same origin.
func (f *fakeMCPOAuth) bearerOnPath(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.pathHeaders[path]))
	for _, h := range f.pathHeaders[path] {
		out = append(out, h.Get("Authorization"))
	}
	return out
}

// canonicalRoot is the canonical spelling of the origin, which is what a root
// metadata document's resource becomes once stored.
func (f *fakeMCPOAuth) canonicalRoot(t *testing.T) string {
	t.Helper()
	root, err := mcpoauth.Canonicalize(f.rootResource())
	assert.NewAborting(t).NoError(err, "canonicalize the root resource")
	return root
}

// mcpMethodLog is the JSON-RPC method of every POST the MCP endpoint received,
// so a test can wait for a REAL tool invocation instead of a session handshake.
func (f *fakeMCPOAuth) mcpMethodLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.mcpMethods)
}

// waitToolCall blocks until the MCP endpoint has served n `tools/call <name>`
// invocations and returns the bearer that arrived with the last of them.
func (f *fakeMCPOAuth) waitToolCall(t *testing.T, name string, want int, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		f.mu.Lock()
		seen := 0
		bearer := ""
		for i, m := range f.mcpMethods {
			if m == "tools/call "+name {
				seen++
				bearer = f.mcpHeaders[i].Get("Authorization")
			}
		}
		have := len(f.mcpMethods)
		f.mu.Unlock()
		if seen >= want {
			return bearer
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the MCP endpoint served %d tools/call %s invocation(s), want %d (requests seen: %d)", seen, name, want, have)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fakeMCPOAuth) refreshFormSnapshot() []url.Values {
	var out []url.Values
	for _, form := range f.tokenFormSnapshot() {
		if form.Get("grant_type") == "refresh_token" {
			out = append(out, form)
		}
	}
	return out
}

func (f *fakeMCPOAuth) issued() (access, refresh []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accessSeq), slices.Clone(f.refreshSeq)
}

// waitRevocation reports whether one token was offered to the revocation
// endpoint within d. The logout's revocation is bounded and synchronous with
// the verb, so this normally observes it immediately; the window exists so a
// slower daemon does not make the assertion order-dependent.
func (f *fakeMCPOAuth) waitRevocation(t *testing.T, token string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		f.mu.Lock()
		found := slices.Contains(f.revoked, token)
		all := slices.Clone(f.revoked)
		f.mu.Unlock()
		if found {
			return true
		}
		if !time.Now().Before(deadline) {
			t.Logf("revocations offered: %v (wanted the token used by the grant)", all)
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ─── observability the adversarial cases need ──────────────────────────────

// newCountingDestination is a live TCP listener that counts accepted
// connections and answers nothing useful. It exists to make "this destination
// was never dialled" observable from the far end: the listener is unquestionably
// up, so a count of zero means the daemon refused the destination instead of
// connecting to it.
func newCountingDestination(t *testing.T) (addr string, dials func() int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NewAborting(t).NoError(err, "listen for the counting destination")
	var count atomic.Int64
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no request may reach a refused destination", http.StatusTeapot)
	})}
	go func() { _ = srv.Serve(&countingListener{Listener: ln, count: &count}) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return ln.Addr().String(), count.Load
}

type countingListener struct {
	net.Listener
	count *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.count.Add(1)
	}
	return conn, err
}

// uniqueEndpointSuffix mints a per-run path segment. It is unique in this
// process AND across runs, so the resource derived from it (the endpoint URL
// itself) identifies exactly one run's rows in the shared, never-reset database.
func uniqueEndpointSuffix() string { return "run-" + uniqueSuffix() }

// sweepMCPRows deletes every MCP OAuth row this run created. The endpoint path
// carries the run's unique segment and the document at that path declares the
// endpoint as its resource, so one LIKE covers both tables — which is what keeps
// the shared disposable database from accumulating grants that a later run could
// match.
func sweepMCPRows(t *testing.T, suffix string) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		return
	}
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Errorf("sweep MCP rows for %s: pool: %v", suffix, err)
			return
		}
		defer pool.Close()
		like := "%" + suffix + "%"
		// A failed sweep is a leak in a database that is never reset, so it is
		// reported rather than logged.
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM conversations.mcp_endpoint WHERE endpoint LIKE $1`, like); err != nil {
			t.Errorf("sweep MCP endpoint rows for %s: %v", suffix, err)
		}
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM conversations.mcp_credential WHERE resource LIKE $1`, like); err != nil {
			t.Errorf("sweep MCP credential rows for %s: %v", suffix, err)
		}
	})
}

// ─── mcp.json / rafikid.toml fixtures ───────────────────────────────────────

// writeMCPJSON writes an .mcp.json-shaped file and returns its absolute path.
// The daemon reads this exact file; these tests run on the same host as it.
func writeMCPJSON(t *testing.T, servers map[string]any) string {
	t.Helper()
	c := assert.NewAborting(t)
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	c.NoError(err, "marshal mcp.json")
	path := filepath.Join(t.TempDir(), "mcp.json")
	c.NoError(os.WriteFile(path, b, 0o600), "write mcp.json")
	return path
}

// mcpOAuthDaemonEnv isolates the daemon's config and gives it the operator
// [mcp_oauth] allowance this fixture needs: loopback addresses and cleartext
// HTTP for 127.0.0.1, and nothing else. That is the ONLY reason the daemon can
// reach a loopback HTTP authorization server at all.
func mcpOAuthDaemonEnv(t *testing.T) []string {
	t.Helper()
	c := assert.NewAborting(t)
	dir := t.TempDir()
	cfg := `[mcp_oauth]
allow_hosts = ["127.0.0.1"]
allow_http_hosts = ["127.0.0.1"]
`
	path := filepath.Join(dir, "rafikid.toml")
	c.NoError(os.WriteFile(path, []byte(cfg), 0o600), "write rafikid.toml")
	return []string{
		// The suite's daemons otherwise inherit the developer's XDG_CONFIG_HOME
		// (unset → ~/.config), so an ambient rafikid.toml would be read.
		"XDG_CONFIG_HOME=" + dir,
		"RAFIKI_CONFIG=" + path,
	}
}

// mcpOAuthSecretsKey mints a throwaway RAFIKI_SECRETS_KEY: base64 of exactly 32
// bytes, which is what pkg/secretbox accepts. Per test, so one test's sealed
// rows can never be opened by another's daemon.
func mcpOAuthSecretsKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	assert.NewAborting(t).NoError(err, "read random key")
	return base64.StdEncoding.EncodeToString(key)
}

// bootMCPOAuthDaemon boots a DB-backed daemon with the operator policy, a
// secrets key, and no executor pool. extraEnv entries are appended last and win.
func bootMCPOAuthDaemon(t *testing.T, extraEnv ...string) *daemon {
	t.Helper()
	env := append(mcpOAuthDaemonEnv(t), "RAFIKI_SECRETS_KEY="+mcpOAuthSecretsKey(t))
	env = append(env, "RAFIKI_EXECUTORS_ENABLED=0")
	env = append(env, extraEnv...)
	d := bootDaemonDB(t, nextDaemonID(), env...)
	t.Cleanup(func() {
		// Stop first, then remove: this cleanup runs before bootDaemonDB's own
		// (LIFO), and stopping an already-stopped daemon is a no-op.
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})
	return d
}

// waitStderrContainsAfter is waitStderrContains restricted to text written after
// byte offset from, so an assertion cannot be satisfied by an earlier line that
// happened to carry the same words. The buffer only grows, so from is a stable
// boundary.
func waitStderrContainsAfter(t *testing.T, d *daemon, sub string, from int, timeout time.Duration) string {
	t.Helper()
	if d.stderr == nil {
		t.Fatalf("this daemon's stderr is not captured, so %q cannot be observed", sub)
	}
	deadline := time.Now().Add(timeout)
	for {
		s := d.stderr.String()
		if len(s) >= from && strings.Contains(s[from:], sub) {
			return s
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the daemon logged no %q within %s after byte %d\nnew stderr:\n%s",
				sub, timeout, from, s[min(from, len(s)):])
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ─── in-process harness: engine + store, no daemon ──────────────────────────

// mcpOAuthHarness is an engine and store pair over the shared disposable
// database, with a real live user as its owner. It exists for the properties
// that cannot be injected through a daemon deterministically — a store outage,
// two independent sessions racing one refresh, a logout landing mid-refresh —
// where the engine and store ARE the subject.
type mcpOAuthHarness struct {
	t      *testing.T
	dsn    string
	pool   *pgxpool.Pool
	box    *secretbox.Box
	store  mcpoauth.Store
	engine *mcpoauth.Engine
	policy *mcpoauth.Policy
	owner  string
	key    string
}

// newMcpOAuthHarness builds the harness. The owner is a real user row, because
// every store mutation requires a live owner and refuses a tombstoned one.
func newMcpOAuthHarness(t *testing.T) *mcpOAuthHarness {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool := openPool(t, dsn)
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
	key := mcpOAuthSecretsKey(t)
	box, err := secretbox.New(mustBase64(t, key))
	c.NoError(err, "secretbox")

	owner, _ := createRecallUser(t, pool, "mcp-oauth-"+uniqueSuffix(), false)
	policy, err := mcpoauth.Config{
		AllowHosts:     []string{"127.0.0.1"},
		AllowHTTPHosts: []string{"127.0.0.1"},
	}.Policy()
	c.NoError(err, "policy")
	h := &mcpOAuthHarness{t: t, dsn: dsn, pool: pool, box: box, store: mcpauthdb.New(pool, box), policy: policy, owner: owner, key: key}
	h.engine = h.newEngine(context.Background())
	t.Cleanup(func() {
		_ = h.engine.Close()
		// The grant and endpoint rows reference the user row by UUID and are not
		// cascaded by the tombstone usersdb.Delete writes, so they are swept
		// explicitly: the shared disposable database is never reset.
		//
		// The sweep uses a FRESH pool and reports its own failure: a test is
		// allowed to close the harness's pool (the store-outage case does
		// exactly that), and a sweep that silently ran against a closed pool
		// left one grant and one endpoint row behind on every run.
		sweep, sweepErr := pgxpool.New(context.Background(), dsn)
		if sweepErr != nil {
			t.Errorf("sweep MCP rows for owner %s: pool: %v", owner, sweepErr)
			pool.Close()
			return
		}
		if _, err := sweep.Exec(context.Background(),
			`DELETE FROM conversations.mcp_endpoint WHERE owner_user_id = $1::uuid`, owner); err != nil {
			t.Errorf("sweep MCP endpoint rows for owner %s: %v", owner, err)
		}
		if _, err := sweep.Exec(context.Background(),
			`DELETE FROM conversations.mcp_credential WHERE owner_user_id = $1::uuid`, owner); err != nil {
			t.Errorf("sweep MCP credential rows for owner %s: %v", owner, err)
		}
		sweep.Close()
		pool.Close()
	})
	return h
}

func mustBase64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	assert.NewAborting(t).NoError(err, "decode key")
	return b
}

// newEngine builds an engine over the harness's store with the operator policy
// the daemon would have and no logger.
func (h *mcpOAuthHarness) newEngine(ctx context.Context) *mcpoauth.Engine {
	return h.newEngineWith(ctx, nil)
}

// newEngineWith is newEngine with the clock seam overridden, which is how a
// test makes a pending login expire without waiting ten minutes for it.
func (h *mcpOAuthHarness) newEngineWith(ctx context.Context, after func(time.Duration) <-chan time.Time) *mcpoauth.Engine {
	h.t.Helper()
	e, err := mcpoauth.NewEngine(ctx, mcpoauth.EngineOptions{
		Store:  h.store,
		Policy: h.policy,
		After:  after,
	})
	assert.NewAborting(h.t).NoError(err, "new engine")
	h.t.Cleanup(func() { _ = e.Close() })
	return e
}

// endpointForRun is a per-run unique MCP endpoint on fx. The resource a login
// binds is derived from the endpoint path, so the rows this harness creates are
// identified by THIS run alone — which is what makes an expiry keyed on the
// resource unambiguous in a shared, never-reset database.
func (h *mcpOAuthHarness) endpointForRun(fx *fakeMCPOAuth) string {
	return fx.endpointAt(uniqueEndpointSuffix())
}

// lookup resolves one endpoint's binding without failing the test.
func (h *mcpOAuthHarness) lookup(endpoint string) (mcpoauth.Binding, error) {
	return h.store.LookupBinding(context.Background(), h.owner, endpoint)
}

// binding resolves one endpoint's binding and fails the test when there is none.
func (h *mcpOAuthHarness) binding(endpoint string) mcpoauth.Binding {
	h.t.Helper()
	b, err := h.lookup(endpoint)
	assert.NewAborting(h.t).NoError(err, "LookupBinding(%s)", endpoint)
	return b
}

// secondStore is an INDEPENDENT store over its own pool — not a second
// variable pointing at the same connection. A race test needs that: two
// sessions in one process sharing a pool would serialize on nothing the
// database is doing, and would prove nothing about the row-level protocol.
func (h *mcpOAuthHarness) secondStore() (mcpoauth.Store, *pgxpool.Pool) {
	h.t.Helper()
	pool := openPool(h.t, h.dsn)
	h.t.Cleanup(pool.Close)
	return mcpauthdb.New(pool, h.box), pool
}

// begin starts a login and fails the test on error.
func (h *mcpOAuthHarness) begin(fx *fakeMCPOAuth, server, endpoint string) mcpoauth.LoginDescriptor {
	h.t.Helper()
	d, err := h.engine.Begin(context.Background(), mcpoauth.BeginRequest{
		OwnerUserID: h.owner, Server: server, Endpoint: endpoint,
	})
	assert.NewAborting(h.t).NoError(err, "Begin(%s)", endpoint)
	return d
}

// login runs one whole explicit login: Begin, the browser redirect, Complete.
func (h *mcpOAuthHarness) login(fx *fakeMCPOAuth, server, endpoint string) mcpoauth.CompleteResult {
	h.t.Helper()
	d := h.begin(fx, server, endpoint)
	res, err := h.engine.Complete(context.Background(), h.owner, d.LoginID, fx.callbackQuery(h.t, d.AuthorizeURL))
	assert.NewAborting(h.t).NoError(err, "Complete(%s)", endpoint)
	assert.NewAborting(h.t).Nil(res.Continuation, "unexpected continuation round")
	return res
}

// passive resolves the owner-bound passive handler for one endpoint.
func (h *mcpOAuthHarness) passive(endpoint string, headers map[string]string) (auth.OAuthHandler, *http.Client, error) {
	return h.engine.Passive(h.owner)(context.Background(), "fixture", endpoint, headers)
}

// connectMCP opens a real MCP session over the passive handler and returns the
// session so the caller can list or call tools.
func connectMCP(t *testing.T, endpoint string, handler auth.OAuthHandler, client *http.Client) (*mcp.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "mcp-oauth-it", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             endpoint,
			HTTPClient:           client,
			OAuthHandler:         handler,
			DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, nil
}

// watchRefreshCount reports how many refresh_token requests the fixture has
// served so far.
func (f *fakeMCPOAuth) refreshCount() int { return len(f.refreshFormSnapshot()) }
