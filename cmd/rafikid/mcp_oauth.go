// SPDX-License-Identifier: Apache-2.0

package main

// MCP OAuth, daemon side: the three Control verbs, the exact-entry resolver
// they share, and the two hand-offs the rest of the daemon needs — the passive
// factory a fundi child dials direct-URL servers with, and the owner cleanup a
// user removal owes.
//
// The rules that shape this file:
//
//   - EXPLICIT and USER-ONLY. A login starts only from BeginMcpLogin, never
//     from a spawn, and the grant belongs to the authenticated caller. Nothing
//     interactive can run inside a spawn: a child that meets a 401 gets the
//     `rafiki mcp login <server>` hint, not a browser.
//   - The daemon reads the EXACT file and the EXACT entry the request names.
//     There is no default-path resolution here — the client resolves the path,
//     the daemon refuses a relative one — and no fallback to another entry.
//   - Everything that cannot authenticate by an interactive OAuth flow on THIS
//     machine is refused BEFORE any network work: a stdio server, a
//     via_executor server, a missing url, and an entry carrying an explicit
//     Authorization header (store OAuth must not quietly override a configured
//     credential).
//   - No configured header ever leaves this file. The entry's headers are read
//     for exactly one decision (the Authorization precedence rule); the engine
//     is handed an endpoint and a scope list and nothing else, so a project's
//     secret cannot drift into discovery, registration or token requests.
//   - The destination policy is the OPERATOR's, from rafikid.toml. A project
//     entry, a login flag or remote metadata cannot widen it.
//   - Tokens never cross the wire and never enter a CLI or a child process. The
//     engine holds them sealed, and no verb here returns one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/mcpauthdb"
	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/secretbox"
)

// mcpOwnerCleanupTimeout bounds the durable half of a removed user's cleanup.
// It runs on a context the caller cannot cancel (a removal is a privacy act,
// and an admin's client disconnecting must not leave half the grants behind)
// but it is bounded, because a hung database must not hold the removal open.
const mcpOwnerCleanupTimeout = 30 * time.Second

// mcpOAuth is the daemon's MCP OAuth broker: the login engine, the store its
// owner cleanup goes through, and the reason the capability is unavailable when
// it is.
//
// A nil *mcpOAuth is the documented "not configured" state: every method is
// nil-safe, the Connect verbs answer Unavailable with a reason, and a fundi
// child simply gets no credential factory — which is exactly the behaviour a
// daemon with no database or no RAFIKI_SECRETS_KEY has always had. Static and
// unauthenticated MCP servers keep working either way.
type mcpOAuth struct {
	engine *mcpoauth.Engine
	store  mcpoauth.Store
	why    string // non-empty = unavailable, the reason shown to users
	log    *slog.Logger
}

// newMCPOAuth builds the broker over one pool and the secretbox the provider
// credentials already use. box is nil (and why non-empty) when the key or the
// database is missing: the capability is then unavailable with that reason and
// NOTHING falls back to plaintext — mcpauthdb panics on a nil box precisely so
// that a fallback cannot be written by accident.
//
// policy is the operator's [mcp_oauth] section. It is passed even when it is the
// zero policy, so "the operator configured nothing" means public HTTPS only
// rather than a nil that some later caller could widen.
func newMCPOAuth(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, policy *mcpoauth.Policy, why string, log *slog.Logger) *mcpOAuth {
	if log == nil {
		log = slog.Default()
	}
	if pool == nil || box == nil {
		if why == "" {
			why = "MCP OAuth needs a database and RAFIKI_SECRETS_KEY; static or unauthenticated MCP servers still work"
		}
		return &mcpOAuth{why: why, log: log}
	}
	store := mcpauthdb.New(pool, box)
	engine, err := mcpoauth.NewEngine(ctx, mcpoauth.EngineOptions{
		Store:  store,
		Policy: policy,
		Logger: log,
	})
	if err != nil {
		return &mcpOAuth{why: fmt.Sprintf("MCP OAuth is unavailable: %v", err), log: log}
	}
	return &mcpOAuth{engine: engine, store: store, why: "", log: log}
}

