// SPDX-License-Identifier: Apache-2.0

package main

// The daemon side of MCP OAuth: the exact-entry resolver the login verbs share,
// the degraded (no key / no database) state, the one-box rule, the owner
// cleanup a user removal owes, the owner binding for a fundi child, and the one
// thing that must NOT exist — protected-resource metadata advertised by rafiki's
// own MCP face.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rafikidconfig"
	"go.graveland.dev/rafiki/pkg/secretbox"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// ─── the exact entry ─────────────────────────────────────────────────────────

// TestMcpServerEndpointRefusesWhatCannotLogIn is the "refuse before discovery"
// rule, entry shape by entry shape. Every case must fail WITHOUT any network
// work: the resolver is pure file inspection, and a case that reached discovery
// would have to be an endpoint the daemon then had to dial.
func TestMcpServerEndpointRefusesWhatCannotLogIn(t *testing.T) {
	c := assert.NewCollecting(t)

	config := `{"mcpServers":{
		"linear":{"url":"https://mcp.example.net/mcp"},
		"stdio":{"command":"node","args":["server.js"]},
		"remote":{"via_executor":{"selector":"kind=native","command":"git"}},
		"nourl":{"command":""},
		"authHeader":{"url":"https://mcp.example.net/mcp","headers":{"Authorization":"Bearer xxx"}},
		"authLower":{"url":"https://mcp.example.net/mcp","headers":{"authorization":"Bearer xxx"}},
		"authEmpty":{"url":"https://mcp.example.net/mcp","headers":{"AUTHORIZATION":""}},
		"otherHeaders":{"url":"https://mcp.example.net/mcp","headers":{"X-Team":"core"}}
	}}`
	path := writeMCPConfig(t, config)

	for _, tc := range []struct {
		name   string
		path   string
		server string
	}{
		{"stdio server", path, "stdio"},
		{"via_executor server", path, "remote"},
		{"entry with no url", path, "nourl"},
		{"explicit Authorization header", path, "authHeader"},
		{"lowercase authorization header", path, "authLower"},
		{"empty-valued Authorization header", path, "authEmpty"},
		{"server the file does not declare", path, "absent"},
		{"relative config path", ".mcp.json", "linear"},
		{"empty config path", "", "linear"},
		{"empty server name", path, ""},
		{"config file that does not exist", filepath.Join(t.TempDir(), "nope.json"), "linear"},
	} {
		_, err := mcpServerEndpoint(tc.path, tc.server)
		c.Require().Error(err, "%s: must be refused", tc.name)
		var ce *connectapi.ControllerError
		c.Require().True(errors.As(err, &ce), "%s: want a ControllerError, got %T", tc.name, err)
		c.Eq(protocol.ErrInvalidArgs, ce.Code, "%s: code", tc.name)
		c.False(strings.Contains(ce.Message, "xxx"), "%s: the refusal must not quote a header value: %s", tc.name, ce.Message)
	}

	// The Authorization refusal names the header the user must remove, and says
	// why: an explicit header disables stored OAuth, so a login would store a
	// grant nothing spends.
	_, err := mcpServerEndpoint(path, "authEmpty")
	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "authEmpty: ControllerError")
	c.StrContains(ce.Message, "AUTHORIZATION", "the refusal names the exact spelling: %s", ce.Message)
	c.StrContains(ce.Message, "remove the header", "the refusal names the fix: %s", ce.Message)

	// Unrelated headers are not a reason to refuse: they coexist with OAuth.
	endpoint, err := mcpServerEndpoint(path, "otherHeaders")
	c.NoError(err, "unrelated headers must not disable a login")
	c.Eq("https://mcp.example.net/mcp", endpoint, "the configured URL is returned verbatim")

	endpoint, err = mcpServerEndpoint(path, "linear")
	c.NoError(err, "a plain direct-url entry")
	c.Eq("https://mcp.example.net/mcp", endpoint, "endpoint")
}

