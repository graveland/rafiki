// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
)

// OwnerGuard is the owner isolation a NON-admin user credential carries on
// the live-agent verbs. It exists alongside ChildScope, not inside it:
// ChildScope is "the caller's position in the tree" (a per-child credential's
// subtree authority); a user has no position, so handing one a ChildScope
// would make Spawn force an empty parent and change the script verbs'
// behaviour. The guard answers one question — is this child the caller's? —
// and the daemon's implementation (cmd/rafikid connect_ownerguard.go)
// resolves it through Controller.OwnerUserIDForChild, the same parent-chain
// attribution walk the proxy face uses.
//
// The guard the source resolves is nil for every caller with full authority
// (the unix socket's local trust, an admin user credential) and for every
// caller the existing ChildScope governs (a per-child credential): only a
// non-admin user credential resolves a real guard. Handlers consult it AFTER
// their childScope block and BEFORE any side effect, so a foreign id never
// reaches the lifecycle, the inbox or a store read.
//
// Authorize's refusal names the target with ONE text for foreign, unknown
// and unowned ids alike — a caller cannot probe which children exist by
// asking. The exact text lives in the daemon's implementation; handlers
// forward it unchanged, as they do ChildScope.Authorize's. An EMPTY id is the
// one shape a handler rejects before the guard runs: every verb already
// answers a missing required child_id InvalidArgument, that answer names no
// child so it probes nothing, and making it credential-dependent would
// disclose which kind of caller is asking. Authorize refuses "" too, for a
// caller that reaches it.
type OwnerGuard interface {
	// Authorize refuses every child id the caller does not own: foreign,
	// unknown and unowned (no owner anywhere in the lineage) ids all get the
	// one identical connect CodePermissionDenied. Ownership is a LIVE-row
	// property — a closed child has no row, so it refuses exactly as an
	// unknown id does.
	Authorize(childID string) error

	// Owns is Authorize's predicate form, for list and stream filtering:
	// true when the caller owns childID. The same ownership resolution backs
	// both, so a row Authorize admits is always one Owns keeps.
	Owns(childID string) bool

	// OwnsConversation resolves the conversation to the child that owns it —
	// the same conversationIDForChild mapping ChildScope.ConversationInScope
	// uses — and answers Owns on that child. An unresolvable conversation (a
	// closed child, a claude child with no captured conversation yet, an
	// unknown id) is NOT owned: refuse, never guess. This is ListTasks'
	// boundary.
	OwnsConversation(conversationID string) bool
}

// OwnerGuardSource resolves the caller's OwnerGuard per request. The identity
// lives in the request context (resolved by the mount's identity interceptor,
// never by this package), so the daemon wires a closure that reads it there —
// the same rule as ChildScopeSource: pkg/connectapi has no credential path of
// its own. A source that returns nil means "no owner restriction": the
// handler takes the unrestricted path.
type OwnerGuardSource func(ctx context.Context) OwnerGuard

// SetOwnerGuardSource attaches the resolver. Post-construction setter for the
// same reason as SetChildScopeSource: the Controller is built after this
// Server. A nil source is refused rather than stored, the same rule as
// SetSkillManager: storing &src for a nil func would make Load() return a
// non-nil pointer whose call panics.
func (s *Server) SetOwnerGuardSource(src OwnerGuardSource) {
	if src == nil {
		return
	}
	s.guards.Store(&src)
}

// ownerGuard resolves the caller's owner guard, nil when the source is
// unwired (every handler then serves the unrestricted path, exactly as before
// this seam existed — the daemon's wiring, pinned by the integration test, is
// what makes the guard live) or when the caller has full authority.
func (s *Server) ownerGuard(ctx context.Context) OwnerGuard {
	p := s.guards.Load()
	if p == nil || *p == nil {
		return nil
	}
	return (*p)(ctx)
}