// available reports whether MCP OAuth can be used at all, and why not when it
// cannot. The reason is safe to show a user: it names the missing setting.
func (m *mcpOAuth) available() (bool, string) {
	if m == nil {
		return false, "MCP OAuth is not configured on this daemon"
	}
	if m.why != "" {
		return false, m.why
	}
	if m.engine == nil || m.store == nil {
		return false, "MCP OAuth is unavailable: the daemon has no login engine"
	}
	return true, ""
}

// close terminates every pending login and waits for their tasks. It runs on
// daemon shutdown, before the pool closes: a flow still exchanging a code would
// otherwise write through a closed database.
func (m *mcpOAuth) close() {
	if m == nil || m.engine == nil {
		return
	}
	_ = m.engine.Close()
}

// passiveFactory is the out-of-band MCPHTTP factory for ONE owner's children,
// or nil when this daemon can serve no stored credential.
//
// It is the single place the two named function types meet: mcpoauth's
// PassiveFactory and tools' MCPHTTPFactory have the same signature and
// deliberately different names, and the conversion is explicit here so
// pkg/fundi/tools never has to import pkg/mcpoauth.
//
// An EMPTY owner gets no factory at all: without an authenticated owner there
// is no grant to look up, so a session dials unauthenticated (a public server
// works) and a protected one gets the login hint. That is the same behaviour as
// "nothing stored", which is what it is.
func (m *mcpOAuth) passiveFactory(owner string) tools.MCPHTTPFactory {
	if m == nil || m.engine == nil || strings.TrimSpace(owner) == "" {
		return nil
	}
	return tools.MCPHTTPFactory(m.engine.Passive(owner))
}

// ─── the exact entry ─────────────────────────────────────────────────────────

// mcpServerEndpoint resolves the ONE entry a login or a config-form logout
// names, and refuses everything that cannot do an interactive OAuth login on
// this daemon.
//
// It returns the entry's configured URL verbatim: canonicalisation belongs to
// the engine (pkg/mcpoauth owns that rule), and NO destination check happens
// here — a URL being present in a project file says nothing about whether the
// daemon may reach it. The engine's discovery applies the operator's
// [mcp_oauth] policy to every leg.
//
// Headers are read for the precedence rule only and never returned: the caller
// gets an endpoint and nothing else, so no configured secret can reach a
// discovery, registration or token request.
func mcpServerEndpoint(configPath, serverName string) (string, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		return "", mcpInvalidArgument("config_path is required")
	}
	if !filepath.IsAbs(path) {
		// The daemon must not resolve a project path in its own working
		// directory: that is a different directory from the caller's, so a
		// relative path would silently name a file the user did not mean.
		return "", mcpInvalidArgument(fmt.Sprintf(
			"config_path %q must be an absolute path on the daemon host", configPath))
	}
	if strings.TrimSpace(serverName) == "" {
		return "", mcpInvalidArgument("server is required: name the .mcp.json entry to log in to")
	}
	cfg, err := tools.LoadMCPConfig(path)
	if err != nil {
		// LoadMCPConfig's text names the path and the parse problem; it never
		// carries a header value, a command line or any other entry content.
		return "", mcpInvalidArgument(fmt.Sprintf("cannot read the MCP config %q: %v", path, err))
	}
	entry, ok := cfg.MCPServers[serverName]
	if !ok {
		return "", mcpInvalidArgument(fmt.Sprintf("the MCP config %q has no mcpServers entry named %q", path, serverName))
	}
	if entry.ViaExecutor != nil {
		// The argv, the environment and any credential belong to the machine
		// that runs the server. A daemon-side login would be a credential the
		// executor never sees, so this entry cannot be logged in here.
		return "", mcpInvalidArgument(fmt.Sprintf(
			"server %q runs via_executor: its transport and credentials live on the executor machine, so it cannot be logged in from the daemon", serverName))
	}
	if entry.Command != "" {
		return "", mcpInvalidArgument(fmt.Sprintf(
			"server %q is a stdio server (command): a local process needs no OAuth login; only a direct url entry can log in", serverName))
	}
	if entry.URL == "" {
		return "", mcpInvalidArgument(fmt.Sprintf(
			"server %q has no url: only a direct url entry can log in", serverName))
	}
	if key, ok := mcpAuthorizationHeader(entry.Headers); ok {
		// Precedence, case-insensitive and even when the value is empty: an
		// explicit Authorization header disables stored OAuth for that entry,
		// so a login would store a grant nothing would ever spend. Removing the
		// header is the user's decision to make, not ours.
		return "", mcpInvalidArgument(fmt.Sprintf(
			"server %q sets an %s header in the MCP config: an explicit header disables stored OAuth for that entry, so remove the header (or the entry) and log in again",
			serverName, key))
	}
	return entry.URL, nil
}

