// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/server"
)

// streamRegistry tracks the daemon's open server-streaming Control handlers
// by the credential that opened them, so revocation can CUT a stream instead
// of waiting for it to end on its own. A stream opened before its token
// expired keeps running — deliberately: an expired token's next REQUEST is
// refused, and expiry is a property of time, not an act by anyone. Revocation
// is a security act, so it cuts.
//
// What the registry can and cannot see. Entries exist only for streams the
// daemon itself admitted — its Connect mounts, through
// streamRevocationInterceptor — and hooks fire only on revocations the daemon
// performs: connectUserAdmin.RevokeToken and Controller.UserRm. Revocation
// from the host CLI (`rafikid user token revoke`, pure DB) cannot reach a
// running daemon's registry; on such a token open streams run on, and new
// requests stop within the auth cache's ≤5s window (server.DefaultAuthCacheTTL).
// That asymmetry is documented, not fixed: the CLI has no channel to the
// daemon short of a Control RPC, and intercepting it would mean the daemon
// polls the users table for the sake of a path that already has one
// (Connect's RevokeToken).
//
// Concurrency rules, both load-bearing: cancel funcs are collected UNDER the
// mutex and invoked AFTER it (a cancel may run arbitrary user-code — an
// unwound handler's deferred cleanup — and must never run while another
// add/remove waits on the mutex); and remove is idempotent, because a stream
// can be torn down by revocation and its own deferred remove in either order.
//
// A nil *streamRegistry's revoke methods are inert (return 0); add is only
// reached through the interceptor, which checks for nil first. Controllers
// built by hand in tests (and any wiring path that predates the field) then
// behave as if no streams were ever registered, rather than panicking.
type streamRegistry struct {
	mu      sync.Mutex
	next    uint64
	byToken map[string]map[uint64]streamEntry

	// forget purges the daemon's auth cache when wired (main.go sets it to the
	// face's *server.UserTokenAuth). revokeToken/revokeUser call it BEFORE the
	// registry cut, so a stream the client immediately re-opens cannot
	// re-register from an identity still cached under the revoked credential.
	// Nil (test wiring that never had a face) leaves revocation cutting open
	// streams without touching any cache — the same cut that predates the
	// hook.
	forget authCacheForgetter
}

// authCacheForgetter is the auth-cache purge the revocation paths need: the
// two methods of *server.UserTokenAuth, taken as an interface so the registry
// stays testable without a face.
type authCacheForgetter interface {
	ForgetToken(tokenID string)
	ForgetUser(userID string)
}

// streamEntry is one registered stream.
type streamEntry struct {
	userID string
	cancel context.CancelFunc
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{byToken: make(map[string]map[uint64]streamEntry)}
}

// add registers one stream under the credential that opened it. The returned
// remove is idempotent and safe to call after a revoke has already deleted
// the entry; it does NOT cancel — the interceptor owns its own cancel defer.
func (r *streamRegistry) add(tokenID, userID string, cancel context.CancelFunc) (remove func()) {
	r.mu.Lock()
	if r.byToken == nil {
		r.byToken = make(map[string]map[uint64]streamEntry)
	}
	id := r.next
	r.next++
	byToken := r.byToken[tokenID]
	if byToken == nil {
		byToken = make(map[uint64]streamEntry)
		r.byToken[tokenID] = byToken
	}
	byToken[id] = streamEntry{userID: userID, cancel: cancel}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if byToken, ok := r.byToken[tokenID]; ok {
				if _, ok := byToken[id]; ok {
					delete(byToken, id)
					if len(byToken) == 0 {
						delete(r.byToken, tokenID)
					}
				}
			}
			r.mu.Unlock()
		})
	}
}

