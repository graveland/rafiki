// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// This file is the passive half: what an MCP session uses when it is NOT doing a
// login. It never discovers, never registers, never prompts and never opens a
// browser. It resolves one owner's exact stored binding for one exact endpoint
// and hands back a token source that asks the database for a credential on every
// single acquisition.
//
// Two rules shape it:
//
//   - There is no cache between the database and the request. A grant that was
//     logged out, replaced by a newer login or is mid-refresh is refused on the
//     next call rather than after some token source's idea of expiry. That is
//     why this is a bespoke token source and not an oauth2.ReuseTokenSource: the
//     wrapper's memory would be the authority instead of the row.
//   - Every unusable-credential outcome reads the same to the human: the server
//     needs a login, and here is the command. The database's own sentinels
//     (ErrLoginRequired, ErrStaleFence, ErrMissing) are an implementation detail
//     of the store, so they are folded into one hint and never leak as a generic
//     "database error" the user cannot act on. A database FAILURE stays a
//     failure: it refuses the connection rather than being reported as a missing
//     credential.

// PassiveFactory builds the OAuth handler and HTTP client for one direct-URL MCP
// server. It is the out-of-band seam the fundi runtime takes as an option:
//
//	(ctx, serverName, canonicalEndpoint, headers) -> (handler, client, error)
//
// headers are the entry's configured MCP headers, which exist here for exactly
// one decision: an explicit Authorization header — any case, even empty —
// disables stored OAuth for that entry, and the returned handler then carries no
// token source at all.
type PassiveFactory func(ctx context.Context, serverName, canonicalEndpoint string, headers map[string]string) (auth.OAuthHandler, *http.Client, error)

// LoginRequiredError is what a passive session reports when the server answered
// 401, asked for a scope it does not have, or has a binding the store will not
// hand out any more.
//
// Its message is a complete instruction, because the person who sees it is the
// person who has to run it:
//
//	server X needs a login: run `rafiki mcp login X`
//
// It unwraps to ErrLoginRequired, so a caller can test for the condition without
// matching on text. It never carries the server's own error body, the request,
// or any part of the credential.
type LoginRequiredError struct {
	// Server is the configured server name, as the user wrote it.
	Server string
}

func (e *LoginRequiredError) Error() string {
	return fmt.Sprintf("server %s needs a login: run `rafiki mcp login %s`", e.Server, e.Server)
}

func (e *LoginRequiredError) Unwrap() error { return ErrLoginRequired }

// Passive returns the owner-bound passive factory for one owner. The owner is
// resolved once, here, from the authenticated connection — a session can only
// ever read the grants its own owner logged in for.
func (e *Engine) Passive(owner string) PassiveFactory {
	owner = strings.ToLower(strings.TrimSpace(owner))
	return func(ctx context.Context, serverName, canonicalEndpoint string, headers map[string]string) (auth.OAuthHandler, *http.Client, error) {
		endpoint, err := Canonicalize(canonicalEndpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("mcp: server %q: %w", serverName, err)
		}
		// The MCP client is the guarded streaming client: the destination policy
		// is checked before the request and at every dial, and a redirect may not
		// carry the bearer to another origin.
		client := e.policy.StreamClient()
		handler := &passiveHandler{engine: e, owner: owner, server: serverName, endpoint: endpoint}

		// Precedence: a configured Authorization header wins, case-insensitively
		// and even when its value is empty. Nothing is looked up, so a stored
		// grant cannot displace it — and, for the same reason, this entry can never
		// be told to go and log in: see passiveHandler.Authorize.
		if headerKeyPresent(headers, "Authorization") {
			return handler, client, nil
		}
		if owner == "" {
			// No owner on this connection means no stored grant to offer. The
			// entry still dials; a public server needs no credential.
			return handler, client, nil
		}

		binding, err := e.store.LookupBinding(ctx, owner, endpoint)
		switch {
		case errors.Is(err, ErrMissing), errors.Is(err, ErrLoginRequired):
			// Nothing usable is stored. That is not a reason to refuse the dial:
			// a public server works, and a protected one will say 401 and get
			// the hint from Authorize, because a login is exactly what would fix
			// it.
			handler.hintable = true
			return handler, client, nil
		case err != nil:
			// A store that cannot answer must never be reported as "no
			// credential": that would silently turn a database outage into an
			// anonymous connection.
			return nil, nil, fmt.Errorf("mcp: server %q: reading the stored credential: %w", serverName, err)
		}

		handler.generation = binding.GrantGeneration
		handler.bound = true
		handler.hintable = true
		return handler, client, nil
	}
}

