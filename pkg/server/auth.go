// SPDX-License-Identifier: Apache-2.0

package server

import "net/http"

// CredentialProvenance records what kind of credential stood behind an
// Identity — a property of the credential, never of the request's headers.
//
// The zero value is the fail-closed one: it means no user token was involved,
// so every agent-control check must refuse it. Only resolve() stamps
// identities, and every construction site that means "a real user" — tests
// included — must set ProvenanceUser explicitly rather than rely on a
// non-empty UserID: the child-attribution path hands a spawned agent its
// owner's UserID, so a UserID-shaped check cannot tell the two apart.
type CredentialProvenance uint8

const (
	// ProvenanceUnknown is the zero value: no user credential was presented,
	// or the credential presented was not one. Every agent-control check
	// fails closed on it.
	ProvenanceUnknown CredentialProvenance = iota
	// ProvenanceUser: resolved from a real user token against the users
	// store. The only provenance an agent-control surface accepts.
	// X-Rafiki-Session alongside such a token does not change it.
	ProvenanceUser
	// ProvenanceChildAttributed: the per-boot child secret plus
	// X-Rafiki-Session resolved to the child's owner. The UserID is real and
	// the attribution paths (/v1/messages billing, capture, quota) consume
	// it, but the credential itself is the daemon's shared boot secret, so
	// agent-control surfaces refuse it.
	ProvenanceChildAttributed
	// ProvenanceChildToken: a per-child secret minted by the daemon at spawn.
	// Unlike ProvenanceChildAttributed it names exactly ONE child without
	// consulting any header, so agent-control surfaces accept it and bind to
	// that child's own position in the tree. It dies with the child.
	ProvenanceChildToken
)

// Identity is the authenticated caller of a proxy face.
//
// UserID is the persisted attribution (conversation.owner_user_id,
// conversation_turn.author_user_id) and is empty for non-user callers: a
// spawned child on the per-boot token, or an anonymous request. Username is
// for logs and CLI output only — it is NEVER written to a row, because the
// username is resolved at read time through the users FK. Via records the
// credential the identity was resolved from; see CredentialProvenance.
type Identity struct {
	UserID   string
	Username string
	// ChildID names the ONE child this identity is bound to. Set only for
	// ProvenanceChildToken; empty for every other provenance, including a
	// real user credential, which is what marks the interactive caller as
	// outside the forest.
	ChildID string
	Via     CredentialProvenance
}

// IsUserCredential reports whether the identity was resolved from a real user
// token — the only provenance an agent-control surface accepts. A
// child-attributed identity carries its owner's UserID, so an IsUser-shaped
// (non-empty UserID) check cannot substitute for this.
func (i Identity) IsUserCredential() bool { return i.Via == ProvenanceUser }

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