// revokeToken cancels every open stream registered under tokenID and returns
// how many were cut. Idempotent: a second call for the same token finds
// nothing registered and returns 0.
func (r *streamRegistry) revokeToken(tokenID string) int {
	if r == nil {
		return 0
	}
	// Purge the auth cache FIRST, before the cut: a client that re-opens its
	// stream the instant it is cut must authenticate against the store again
	// (where the revocation is already committed) rather than re-register
	// from an identity still cached under the revoked credential. Called
	// outside r.mu — the forget hook takes the auth cache's own lock.
	if r.forget != nil {
		r.forget.ForgetToken(tokenID)
	}
	r.mu.Lock()
	byToken := r.byToken[tokenID]
	delete(r.byToken, tokenID)
	cancels := make([]context.CancelFunc, 0, len(byToken))
	for _, e := range byToken {
		cancels = append(cancels, e.cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if n := len(cancels); n > 0 {
		slog.Info("revoked credential cut open streams", "token_id", tokenID, "streams", n)
	}
	return len(cancels)
}

// revokeUser cancels every open stream held by ANY credential of userID and
// returns how many were cut. The user-removal hook: every token the user held
// is dead, so every stream any of them opened dies with it. An empty userID
// is a no-op returning 0 — it can match nothing registered (only a real user
// credential registers, and a real user has a non-empty id), so an unresolved
// owner must cut nothing rather than everything.
func (r *streamRegistry) revokeUser(userID string) int {
	if r == nil || userID == "" {
		return 0
	}
	// Cache purge before the cut, for the same reason as revokeToken's.
	if r.forget != nil {
		r.forget.ForgetUser(userID)
	}
	r.mu.Lock()
	var cancels []context.CancelFunc
	for tokenID, byToken := range r.byToken {
		for id, e := range byToken {
			if e.userID == userID {
				cancels = append(cancels, e.cancel)
				delete(byToken, id)
			}
		}
		if len(byToken) == 0 {
			delete(r.byToken, tokenID)
		}
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if n := len(cancels); n > 0 {
		slog.Info("removed user cut open streams", "user_id", userID, "streams", n)
	}
	return len(cancels)
}

// streamRevocationInterceptor is the interceptor form of the registry: it
// registers each admitted streaming handler and cancels it when its
// credential is revoked.
func streamRevocationInterceptor(r *streamRegistry) connect.Interceptor {
	return streamRevocation{reg: r}
}

type streamRevocation struct{ reg *streamRegistry }

// WrapUnary passes through untouched: a unary call is bounded by its own
// lifetime and re-authenticates on the next request, so there is nothing to
// cut and nothing to register.
func (s streamRevocation) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return next
}

func (s streamRevocation) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler wraps ONLY streaming handlers, and only after the
// policy gate has admitted the call — it sits innermost in
// connectControlRoute's composition, so a refused stream never registers.
//
// Only a real user credential with a non-empty TokenID registers: a child
// credential dies with its control connection (the daemon evicts a child's
// transient executor when the stream ends), and a nil identity is the local
// socket's anonymous trust, which no token revocation can name. An expired
// token is NOT a trigger — nothing here ever fires on its own; only
// revokeToken and revokeUser cut.
//
// The handler's context is cancelled by revocation, and connect maps a
// handler that returns nil on a cancelled context to a CLEAN end of stream —
// the client would see a bare EOF and never learn why its stream died. So a
// nil return with the context already cancelled is reported as Canceled: the
// caller is still connected (a departed client reads no trailer), and the
// deliberate cut should say what it is.
func (s streamRevocation) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if s.reg == nil {
			return next(ctx, conn)
		}
		id := server.IdentityFromContext(ctx)
		if id == nil || !id.IsUserCredential() || id.TokenID == "" {
			return next(ctx, conn)
		}
		ctx, cancel := context.WithCancel(ctx)
		remove := s.reg.add(id.TokenID, id.UserID, cancel)
		defer remove()
		defer cancel()
		err := next(ctx, conn)
		if err == nil && ctx.Err() != nil {
			return connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		return err
	})
}
