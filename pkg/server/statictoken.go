// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
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
// Only UserTokenAuth.Middleware populates this, which couples the feature to
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