// TestMcpServerEndpointDoesNotVetTheURL: a URL's presence in a project file
// says nothing about whether the daemon may reach it. The resolver returns it
// verbatim — canonicalisation and the operator's destination policy are the
// engine's — so a loopback or plaintext endpoint is refused by [mcp_oauth], not
// by "the file said so".
func TestMcpServerEndpointDoesNotVetTheURL(t *testing.T) {
	c := assert.NewCollecting(t)
	path := writeMCPConfig(t, `{"mcpServers":{"local":{"url":"http://127.0.0.1:8080/mcp"}}}`)
	endpoint, err := mcpServerEndpoint(path, "local")
	c.NoError(err, "the resolver performs no destination check")
	c.Eq("http://127.0.0.1:8080/mcp", endpoint, "the URL is returned as configured")
}

// TestMcpServerEndpointRefusesAMalformedConfig keeps the parse error a
// caller's problem (InvalidArgument) rather than an internal one, and keeps the
// file's content out of it.
func TestMcpServerEndpointRefusesAMalformedConfig(t *testing.T) {
	c := assert.NewCollecting(t)
	path := writeMCPConfig(t, `{"mcpServers": {`)
	_, err := mcpServerEndpoint(path, "linear")
	c.Require().Error(err, "a malformed config")
	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "want a ControllerError, got %T", err)
	c.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	c.StrContains(ce.Message, path, "the refusal names the file: %s", ce.Message)
}

// ─── the engine seam ─────────────────────────────────────────────────────────

// fakeMcpStore is an mcpoauth.Store double: enough to build a real Engine and to
// observe what the daemon asks of it. The durable semantics are pkg/mcpauthdb's
// and are pinned against a real database there; this pins the daemon's calls.
type fakeMcpStore struct {
	mu sync.Mutex

	invalidatedOwners []string
	invalidateErr     error
	revocations       []mcpoauth.Revocation

	lookupBindingErr  error
	invalidateCalls   int
	beginFenceCalls   int
	invalidatedOfUser map[string]bool
}

var _ mcpoauth.Store = (*fakeMcpStore)(nil)

func (s *fakeMcpStore) BeginFence(_ context.Context, _, endpoint string) (mcpoauth.LoginFence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginFenceCalls++
	return mcpoauth.LoginFence{Endpoint: endpoint, OwnerLive: true, EndpointGeneration: 1}, nil
}

func (s *fakeMcpStore) BindFence(_ context.Context, f mcpoauth.LoginFence, resource, issuer string) (mcpoauth.LoginFence, error) {
	f.Resource, f.Issuer = resource, issuer
	return f, nil
}

func (s *fakeMcpStore) CommitLogin(_ context.Context, f mcpoauth.LoginFence, g mcpoauth.Grant) (mcpoauth.Grant, error) {
	return g, nil
}

func (s *fakeMcpStore) LookupBinding(_ context.Context, _, _ string) (mcpoauth.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupBindingErr != nil {
		return mcpoauth.Binding{}, s.lookupBindingErr
	}
	return mcpoauth.Binding{}, mcpoauth.ErrMissing
}

func (s *fakeMcpStore) Acquire(_ context.Context, _, _ string, _ int64, _ mcpoauth.RefreshFunc) (mcpoauth.Grant, error) {
	return mcpoauth.Grant{}, mcpoauth.ErrMissing
}

func (s *fakeMcpStore) InvalidateResource(_ context.Context, _, _ string) (mcpoauth.Revocation, error) {
	return mcpoauth.Revocation{}, mcpoauth.ErrMissing
}

func (s *fakeMcpStore) InvalidateEndpoint(_ context.Context, _, _ string) (mcpoauth.Revocation, error) {
	return mcpoauth.Revocation{}, mcpoauth.ErrMissing
}

func (s *fakeMcpStore) InvalidateOwner(_ context.Context, owner string) ([]mcpoauth.Revocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateCalls++
	s.invalidatedOwners = append(s.invalidatedOwners, owner)
	if s.invalidateErr != nil {
		return nil, s.invalidateErr
	}
	if s.invalidatedOfUser == nil {
		s.invalidatedOfUser = make(map[string]bool)
	}
	s.invalidatedOfUser[owner] = true
	return s.revocations, nil
}