// passiveHandler is the auth.OAuthHandler a passive session keeps.
type passiveHandler struct {
	engine   *Engine
	owner    string
	server   string
	endpoint string
	// bound is false when this entry must not use stored OAuth: an explicit
	// Authorization header, or no owner.
	bound bool
	// hintable is whether a 401 may be answered with "run rafiki mcp login".
	//
	// It is narrower than "not bound" on purpose. An entry whose credentials were
	// configured explicitly — or whose connection has no owner to look a grant up
	// for — must never be told to run a login: the advice would be wrong, and the
	// login it names cannot help.
	hintable bool
	// generation pins the binding this session resolved. A binding that moves —
	// a logout, a newer login — is refused rather than silently adopted.
	generation int64
}

// TokenSource returns the acquisition source, or nil when this entry has no
// stored credential to offer. A nil source is what lets an unauthenticated
// public server work unchanged.
func (h *passiveHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if h == nil || !h.bound {
		return nil, nil
	}
	return &acquireSource{
		engine:     h.engine,
		owner:      h.owner,
		server:     h.server,
		endpoint:   h.endpoint,
		generation: h.generation,
		ctx:        ctx,
	}, nil
}

// Authorize is the non-interactive half: it never discovers, never registers and
// never prompts.
//
// A 401 or a refused scope on an entry that carries stored OAuth is answered with
// the login hint, because a login is exactly what would fix it. An entry with NO
// stored credential because OAuth was deliberately not attached — an explicit
// Authorization header, or a connection with no owner — must not receive that
// advice: telling somebody to run `rafiki mcp login` for an entry whose header
// disabled OAuth is wrong, and the login it names cannot help. That case gets an
// authored, non-interactive refusal instead.
func (h *passiveHandler) Authorize(_ context.Context, req *http.Request, resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		// The handler owns the body it was handed.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
		_ = resp.Body.Close()
	}
	if h.hintable {
		return &LoginRequiredError{Server: h.server}
	}
	return fmt.Errorf("mcp: server %q rejected the credentials it was configured with", h.server)
}

// acquireSource is the passive token source: one database consultation per
// acquisition, never a cached token.
type acquireSource struct {
	engine     *Engine
	owner      string
	server     string
	endpoint   string
	generation int64
	// ctx is the transport's connection context. It carries the session's
	// lifetime, and the engine's own context is combined with it so daemon
	// shutdown cancels a lookup in flight.
	ctx context.Context
}

// Token returns a usable access token for the next request.
//
// The store is consulted every time — including when the previous token had not
// expired — because a logout, a newer login or a refresh in flight is only ever
// visible there. The pinned generation makes a moved binding an explicit
// refusal instead of a silent adoption of somebody else's credential.
func (s *acquireSource) Token() (*oauth2.Token, error) {
	ctx, cancel := s.engine.withLifetime(s.ctx)
	defer cancel()

	// refreshForStore, not refresh: the store wraps a refresh failure into its
	// own error, and this is the one boundary where the engine guarantees the
	// upstream text never travels past it (see sanitizeRefreshError).
	grant, err := s.engine.store.Acquire(ctx, s.owner, s.endpoint, s.generation, s.engine.refreshForStore)
	if err != nil {
		switch {
		case errors.Is(err, ErrMissing), errors.Is(err, ErrLoginRequired), errors.Is(err, ErrStaleFence):
			// Revoked, replaced, expired with no way to refresh, or the owner is
			// gone. All of it is one instruction for the human.
			return nil, &LoginRequiredError{Server: s.server}
		default:
			return nil, fmt.Errorf("mcp: server %q: reading the stored credential: %w", s.server, err)
		}
	}
	return &oauth2.Token{
		AccessToken:  grant.Token.AccessToken,
		TokenType:    grant.Token.TokenType,
		RefreshToken: grant.Token.RefreshToken,
		Expiry:       grant.Token.Expiry,
	}, nil
}

// headerKeyPresent reports whether headers carries key, matched
// case-insensitively — the test the OAuth precedence rule is written in terms of.
// An empty value counts: "the user configured an Authorization header" is a
// decision about the entry, not about its contents.
func headerKeyPresent(headers map[string]string, key string) bool {
	for k := range headers {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return true
		}
	}
	return false
}

// withLifetime combines the session's context with the engine's, so a lookup in
// flight ends when either does.
func (e *Engine) withLifetime(ctx context.Context) (context.Context, context.CancelFunc) {
	if e == nil || e.ctx == nil {
		return mergeContexts(ctx, nil)
	}
	return mergeContexts(ctx, e.ctx)
}
