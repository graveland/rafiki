// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
)

// This file is the child credential's subtree authority for the Connect
// childScoped verbs — the handler-layer half of the policy gate
// (connect_policy.go), which admits a ProvenanceChildToken caller on those
// nine procedures but cannot see a target child id in a procedure name.
//
// The design is the fundi runtime's, transplanted one layer out:
// controllerSpawner (agent_spawner.go) closes over ONE self id and reads the
// stored parent chain for every authority decision, so a tool argument can
// never widen it. Here the self id is closed over per REQUEST instead of per
// tool binding, taken from the credential the mount's identity interceptor
// resolved — never from a wire field — and every target check walks the same
// childstore.IsDescendant chain. A caller that widens its own scope has to
// forge stored state, not an argument.

// childScope implements connectapi.ChildScope for one per-child credential.
type childScope struct {
	c       *Controller
	childID string
}

var _ connectapi.ChildScope = childScope{}

func (s childScope) ChildID() string { return s.childID }

// Authorize is controllerSpawner.authorize with a connect error instead of a
// plain one: same empty-id guard, same stored-chain predicate, same refusal
// text (an operator reading a Connect refusal and a fundi tool refusal should
// read one sentence, not two near-misses). A child is not a descendant of
// itself, and an unknown id refuses with the same answer — a caller cannot
// even confirm that a sibling or parent exists by asking for it.
func (s childScope) Authorize(target string) error {
	if target == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("agent id is required"))
	}
	if !s.c.st.IsDescendant(s.childID, target) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
			"agent %s is not a descendant of yours; you may only view, steer or kill agents you spawned (or that they spawned). Use agent_list to see your subtree",
			target))
	}
	return nil
}

// Subtree is controllerSpawner.List rendered as the summaries ListChildren
// maps onto the wire: the caller's descendants only, statuses-filtered, cost
// rollup and the SnapshotToSummary mapping shared with the operator list.
// The caller itself is never in the answer — it knows its own row.
func (s childScope) Subtree(statuses []string) []protocol.ChildSummary {
	snaps := s.c.st.Descendants(s.childID)
	kept := make([]childstore.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		if len(statuses) > 0 && !containsString(statuses, string(snap.Status)) {
			continue
		}
		kept = append(kept, snap)
	}
	return s.c.summariesFor(kept)
}

// ConversationInScope resolves the conversation ids of the caller's subtree —
// its own ledger plus every descendant's — through the same
// conversationIDForChild mapping GetHistory already serves, and reports
// whether conversationID is one of them. This is ListTasks' subtree boundary.
//
// The resolution is a database lookup per claude-kind row (fundi rows carry
// their conversation UUID on the snapshot), so the check is honest for mixed
// subtrees rather than silently denying every claude descendant's ledger. An
// unresolvable conversation — a child with no captured conversation yet, or
// an id that names no row at all — is out of scope: refuse, never guess.
func (s childScope) ConversationInScope(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	inScope := func(snap childstore.Snapshot) bool {
		return s.c.conversationIDForChild(snap) == conversationID
	}
	if snap, ok := s.c.st.Get(s.childID); ok && inScope(snap) {
		return true
	}
	for _, snap := range s.c.st.Descendants(s.childID) {
		if inScope(snap) {
			return true
		}
	}
	return false
}

// childScopeFor is the source main.go wires onto the Connect Control service.
// It resolves ONE caller's subtree authority from the request context, and it
// is the single place a child credential turns into handler-layer authority.
//
// The branches matter in this order:
//
//   - A nil identity (unix-socket local trust) and a real user credential
//     resolve NO child scope: every childScoped handler takes the operator
//     path, exactly as before this source existed.
//   - EVERY ProvenanceChildToken identity resolves a scope — the resolved
//     shape, the empty-ChildID resolve that names the provenance but no
//     child, and the child whose row has left the childstore. Returning nil
//     for any of the three would upgrade the caller to operator authority —
//     the one fail-open shape this file refuses to build. The scope itself
//     refuses every target (IsDescendant demands a non-empty ancestor with a
//     stored row), answers an empty subtree, and admits no conversation, so
//     a dead, unnamed or vanished child reads as "nothing visible", never as
//     "the fleet".
//   - Every other presented credential — per-boot + session attribution, the
//     bare per-boot secret, anything else — is refused by the policy gate
//     before a handler runs, so this source never sees it; if it somehow
//     does, it resolves no scope and the operator path applies to the
//     anonymous local caller only.
func (c *Controller) childScopeFor(ctx context.Context) connectapi.ChildScope {
	id := server.IdentityFromContext(ctx)
	if id == nil || id.Via != server.ProvenanceChildToken {
		return nil
	}
	return childScope{c: c, childID: id.ChildID}
}
