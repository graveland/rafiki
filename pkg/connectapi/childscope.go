// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ChildScope is the subtree authority a per-child credential carries on the
// childScoped verbs (Spawn, ListChildren, GetChild, GetHistory, StreamEvents,
// Send, Kill, Close, ListTasks).
//
// The policy gate (cmd/rafikid connect_policy.go) admits a ProvenanceChildToken
// caller on those nine procedures, but a procedure name names no target child,
// so the gate cannot check the subtree: the check has to run where the request
// body is visible, which is here. Every childScoped handler consults this
// interface before acting, and falls through to the operator path when the
// caller resolves to no child scope — a real user credential, or the unix
// socket's anonymous local trust.
//
// The daemon wires ONE implementation (cmd/rafikid connect_childscope.go),
// which reads the stored parent chain via childstore.IsDescendant — the same
// authority the fundi runtime's controllerSpawner applies — and never trusts a
// wire argument for the caller's position in the tree.
type ChildScope interface {
	// ChildID is the caller's own child id, taken from the credential —
	// never from a request field.
	ChildID() string

	// Authorize refuses every target the caller may not act on: the empty id,
	// unknown ids, and every id that is not strictly beneath the caller's own
	// — the caller itself included, because a child is not a descendant of
	// itself. The error is already a connect PermissionDenied carrying the
	// same text the fundi spawner's refusals use, so handlers forward it
	// unchanged.
	Authorize(target string) error

	// Subtree lists the caller's descendants — never the caller itself, the
	// same shape controllerSpawner.List answers — as summaries, filtered by
	// statuses (nil or empty means no filter). It is ListChildren's answer
	// for a child caller, so the cost rollup and the SnapshotToSummary
	// mapping stay one implementation shared with the operator list.
	Subtree(statuses []string) []protocol.ChildSummary

	// ConversationInScope reports whether conversationID is a conversation of
	// the caller's subtree INCLUDING the caller's own: ListTasks' ledger read
	// is keyed by conversation id, so this is its subtree boundary. A
	// conversation whose row cannot be resolved from the childstore (a claude
	// child with no captured conversation yet, an unknown id) is out of
	// scope — refuse, never guess.
	ConversationInScope(conversationID string) bool
}

// ChildScopeSource resolves the caller's ChildScope per request. The identity
// lives in the request context (resolved by the mount's identity interceptor,
// never by this package), so the daemon wires a closure that reads it there —
// the same rule as cmd/rafikid's spawnOwner and scopeFor: pkg/connectapi has
// no credential path of its own.
//
// A source that returns nil, for any caller, means "no child authority":
// every handler takes the operator path. A per-child credential whose
// childstore row has since disappeared must NOT resolve to nil — that would
// silently upgrade a child to operator authority — so the implementation
// returns a scope that refuses everything instead.
type ChildScopeSource func(ctx context.Context) ChildScope

// SetChildScopeSource attaches the resolver. Post-construction setter for the
// same reason as SetChildLister: the Controller is built after this Server.
func (s *Server) SetChildScopeSource(src ChildScopeSource) { s.scopes.Store(&src) }

// childScope resolves the caller's subtree authority, nil when the source is
// unwired (every handler then fails closed on its own wiring checks or serves
// the operator path, exactly as before this seam existed) or when the source
// resolves no child for this caller.
func (s *Server) childScope(ctx context.Context) ChildScope {
	p := s.scopes.Load()
	if p == nil || *p == nil {
		return nil
	}
	return (*p)(ctx)
}

// refuseChildScope is the uniform refusal the childScoped handlers return for
// a target outside the caller's subtree. The code matches the gate's
// PermissionDenied so a denied child caller sees one code whichever layer
// refused it.
func refuseChildScope(err error) error {
	return connect.NewError(connect.CodePermissionDenied, err)
}
