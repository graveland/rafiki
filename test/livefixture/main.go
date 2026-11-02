// SPDX-License-Identifier: Apache-2.0

// Command livefixture is the loopback stand-in the Rust SDK's live-daemon MCP
// OAuth tests need: one origin that is simultaneously a protected MCP resource
// and the authorization server that protects it.
//
// It exists because a real authorization server (Linear, GitHub, …) is a live
// third party with a real account behind it, and the live SDK run must work
// from a clean checkout with no such account, no traffic to one and no
// credentials. Everything the daemon's login flow actually does happens here
// over real HTTP, against a real MCP SDK server:
//
//	RFC 9728  protected resource metadata    GET  /.well-known/oauth-protected-resource[/mcp]
//	RFC 8414  authorization server metadata   GET  /.well-known/oauth-authorization-server
//	RFC 7591  dynamic client registration     POST /register
//	          authorization (scripted)        GET  /authorize
//	          code exchange + refresh         POST /token
//	RFC 7009  revocation                      POST /revoke
//	          the MCP resource itself         ANY  /mcp (streamable HTTP, one tool)
//
// The authorization endpoint completes the grant itself: it answers with a 302
// to the daemon's loopback callback carrying the state it was given, a
// PKCE-bound single-use code and the RFC 9207 iss parameter — exactly the
// redirect a browser would follow. The Rust test plays the browser by reading
// that Location header, so nothing here needs a person, a window or a click.
//
// On startup it writes the two files a run needs — mcp.json naming the
// endpoint, and the [mcp_oauth] snippet an operator's rafikid.toml needs to
// reach a loopback cleartext origin at all — then prints their paths and the
// RAFIKI_SDK_TEST_* values the Rust tests read, as shell `export` lines on
// stdout. Everything else goes to stderr, so `eval "$(livefixture -dir …)"` is
// safe.
//
// It is a main package under test/: nothing can import it, and no production
// code depends on it. It imports the standard library and the MCP SDK only.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixture is the whole server: metadata, registration, authorization, tokens,
// revocation and the MCP endpoint. Its state is what makes the flow checkable
// (a code is single use, a refresh token dies when it is rotated), not what
// makes the Rust tests pass — they assert the daemon's answers.
type fixture struct {
	base string // "http://127.0.0.1:PORT", the origin everything below is derived from
	log  *log.Logger

	mu      sync.Mutex
	nextSeq int
	codes   map[string]*authCode
	access  map[string][]string // access token → scopes
	refresh map[string]bool     // refresh tokens still exchangeable
}

// authCode is one issued authorization code, single use.
type authCode struct {
	challenge   string
	redirectURI string
	scopes      string
}

func newFixture(base string, logger *log.Logger) *fixture {
	return &fixture{
		base:    base,
		log:     logger,
		codes:   map[string]*authCode{},
		access:  map[string][]string{},
		refresh: map[string]bool{},
	}
}

// ─── routes ─────────────────────────────────────────────────────────────────

func (f *fixture) routes() http.Handler {
	mux := http.NewServeMux()
	// RFC 9728's path form for a resource at /mcp: the metadata lives at
	// /.well-known/oauth-protected-resource/mcp. The root document covers a
	// client that falls back to the origin, and the trailing-slash pattern is
	// the same document one path segment deeper (a client that appends the
	// endpoint path).
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", f.handlePRM)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp/", f.handlePRM)
	mux.HandleFunc("/.well-known/oauth-protected-resource", f.handlePRM)
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.handleASMetadata)
	mux.HandleFunc("/.well-known/openid-configuration", f.handleASMetadata)
	mux.HandleFunc("/register", f.handleRegister)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/revoke", f.handleRevoke)
	mux.Handle("/mcp", f.guard(f.mcpHandler()))
	mux.Handle("/mcp/", f.guard(f.mcpHandler()))
	return mux
}

// mcpHandler is the real MCP server: the daemon opens a session with the
// token it just obtained and lists tools, which is how a login proves the
// credential works. One tool is enough, and one tool is the point — the Rust
// test asserts the verified list is non-empty, not its size.
func (f *fixture) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "rafiki-livefixture", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "the live fixture's one tool; returns its argument",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "echo " + args.Text},
		}}, nil
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{JSONResponse: true})
}

// guard is the protected-resource half: a request without a token this fixture
// issued gets a 401 carrying the resource_metadata challenge a client uses to
// find the authorization server. That is what makes the endpoint "protected"
// rather than merely authenticated by convention.
func (f *fixture) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.scopesOf(r); !ok {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+f.base+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "a bearer token from this fixture is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fixture) scopesOf(r *http.Request) ([]string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return nil, false
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	f.mu.Lock()
	defer f.mu.Unlock()
	scopes, ok := f.access[token]
	return scopes, ok
}

// ─── metadata ───────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The client is gone if this fails; there is nothing left to report to.
	_ = json.NewEncoder(w).Encode(body)
}