// newTestBroker builds a broker over a REAL engine and the fake store, so the
// daemon's calls go through the same code path production uses.
func newTestBroker(t *testing.T, store *fakeMcpStore) *mcpOAuth {
	t.Helper()
	engine, err := mcpoauth.NewEngine(context.Background(), mcpoauth.EngineOptions{Store: store})
	assert.NewAborting(t).NoError(err, "new engine")
	t.Cleanup(func() { _ = engine.Close() })
	return &mcpOAuth{engine: engine, store: store, log: slog.Default()}
}

// TestMcpOAuthUnavailableWithoutAStore pins the degradation: no database or no
// key means the capability answers Unavailable WITH ITS REASON — never a
// plaintext fallback, never a nil panic — and the failure to read an entry is
// not even reached.
func TestMcpOAuthUnavailableWithoutAStore(t *testing.T) {
	c := assert.NewCollecting(t)
	broker := newMCPOAuth(context.Background(), nil, nil, nil, "RAFIKI_SECRETS_KEY is not set", slog.Default())

	ok, why := broker.available()
	c.False(ok, "no pool means unavailable")
	c.StrContains(why, "RAFIKI_SECRETS_KEY", "the reason is the caller's: %s", why)

	_, err := broker.BeginMcpLogin(context.Background(), "u1", "/c.json", "linear", nil)
	var ce *connectapi.ControllerError
	c.Require().True(errors.As(err, &ce), "Begin must be an authored refusal, got %T", err)
	c.Eq(protocol.ErrUnavailable, ce.Code, "Begin code")
	c.Eq(why, ce.Message, "the refusal carries the same reason")

	mustComplete(t, broker)
	c.Error(broker.DeleteMcpCredential(context.Background(), "u1",
		connectapi.McpDeleteSelector{Resource: "https://mcp.example.net/mcp"}), "Delete is refused without a store")

	// A nil broker is the same answer, not a panic: a Controller built by hand
	// in a test, or a daemon that never built one, must not crash a verb.
	var nilBroker *mcpOAuth
	ok, _ = nilBroker.available()
	c.False(ok, "nil broker is unavailable")
	c.Error(nilBroker.DeleteMcpCredential(context.Background(), "u1",
		connectapi.McpDeleteSelector{Resource: "https://x/y"}), "nil broker refuses Delete")
	nilBroker.deleteOwner(context.Background(), "u1")
}

// mustComplete calls Complete on an unavailable broker and returns the empty
// result it answers with, so a test can assert on the refusal separately.
func mustComplete(t *testing.T, broker *mcpOAuth) connectapi.McpLoginComplete {
	t.Helper()
	result, err := broker.CompleteMcpLogin(context.Background(), "u1", "ml-1", "code=abc")
	assert.NewAborting(t).Error(err, "Complete must be refused without a store")
	return result
}