// mcpAuthorizationHeader reports whether headers carries an Authorization key
// under ANY capitalisation, and which spelling it used (so the refusal can name
// what the user must delete). The VALUE is deliberately not returned: it is a
// credential, and an error message is not a place to repeat one.
func mcpAuthorizationHeader(headers map[string]string) (string, bool) {
	for key := range headers {
		if strings.EqualFold(key, "Authorization") {
			return key, true
		}
	}
	return "", false
}

// mcpInvalidArgument is an authored refusal (InvalidArgument on the wire). Its
// message is ours: it names the entry, the path and the fix, never a header
// value or a credential.
func mcpInvalidArgument(msg string) error {
	return &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: msg}
}

// ─── Control verbs ───────────────────────────────────────────────────────────

// errUnknownMcpLogin is the single answer for a login id that is absent,
// expired, or was begun by a different owner. Naming which one would let a
// caller learn that another user's login exists.
var errUnknownMcpLogin = &connectapi.ControllerError{
	Code:    protocol.ErrNotFound,
	Message: "unknown or expired MCP login; start the login again",
}

// mcpLoginEngineError classifies an engine/store error for the wire.
//
// Two things it must not do: forward a dependency's text (the handler's
// fallthrough logs an uncoded error and answers "internal error"), and quote the
// callback. Everything the engine rejects about a callback is therefore one
// "start again" answer with no detail from the query.
func mcpLoginEngineError(err error) error {
	var authErr *mcpoauth.AuthorizationError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &authErr):
		// The authorization server rejected the request (access_denied, or a
		// protocol error). Its code is a sanitized protocol string; the login is
		// over either way, and the SDK would otherwise mint a SECOND consent URL
		// for the same rejected input.
		return mcpInvalidArgument(fmt.Sprintf(
			"the MCP authorization server refused the login (%s); start the login again with `rafiki mcp login <server>`", authErr.Code))
	case errors.Is(err, mcpoauth.ErrConsentRejected):
		return mcpInvalidArgument("the MCP server refused the consent round; start the login again with `rafiki mcp login <server>`")
	case errors.Is(err, mcpoauth.ErrCallbackReplayed):
		return mcpInvalidArgument("that callback was already used; start the login again with `rafiki mcp login <server>`")
	case errors.Is(err, mcpoauth.ErrCallbackTooLarge):
		return mcpInvalidArgument("the callback is too large; start the login again with `rafiki mcp login <server>`")
	case errors.Is(err, mcpoauth.ErrInvalidCallback):
		return mcpInvalidArgument("the callback does not match the login (a bad code, state or issuer); start the login again with `rafiki mcp login <server>`")
	case errors.Is(err, mcpoauth.ErrPendingLimit):
		// Retryable: nothing is wrong with the request, there is just no room.
		return &connectapi.ControllerError{
			Code:    protocol.ErrBackpressure,
			Message: "too many MCP logins are in flight; finish or cancel one of them and retry",
		}
	case errors.Is(err, mcpoauth.ErrStaleFence):
		return &connectapi.ControllerError{
			Code:    protocol.ErrFailedPrecondition,
			Message: "the login was superseded by a logout or a newer login for the same server; start again",
		}
	case errors.Is(err, mcpoauth.ErrLoginRequired):
		return &connectapi.ControllerError{
			Code:    protocol.ErrFailedPrecondition,
			Message: "the MCP credential can no longer be used; run `rafiki mcp login <server>` again",
		}
	case errors.Is(err, mcpoauth.ErrUnavailable):
		return &connectapi.ControllerError{
			Code:    protocol.ErrUnavailable,
			Message: "the MCP credential store is unavailable",
		}
	case errors.Is(err, mcpoauth.ErrClosed):
		return &connectapi.ControllerError{
			Code:    protocol.ErrUnavailable,
			Message: "the daemon is shutting down",
		}
	case errors.Is(err, mcpoauth.ErrDestinationDenied):
		// The operator's policy refused a discovered destination. The engine's
		// own text names the host and the section, and is ours to forward.
		return mcpInvalidArgument(fmt.Sprintf(
			"MCP OAuth cannot reach that server: %v (the operator's [mcp_oauth] policy in rafikid.toml decides which destinations are allowed)", err))
	}
	return err
}

