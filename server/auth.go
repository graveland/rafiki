package server

import "net/http"

// Identity is the authenticated caller of a proxy face, captured as the
// conversation owner / turn author.
type Identity struct {
	Username string
}

// Authenticator resolves the caller identity for a proxied request. Embedded
// mode adapts the host's identity middleware (typically reading what it
// already stored on the request context); standalone mode maps static bearer
// tokens to identities (phase 4). A nil Authenticator or a nil Identity means
// anonymous: the request proceeds and is captured without an owner —
// rejection semantics (401 on unknown tokens) arrive with the standalone
// binary in phase 4; in embedded mode the host middleware has already
// authenticated the request before it reaches the proxy.
type Authenticator interface {
	Identify(r *http.Request) *Identity
}