// TestMcpOAuthEngineErrorMapping pins the sentinel classifications the wire
// depends on: one NotFound for every "this login is not yours to finish", a
// "start again" InvalidArgument for every rejected-consent shape (so the SDK
// never mints a second consent URL from a round it must not serve), a
// retryable backpressure for the pending cap, and Unavailable only for a real
// outage.
func TestMcpOAuthEngineErrorMapping(t *testing.T) {
	c := assert.NewCollecting(t)

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"consent rejected", mcpoauth.ErrConsentRejected, protocol.ErrInvalidArgs},
		{"callback replayed", mcpoauth.ErrCallbackReplayed, protocol.ErrInvalidArgs},
		{"callback too large", mcpoauth.ErrCallbackTooLarge, protocol.ErrInvalidArgs},
		{"invalid callback", mcpoauth.ErrInvalidCallback, protocol.ErrInvalidArgs},
		{"authorization error", &mcpoauth.AuthorizationError{Code: "access_denied"}, protocol.ErrInvalidArgs},
		{"pending limit", mcpoauth.ErrPendingLimit, protocol.ErrBackpressure},
		{"stale fence", mcpoauth.ErrStaleFence, protocol.ErrFailedPrecondition},
		{"login required", mcpoauth.ErrLoginRequired, protocol.ErrFailedPrecondition},
		{"store unavailable", mcpoauth.ErrUnavailable, protocol.ErrUnavailable},
		{"engine closed", mcpoauth.ErrClosed, protocol.ErrUnavailable},
		{"destination denied", mcpoauth.ErrDestinationDenied, protocol.ErrInvalidArgs},
	} {
		mapped := mcpLoginEngineError(tc.err)
		var ce *connectapi.ControllerError
		c.Require().True(errors.As(mapped, &ce), "%s: want a ControllerError, got %T", tc.name, mapped)
		c.Eq(tc.want, ce.Code, "%s: code", tc.name)
		c.False(strings.Contains(ce.Message, "code=") || strings.Contains(ce.Message, "secret") ||
			strings.Contains(ce.Message, "token"), "%s: the message must quote nothing secret: %s", tc.name, ce.Message)
	}

	// An error the daemon did not author stays uncoded, so the handler's
	// fallthrough logs it and answers a redacted internal.
	c.Error(mcpLoginEngineError(errors.New("dial tcp 10.0.0.1:5432: refused")), "an uncoded error is forwarded")
	var ce *connectapi.ControllerError
	c.False(errors.As(mcpLoginEngineError(errors.New("x")), &ce), "an uncoded error must not become an authored refusal")
}

// TestMcpOAuthPassiveFactoryIsOwnerBound is the fundi seam: a factory exists
// only for a real owner, and it is the engine's own owner-bound factory (the
// explicit conversion between the two named function types happens at this one
// site).
func TestMcpOAuthPassiveFactoryIsOwnerBound(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakeMcpStore{}
	broker := newTestBroker(t, store)

	c.True(broker.passiveFactory("") == nil, "an empty owner gets no factory: there is no grant to look up")
	c.True(broker.passiveFactory("   ") == nil, "a blank owner gets no factory either")
	c.True(broker.passiveFactory("u1") != nil, "a real owner gets the engine's factory")

	var nilBroker *mcpOAuth
	c.True(nilBroker.passiveFactory("u1") == nil, "no broker, no factory")

	// The factory resolves the exact binding and, with nothing stored, still
	// hands back a usable client: a public server works without a credential.
	store.mu.Lock()
	store.lookupBindingErr = nil
	store.mu.Unlock()
	factory := broker.passiveFactory("u1")
	handler, client, err := factory(context.Background(), "linear", "https://mcp.example.net/mcp", nil)
	c.NoError(err, "a missing credential must not refuse the dial")
	c.True(handler != nil, "handler")
	c.True(client != nil, "client")
}

// TestMcpOAuthPassiveFactoryRefusesAStoreOutage is the other half: a database
// that cannot answer is never reported as "no credential", which would silently
// turn an outage into an anonymous connection.
func TestMcpOAuthPassiveFactoryRefusesAStoreOutage(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakeMcpStore{lookupBindingErr: mcpoauth.ErrUnavailable}
	broker := newTestBroker(t, store)

	_, _, err := broker.passiveFactory("u1")(context.Background(), "linear", "https://mcp.example.net/mcp", nil)
	c.Require().Error(err, "a store outage must refuse the dial")
	c.StrContains(err.Error(), "linear", "the refusal names the server: %v", err)
}

// ─── owner removal ───────────────────────────────────────────────────────────