// BeginMcpLogin starts one explicit login against the exact entry named.
func (m *mcpOAuth) BeginMcpLogin(
	ctx context.Context, ownerUserID, configPath, serverName string, scopes []string,
) (connectapi.ProviderLoginBegin, error) {
	if ok, why := m.available(); !ok {
		return connectapi.ProviderLoginBegin{}, &connectapi.ControllerError{
			Code: protocol.ErrUnavailable, Message: why,
		}
	}
	endpoint, err := mcpServerEndpoint(configPath, serverName)
	if err != nil {
		return connectapi.ProviderLoginBegin{}, err
	}
	descriptor, err := m.engine.Begin(ctx, mcpoauth.BeginRequest{
		OwnerUserID: ownerUserID,
		Server:      serverName,
		Endpoint:    endpoint,
		Scopes:      scopes,
	})
	if err != nil {
		return connectapi.ProviderLoginBegin{}, mcpLoginEngineError(err)
	}
	return mcpBeginDescriptor(descriptor), nil
}

// CompleteMcpLogin finishes one consent round.
//
// A continuation is returned as a continuation and NOTHING else: no resource,
// no tool list. The engine sets Server on the descriptor for the client's
// benefit, but a round that has not committed a grant must never look like a
// finished login on the wire.
func (m *mcpOAuth) CompleteMcpLogin(
	ctx context.Context, ownerUserID, loginID, callbackQuery string,
) (connectapi.McpLoginComplete, error) {
	if ok, why := m.available(); !ok {
		return connectapi.McpLoginComplete{}, &connectapi.ControllerError{
			Code: protocol.ErrUnavailable, Message: why,
		}
	}
	result, err := m.engine.Complete(ctx, ownerUserID, loginID, callbackQuery)
	if err != nil {
		if errors.Is(err, mcpoauth.ErrMissing) {
			// Unknown, expired and foreign-owner are one answer, and the store
			// consumed nothing to produce it.
			return connectapi.McpLoginComplete{}, errUnknownMcpLogin
		}
		return connectapi.McpLoginComplete{}, mcpLoginEngineError(err)
	}
	if result.Continuation != nil {
		descriptor := mcpBeginDescriptor(*result.Continuation)
		return connectapi.McpLoginComplete{Continuation: &descriptor}, nil
	}
	return connectapi.McpLoginComplete{
		Server:    result.Server,
		Resource:  result.Resource,
		AccountID: result.AccountID,
		Tools:     result.Tools,
	}, nil
}

