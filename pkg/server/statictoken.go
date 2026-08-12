// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

type ctxKeyIdentity struct{}

// WithIdentity stores the authenticated identity on the context (set by
// auth middleware, read by ContextAuthenticator for capture attribution).
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKeyIdentity{}, id)
}

// IdentityFromContext returns the identity stored by WithIdentity, or nil.
func IdentityFromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKeyIdentity{}).(*Identity)
	return id
}

type ctxKeyPassthrough struct{}

// WithPassthroughCredential stores the caller's own upstream credential. It is
// set only when a request authenticated via X-Rafiki-Token, which is the
// caller declaring that its Authorization header is its own rather than
// rafiki's.
func WithPassthroughCredential(ctx context.Context, cred string) context.Context {
	return context.WithValue(ctx, ctxKeyPassthrough{}, cred)
}

// PassthroughCredential returns the credential stored by
// WithPassthroughCredential, or "" for an ordinary request whose Authorization
// header is rafiki's own token.
//
// Only StaticTokenAuth.Middleware populates this, which couples the feature to
// standalone mode: an embedded host mounts the faces under its own middleware
// stack (see Handler.Mount), where nothing sets the credential and every
// request silently bills the daemon's key instead. A host that wants
// passthrough must call WithPassthroughCredential itself, having decided by
// its own means that the caller's Authorization is not the host's.
func PassthroughCredential(ctx context.Context) string {
	cred, _ := ctx.Value(ctxKeyPassthrough{}).(string)
	return cred
}

// ContextAuthenticator resolves capture identity from rafiki's own request
// context — the standalone counterpart of sc's middleware adapter.
type ContextAuthenticator struct{}

func (ContextAuthenticator) Identify(r *http.Request) *Identity {
	return IdentityFromContext(r.Context())
}

// StaticTokenAuth authenticates requests against config-defined bearer
// tokens (token value → client name; the name becomes the captured
// owner/origin identity). Tokens are accepted via `X-Rafiki-Token`,
// `Authorization: Bearer` or `x-api-key` — Anthropic-protocol clients like
// sentinel and Claude Code send the middle or last of those. `X-Rafiki-Token`
// additionally means the request's Authorization header belongs to the
// caller and is forwarded upstream (see PassthroughCredential). Unknown or
// missing tokens are rejected with 401.
type StaticTokenAuth struct {
	byToken map[string]string // token value -> client name
}

func NewStaticTokenAuth(tokens map[string]string) *StaticTokenAuth {
	byToken := make(map[string]string, len(tokens))
	for name, token := range tokens {
		if token != "" {
			byToken[token] = name
		}
	}
	return &StaticTokenAuth{byToken: byToken}
}

// Middleware rejects unauthenticated requests and stores the resolved
// identity for the proxy faces' ContextAuthenticator.
func (a *StaticTokenAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, passthrough := credential(r)
		if token == "" {
			http.Error(w, "missing credentials (Authorization: Bearer, x-api-key or X-Rafiki-Token)", http.StatusUnauthorized)
			return
		}
		name, ok := a.lookup(token)
		if !ok {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		ctx := WithIdentity(r.Context(), &Identity{Username: name})
		if passthrough {
			// X-Rafiki-Token means "Authorization is mine, bill it upstream",
			// so both of these fail CLOSED. Proceeding would charge the
			// daemon's key — the exact outcome the caller opted out of — with
			// no error and no log to notice it by.
			cred := r.Header.Get("Authorization")
			if cred == "" {
				http.Error(w, "X-Rafiki-Token means passthrough auth, but no Authorization credential was supplied to forward upstream", http.StatusUnauthorized)
				return
			}
			// Never relay rafiki's own static token to the upstream provider:
			// a client that puts the same token in both headers would ship our
			// secret to a third party and get an opaque 401 back for it.
			if _, isOurs := a.lookup(strings.TrimPrefix(cred, "Bearer ")); isOurs {
				http.Error(w, "Authorization carries rafiki's own token, not an upstream credential; passthrough auth needs your provider credential there", http.StatusUnauthorized)
				return
			}
			ctx = WithPassthroughCredential(ctx, cred)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// lookup does a constant-time comparison against every configured token so
// token values aren't timing-probeable.
func (a *StaticTokenAuth) lookup(token string) (string, bool) {
	var name string
	found := false
	for candidate, n := range a.byToken {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			name, found = n, true
		}
	}
	return name, found
}

// credential returns rafiki's own token and whether it arrived in
// X-Rafiki-Token. That header is checked first and is the passthrough signal:
// a caller which puts rafiki's token there is stating that Authorization
// carries its own upstream credential instead, so the two never collide.
func credential(r *http.Request) (token string, passthrough bool) {
	if h := r.Header.Get("X-Rafiki-Token"); h != "" {
		return h, true
	}
	return bearerOrAPIKey(r), false
}

func bearerOrAPIKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := strings.CutPrefix(h, "Bearer "); ok {
			return token
		}
	}
	return r.Header.Get("x-api-key")
}