// TestMcpOAuthDeleteOwnerInvalidatesLocally pins the ruling: removal cancels
// the daemon's pending logins and advances every grant/binding fence, on a
// context the caller cannot cancel.
func TestMcpOAuthDeleteOwnerInvalidatesLocally(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakeMcpStore{revocations: []mcpoauth.Revocation{{Resource: "https://mcp.example.net/mcp"}}}
	broker := newTestBroker(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a client that disconnected mid-removal must not shorten it
	broker.deleteOwner(ctx, "u1")

	store.mu.Lock()
	defer store.mu.Unlock()
	c.Eq(1, store.invalidateCalls, "the delete must invalidate through the store")
	c.EqDeep([]string{"u1"}, store.invalidatedOwners, "the removed user's grants")
	c.True(store.invalidatedOfUser["u1"], "the owner is tombstoned in the store")
}

// TestMcpOAuthDeleteOwnerSurvivesAFailedCleanup pins the fail-closed rule's
// other half: the removal has already happened, so a cleanup failure is logged
// and swallowed. The users row tombstone (written before this call) is what
// keeps every later read and write refused.
func TestMcpOAuthDeleteOwnerSurvivesAFailedCleanup(t *testing.T) {
	c := assert.NewCollecting(t)
	logs := &syncBuffer{}
	store := &fakeMcpStore{invalidateErr: errors.New("store is down")}
	broker := newTestBroker(t, store)
	broker.log = slog.New(slog.NewJSONHandler(logs, nil))

	broker.deleteOwner(context.Background(), "u1")
	c.StrContains(logs.String(), "invalidating a removed user's grants failed", "the failure is logged: %s", logs.String())
	c.StrContains(logs.String(), `"owner":"u1"`, "the log names the owner: %s", logs.String())
	c.False(strings.Contains(logs.String(), "revocations"), "a token payload is never logged: %s", logs.String())
}

// TestMcpOAuthDeleteOwnerIgnoresAnEmptyOwner keeps the removal safe on a
// controller with no owner column (a legacy child) and on a nil broker.
func TestMcpOAuthDeleteOwnerIgnoresAnEmptyOwner(t *testing.T) {
	c := assert.NewCollecting(t)
	store := &fakeMcpStore{}
	broker := newTestBroker(t, store)
	broker.deleteOwner(context.Background(), "")

	store.mu.Lock()
	defer store.mu.Unlock()
	c.Eq(0, store.invalidateCalls, "an empty owner must not reach the store")

	var nilBroker *mcpOAuth
	nilBroker.deleteOwner(context.Background(), "u1")
}

// ─── secretbox: one box, one read ───────────────────────────────────────────

// TestNewCredentialBoxUnsetsTheKeyOnce pins the whole lifetime rule: the key is
// read from the environment exactly once, removed immediately, and a second
// attempt finds nothing — which is why main.go builds BOTH sealed stores from
// ONE box. The provider store's own contract (TestNewCredentialStore*) sits on
// the same function.
func TestNewCredentialBoxUnsetsTheKeyOnce(t *testing.T) {
	c := assert.NewAborting(t)
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	t.Setenv(secretbox.EnvKey, key)

	box, why := newCredentialBox(nil)
	c.True(box == nil, "no pool gives no box")
	c.NotEq("", why, "no pool gives a reason")
	c.Eq(key, os.Getenv(secretbox.EnvKey), "a failure path leaves the environment alone for the next start")

	box, why = newCredentialBox(&pgxpool.Pool{})
	c.Eq("", why, "a valid key gives no reason")
	c.True(box != nil, "the box must be built")
	c.Eq("", os.Getenv(secretbox.EnvKey), "the key must not stay in the process environment")

	second, secondWhy := newCredentialBox(&pgxpool.Pool{})
	c.True(second == nil, "a second resolution finds nothing: the key was unset")
	c.NotEq("", secondWhy, "and says why, rather than silently building a second box")
}

// ─── startup policy ─────────────────────────────────────────────────────────

// TestMcpOAuthPolicyIsCheckedWithoutCredentials pins the startup ordering:
// an invalid [mcp_oauth] section is a STARTUP error whether or not this daemon
// has a key or a database. A destination policy that silently matches nothing
// would look like a working allow-list.
func TestMcpOAuthPolicyIsCheckedWithoutCredentials(t *testing.T) {
	c := assert.NewCollecting(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "rafikid.toml")
	c.NoError(os.WriteFile(path, []byte("[mcp_oauth]\nallow_hosts = [\"*.example.com\"]\n"), 0o600), "write rafikid.toml")

	// No RAFIKI_SECRETS_KEY in the environment at all.
	_, err := rafikidconfig.Load(path)
	c.Require().Error(err, "a wildcard must fail startup")
	c.StrContains(err.Error(), "allow_hosts", "the refusal names the key: %v", err)
}

// ─── the own MCP face ──────────────────────────────────────────────────────

// TestMcpOAuthOwnFaceAdvertisesNoProtectedResourceMetadata pins what rafiki's
// OWN MCP agent-control surface must never do: tell a client that it is an
// OAuth protected resource. That surface authenticates with a rafiki token, not
// with an MCP OAuth grant, and advertising `/.well-known/oauth-protected-
// resource` (or a WWW-Authenticate challenge naming one) would send a client
// off to register and log in against a daemon that cannot receive it.
func TestMcpOAuthOwnFaceAdvertisesNoProtectedResourceMetadata(t *testing.T) {
	c := assert.NewCollecting(t)
	face := newMCPFace(slog.Default(), nil, nil, "test")
	face.SetController(&Controller{})

	path, handler := face.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	for _, probe := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, probe, nil))
		c.Eq(http.StatusNotFound, rec.Code, "%s must not be served: %s", probe, rec.Body.String())
		c.False(strings.Contains(rec.Body.String(), "authorization_servers"),
			"%s must not answer protected-resource metadata: %s", probe, rec.Body.String())
	}

	// An unauthenticated POST to the agent-control path is a plain 403 with no
	// challenge: nothing here invites an OAuth flow.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
	c.Eq(http.StatusForbidden, rec.Code, "unauthenticated POST")
	c.Eq("", rec.Header().Get("WWW-Authenticate"),
		"the face must not advertise an authorization server: %v", rec.Header())

	// And an entitled caller still reaches the SDK handler, so the assertions
	// above are about metadata, not about the path being dead.
	entitled := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	entitled = entitled.WithContext(server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, entitled)
	c.NotEq(http.StatusNotFound, rec.Code, "an entitled caller reaches the MCP handler")
}