// DeleteMcpCredential removes one grant.
//
// The RESOURCE form never touches the config: a user whose project file is gone
// must still be able to remove the grant it left behind. The ENTRY form resolves
// the exact direct-URL endpoint first (so a logout cannot be aimed at a server
// this daemon could not have logged in to) and invalidates the endpoint fence
// even when no grant exists yet — that is what kills a first login still in
// flight.
func (m *mcpOAuth) DeleteMcpCredential(
	ctx context.Context, ownerUserID string, sel connectapi.McpDeleteSelector,
) error {
	if ok, why := m.available(); !ok {
		return &connectapi.ControllerError{Code: protocol.ErrUnavailable, Message: why}
	}
	var selector mcpoauth.DeleteSelector
	if sel.Resource != "" {
		selector = mcpoauth.DeleteSelector{Resource: sel.Resource}
	} else {
		endpoint, err := mcpServerEndpoint(sel.ConfigPath, sel.Server)
		if err != nil {
			return err
		}
		selector = mcpoauth.DeleteSelector{Endpoint: endpoint}
	}
	if err := m.engine.Delete(ctx, ownerUserID, selector); err != nil {
		if errors.Is(err, mcpoauth.ErrMissing) {
			if sel.Resource != "" {
				return &connectapi.ControllerError{
					Code:    protocol.ErrNotFound,
					Message: fmt.Sprintf("no MCP credential is stored for resource %q", sel.Resource),
				}
			}
			return &connectapi.ControllerError{
				Code:    protocol.ErrNotFound,
				Message: fmt.Sprintf("no MCP credential is stored for server %q", sel.Server),
			}
		}
		return mcpLoginEngineError(err)
	}
	return nil
}

// mcpBeginDescriptor converts the engine's descriptor for the wire seam.
func mcpBeginDescriptor(d mcpoauth.LoginDescriptor) connectapi.ProviderLoginBegin {
	return connectapi.ProviderLoginBegin{
		LoginID:       d.LoginID,
		AuthorizeURL:  d.AuthorizeURL,
		CallbackPath:  d.CallbackPath,
		RedirectPort:  d.RedirectPort,
		RedirectHosts: d.RedirectHosts,
	}
}

// ─── owner removal ───────────────────────────────────────────────────────────

// deleteOwner drops everything one removed user had, and is best effort by
// design: the removal has already happened and is not the daemon's to undo.
//
// The order is the ruling. LOCAL invalidation first — cancel the daemon's own
// pending logins for the owner so no interactive round can complete after the
// tombstone, then advance every grant, binding and endpoint fence in one
// transaction — and then, bounded, whatever upstream revocation the store handed
// back. The users row tombstone remains the fail-closed authority: the store
// refuses every read and write for a tombstoned owner, so a cleanup that never
// finishes still cannot resurrect a grant.
//
// A failure here is logged and does not fail the removal.
func (m *mcpOAuth) deleteOwner(ctx context.Context, ownerUserID string) {
	if m == nil || m.engine == nil || m.store == nil || ownerUserID == "" {
		return
	}
	cancelled := m.engine.CancelOwner(ownerUserID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpOwnerCleanupTimeout)
	defer cancel()
	detached, err := m.store.InvalidateOwner(ctx, ownerUserID)
	if err != nil {
		// The tombstone is what keeps this safe; this line is for the operator.
		m.log.Warn("MCP credentials: invalidating a removed user's grants failed",
			"owner", ownerUserID, "error", err)
		return
	}
	if n := len(detached) + cancelled; n > 0 {
		// Counts only: a revocation payload holds tokens, and a log line is not
		// a place for one.
		m.log.Info("MCP credentials: removed a user's MCP grants",
			"owner", ownerUserID, "grants", len(detached), "logins_cancelled", cancelled)
	}
}
