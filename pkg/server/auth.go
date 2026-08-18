// SPDX-License-Identifier: Apache-2.0

package server

import "net/http"

// Identity is the authenticated caller of a proxy face.
//
// UserID is the persisted attribution (conversation.owner_user_id,
// conversation_turn.author_user_id) and is empty for non-user callers: a
// spawned child on the per-boot token, or an anonymous request. Username is
// for logs and CLI output only — it is NEVER written to a row, because the
// username is resolved at read time through the users FK.
type Identity struct {
	UserID   string
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