// TestMcpOAuthToolsFactorySeamIsExplicit pins the type meeting point: the
// broker's factory is a tools.MCPHTTPFactory, and it is produced by converting
// the engine's mcpoauth.PassiveFactory at this one site. pkg/fundi/tools must
// never import pkg/mcpoauth for this.
func TestMcpOAuthToolsFactorySeamIsExplicit(t *testing.T) {
	c := assert.NewCollecting(t)
	broker := newTestBroker(t, &fakeMcpStore{})

	// A CONVERSION, not a plain assignment: it compiles only if the broker's
	// factory has exactly tools.MCPHTTPFactory's underlying signature.
	var factory = tools.MCPHTTPFactory(broker.passiveFactory("u1"))
	c.True(factory != nil, "the broker's factory must satisfy tools.MCPHTTPFactory")

	// A child with no owner (a legacy row) must not fall through to a
	// daemon-wide lookup: it gets no factory at all.
	c.True(broker.passiveFactory("") == nil, "no owner, no factory")
}

// ─── timing guard ──────────────────────────────────────────────────────────

// TestMcpOwnerCleanupIsBounded keeps the removal's cleanup from becoming an
// unbounded wait on a hung database: the bound is a named constant, well under
// any shutdown budget, and a caller-cancelled context does not shorten it.
func TestMcpOwnerCleanupIsBounded(t *testing.T) {
	c := assert.NewCollecting(t)
	c.True(mcpOwnerCleanupTimeout > 0, "the cleanup must be bounded")
	c.True(mcpOwnerCleanupTimeout <= 30*time.Second, "the bound must stay a bound: %s", mcpOwnerCleanupTimeout)
}
