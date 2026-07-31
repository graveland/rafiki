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

// ContextAuthenticator resolves capture identity from rafiki's own request
// context — the standalone counterpart of sc's middleware adapter.
type ContextAuthenticator struct{}

func (ContextAuthenticator) Identify(r *http.Request) *Identity {
	return IdentityFromContext(r.Context())
}

// StaticTokenAuth authenticates requests against config-defined bearer
// tokens (token value → client name; the name becomes the captured
// owner/origin identity). Tokens are accepted via `Authorization: Bearer` or
// `x-api-key` — Anthropic-protocol clients like sentinel and Claude Code
// send the latter. Unknown or missing tokens are rejected with 401.
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
		token := bearerOrAPIKey(r)
		if token == "" {
			http.Error(w, "missing credentials (Authorization: Bearer or x-api-key)", http.StatusUnauthorized)
			return
		}
		name, ok := a.lookup(token)
		if !ok {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), &Identity{Username: name})))
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

func bearerOrAPIKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := strings.CutPrefix(h, "Bearer "); ok {
			return token
		}
	}
	return r.Header.Get("x-api-key")
}
