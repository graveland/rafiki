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
	return s.c.summariesFor(s.filterStatuses(s.c.st.Descendants(s.childID), statuses))
}

// AuthorizeRead is Authorize's read-side sibling: the same empty-id guard and
// the same refusal shape, but the predicate is canReadChild — the caller's
// subtree OR its owner's children — because reading another agent widens to the
// owner while acting stays on the tree. A vanished caller refuses everything:
// canReadChild resolves no owner for an id with no row, so only the (also
// empty) subtree arm could ever admit, and IsDescendant demands a stored
// ancestor.
func (s childScope) AuthorizeRead(target string) error {
	if target == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("agent id is required"))
	}
	if !s.c.canReadChild(s.childID, target) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(notReadableText, target))
	}
	return nil
}

// OwnedChildren is ListChildren's owner-scope answer: the caller's subtree
// first, marked in-subtree, then the rest of what its owner owns, marked not.
// An ownerless (or vanished) caller has no owner scope at all, so it gets its
// subtree alone — still marked in-subtree, because those rows ARE its subtree
// and the flag says "spawned by me", not "you may read it".
func (s childScope) OwnedChildren(statuses []string) []protocol.ChildSummary {
	subtree, others, ok := s.c.ownedSnapshots(s.childID)
	if !ok {
		return markInSubtree(s.Subtree(statuses), true)
	}
	out := markInSubtree(s.c.summariesFor(s.filterStatuses(subtree, statuses)), true)
	return append(out, markInSubtree(s.c.summariesFor(s.filterStatuses(others, statuses)), false)...)
}

// filterStatuses applies the child-list status filter to a snapshot slice, the
// same nil-or-empty-means-no-filter rule Controller.ListChildren applies. Both
// lists an owner-scope answer carries go through it, so a status filter never
// applies to one half of the answer and not the other.
func (s childScope) filterStatuses(snaps []childstore.Snapshot, statuses []string) []childstore.Snapshot {
	kept := make([]childstore.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		if len(statuses) > 0 && !containsString(statuses, string(snap.Status)) {
			continue
		}
		kept = append(kept, snap)
	}
	return kept
}

// markInSubtree stamps the owner-scope flag onto every row of one half of the
// answer. It mutates the summaries in place and returns them, because
// summariesFor hands back a fresh slice the caller owns.
func markInSubtree(summaries []protocol.ChildSummary, inSubtree bool) []protocol.ChildSummary {
	for i := range summaries {
		summaries[i].InSubtree = &inSubtree
	}
	return summaries
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