// handlePRM answers the protected-resource metadata. Two documents, one origin,
// because a client may ask at either location: the RFC 9728 path form for the
// /mcp endpoint is what the SDK fetches (it describes the endpoint, which is
// the resource the login binds), while the well-known ROOT document describes
// the origin. Answering the root document with the endpoint's resource would
// be a mismatch for the resource the root was requested for.
func (f *fixture) handlePRM(w http.ResponseWriter, r *http.Request) {
	resource := f.base + "/mcp"
	if r.URL.Path == "/.well-known/oauth-protected-resource" {
		resource = f.base
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              resource,
		"authorization_servers": []string{f.base},
		"scopes_supported":      []string{"read"},
	})
}

func (f *fixture) handleASMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                f.base,
		"authorization_endpoint":                f.base + "/authorize",
		"token_endpoint":                        f.base + "/token",
		"registration_endpoint":                 f.base + "/register",
		"revocation_endpoint":                   f.base + "/revoke",
		"scopes_supported":                      []string{"read", "offline_access"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		// RFC 9207. Advertised because the daemon's discovery validates the
		// iss parameter against this issuer when a server says it emits one;
		// advertising it here is what makes the round trip realistic.
		"authorization_response_iss_parameter_supported": true,
	})
}

func (f *fixture) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.refuse(w, "dynamic client registration: the body is not JSON: %v", err)
		return
	}
	f.mu.Lock()
	f.nextSeq++
	clientID := fmt.Sprintf("client-%d", f.nextSeq)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              body["redirect_uris"],
		"grant_types":                body["grant_types"],
		"token_endpoint_auth_method": "none",
	})
}

// ─── authorization ──────────────────────────────────────────────────────────

// handleAuthorize is the browser-facing half, scripted: it validates the
// request the daemon's OAuth client built (a code request with state and PKCE
// S256), records a single-use code bound to the challenge, and answers with
// the redirect a consenting user's browser would receive.
func (f *fixture) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		f.refuse(w, "authorization: the request names no redirect_uri")
		return
	}

	answer := func(values url.Values) {
		u, err := url.Parse(redirect)
		if err != nil {
			f.refuse(w, "authorization: redirect_uri %q is not a URL: %v", redirect, err)
			return
		}
		u.RawQuery = values.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}

	if q.Get("response_type") != "code" || q.Get("client_id") == "" || q.Get("state") == "" ||
		q.Get("code_challenge") == "" || !strings.EqualFold(q.Get("code_challenge_method"), "S256") {
		// An error redirect rather than a 400: a caller waiting on the
		// callback learns what went wrong from the query it already reads, and
		// the daemon turns the error into an authored refusal.
		f.log.Printf("authorize: malformed authorization request: %s", q.Encode())
		answer(url.Values{"error": {"invalid_request"}, "state": {q.Get("state")}})
		return
	}

	f.mu.Lock()
	f.nextSeq++
	code := fmt.Sprintf("code-%d", f.nextSeq)
	f.codes[code] = &authCode{
		challenge:   q.Get("code_challenge"),
		redirectURI: redirect,
		scopes:      q.Get("scope"),
	}
	f.mu.Unlock()
	answer(url.Values{
		"code":  {code},
		"state": {q.Get("state")},
		"iss":   {f.base},
	})
}

// ─── tokens ─────────────────────────────────────────────────────────────────

// issue mints one access/refresh pair under the shared sequence, so a token
// name says which issue produced it and a rotation is distinguishable from the
// token it replaced. The caller holds f.mu.
func (f *fixture) issue(scopes string) (access, refresh string) {
	f.nextSeq++
	n := f.nextSeq
	access = fmt.Sprintf("access-%d", n)
	refresh = fmt.Sprintf("refresh-%d", n)
	f.access[access] = strings.Fields(scopes)
	f.refresh[refresh] = true
	return access, refresh
}

func (f *fixture) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.refuse(w, "token: the body is not a form: %v", err)
		return
	}
	form := r.PostForm

	switch form.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		code, ok := f.codes[form.Get("code")]
		delete(f.codes, form.Get("code")) // single use, whatever happens next
		f.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid_grant", "error_description": "pkce mismatch",
			})
			return
		}
		if form.Get("redirect_uri") != code.redirectURI {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid_grant", "error_description": "redirect_uri mismatch",
			})
			return
		}
		f.mu.Lock()
		access, refresh := f.issue(code.scopes)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         code.scopes,
		})
	case "refresh_token":
		presented := form.Get("refresh_token")
		f.mu.Lock()
		ok := f.refresh[presented]
		if ok {
			// ROTATION: the presented token dies with this answer and only its
			// successor can refresh again. A client that kept the old one is
			// stuck, which is what makes "the refresh used the stored
			// rotation" observable rather than assumed.
			delete(f.refresh, presented)
		}
		access, next := "", ""
		if ok {
			access, next = f.issue("read")
		}
		f.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  access,
			"refresh_token": next,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "read",
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (f *fixture) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.refuse(w, "revoke: the body is not a form: %v", err)
		return
	}
	// RFC 7009 §2.2: the answer is 200 even for a token this server never
	// issued, so a caller cannot use revocation as an oracle. Nothing is
	// invalidated here: the grant the daemon revokes belongs to a fixture this
	// run is about to discard.
	writeJSON(w, http.StatusOK, map[string]any{})
}

// refuse answers a request this fixture cannot honour, naming the reason on
// stderr as well as in the body: the driver prints this log when a run fails,
// and "the fixture said no" with no reason is exactly the kind of failure this
// file exists to avoid.
func (f *fixture) refuse(w http.ResponseWriter, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	f.log.Print(msg)
	http.Error(w, msg, http.StatusBadRequest)
}

// ─── startup ────────────────────────────────────────────────────────────────

// The names the Rust tests and the driver read.
const (
	mcpJSONName   = "mcp.json"
	mcpOAuthName  = "rafikid-mcp-oauth.toml"
	serverName    = "fixture"
	runDirectory  = 0o700
	configFileMod = 0o600
)

func main() {
	dir := flag.String("dir", "", "directory to write mcp.json and the [mcp_oauth] snippet into (required)")
	addr := flag.String("addr", "127.0.0.1:0", "loopback address to listen on")
	flag.Parse()

	logger := log.New(os.Stderr, "livefixture: ", log.LstdFlags|log.Lmsgprefix)
	if *dir == "" {
		fail(logger, "-dir is required: it is where the fixture writes mcp.json and the rafikid.toml snippet")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger, *dir, *addr); err != nil {
		fail(logger, "%v", err)
	}
}

func fail(logger *log.Logger, format string, args ...any) {
	logger.Printf(format, args...)
	os.Exit(1)
}

func run(ctx context.Context, logger *log.Logger, dir, addr string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve -dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, runDirectory); err != nil {
		return fmt.Errorf("create %s: %w", abs, err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	// The address the kernel chose, not the one asked for: port 0 is the
	// normal case, and every URL below is derived from this.
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("listen on %s: not a TCP address", addr)
	}
	base := "http://" + net.JoinHostPort(tcp.IP.String(), fmt.Sprintf("%d", tcp.Port))

	f := newFixture(base, logger)
	srv := &http.Server{Handler: f.routes(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	if err := f.writeConfig(abs); err != nil {
		_ = srv.Close()
		return err
	}
	printExports(abs, base)

	logger.Printf("serving the MCP resource and its authorization server on %s", base)

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	// SIGTERM/SIGINT: close the listener and let in-flight requests finish.
	// A fixture that leaves the port bound would make the next run fail.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Printf("stopped")
	return nil
}

// writeConfig writes the two files a run needs. mcp.json names the endpoint;
// the second file is the [mcp_oauth] snippet an operator's rafikid.toml needs
// for a loopback cleartext origin to be reachable at all — the daemon's
// default policy is public HTTPS only, and no flag widens it.
func (f *fixture) writeConfig(dir string) error {
	mcpJSON, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			serverName: map[string]any{"url": f.base + "/mcp"},
		},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", mcpJSONName, err)
	}
	if err := os.WriteFile(filepath.Join(dir, mcpJSONName), append(mcpJSON, '\n'), configFileMod); err != nil {
		return fmt.Errorf("write %s: %w", mcpJSONName, err)
	}
	snippet := `# Written by test/livefixture. The daemon's default MCP OAuth destination
# policy is public HTTPS only; this section is what lets the live run reach a
# loopback cleartext authorization server on the same host. Nothing else in
# rafikid.toml is needed for the MCP OAuth live tests.
[mcp_oauth]
allow_hosts = ["127.0.0.1"]
allow_http_hosts = ["127.0.0.1"]
`
	if err := os.WriteFile(filepath.Join(dir, mcpOAuthName), []byte(snippet), configFileMod); err != nil {
		return fmt.Errorf("write %s: %w", mcpOAuthName, err)
	}
	return nil
}

// printExports writes the shell the driver evaluates. Values are single-quoted
// with the one escape a single-quoted string needs, so a directory with a space
// or an apostrophe in it cannot break the run.
func printExports(dir, base string) {
	for _, kv := range [][2]string{
		{"RAFIKI_SDK_TEST_MCP_CONFIG", filepath.Join(dir, mcpJSONName)},
		{"RAFIKI_SDK_TEST_MCP_SERVER", serverName},
		{"RAFIKI_SDK_TEST_MCP_RESOURCE", base + "/mcp"},
		// Not a RAFIKI_SDK_TEST_* value: the driver, not the tests, assembles
		// rafikid.toml from this snippet.
		{"RAFIKI_LIVEFIXTURE_RAFIKID_TOML", filepath.Join(dir, mcpOAuthName)},
	} {
		fmt.Printf("export %s=%s\n", kv[0], shellQuote(kv[1]))
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
