// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/tags"
	"go.graveland.dev/rafiki/pkg/users"
)

// forkAncestorHopLimit bounds the parent-chain walk. A chain deeper than this
// is refused rather than followed: no legitimate tree is that deep, and an
// unbounded walk is how a cycle or a corrupted row becomes a hang.
const forkAncestorHopLimit = 64

// forkSourceStore is the child store view a fork resolves authority through.
// Declared at the consumer because childstore.ChildStore deliberately does not
// carry ForkSource, so a store that lost it would silently leave every fork
// answering "not visible".
type forkSourceStore interface {
	ForkSource(ctx context.Context, childID string) (childstore.ForkSource, bool, error)
}

// SetForkSource records the child store a fork reads authority from: the
// parent chain and a source's owner, conversation and kind. Wired once at
// startup beside the lineage source, before the socket accepts anything.
//
// A nil interface is REFUSED and leaves the field nil (the discipline every
// other Set* applies): an absent source must stay absent, so ancestorsOf
// fails closed rather than nil-panicking on the first ForkSource call.
func (c *Controller) SetForkSource(src forkSourceStore) {
	if src == nil {
		return
	}
	c.forks = src
}

// SetTagStore records the tag store a from_tag spawn resolves through. A nil
// store is REFUSED: a from_tag spawn on a daemon without one must answer
// "tags are unavailable" rather than nil-panic.
func (c *Controller) SetTagStore(st tagStore) {
	if st == nil {
		return
	}
	c.tagStore = st
}

// wireForkSource gives ctrl the child store's fork view when the store can
// provide one. Extracted from NewController so the wiring is pinned with a
// stub: the Controller's children field is the narrower ChildStore, which does
// NOT include ForkSource, so a store that lost it would leave every fork
// refusing as "not visible" with nothing failing to compile.
func wireForkSource(ctrl *Controller, store childstore.ChildStore) {
	if src, ok := store.(forkSourceStore); ok {
		ctrl.SetForkSource(src)
	}
}

// notVisibleErr is the ONE answer for a source that is foreign, unknown or
// outside the caller's subtree. One text for all three, so a caller cannot
// probe which children exist by asking.
func notVisibleErr(childID string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrPermissionDenied,
		Message: fmt.Sprintf("agent %s is not visible to you", childID),
	}
}

// tagUnavailableErr is the answer when the daemon has no tag store, which is
// the same condition as having no database at all.
func tagUnavailableErr() error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrFailedPrecondition,
		Message: "tags are unavailable on this daemon (no database)",
	}
}

// forkNodeErr is the answer when a fork is asked for on a daemon with no
// conversation store.
func forkNodeErr() error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrFailedPrecondition,
		Message: "forks are unavailable on this daemon (no database)",
	}
}

// ancestorsOf returns childID's parent chain, nearest first, closed rows
// included — a closed child's conversation is exactly what a fork still needs.
// A chain longer than forkAncestorHopLimit, a cycle, or a lookup failure is an
// error; every caller refuses on error rather than treating it as "no
// ancestors", because an empty chain is the most permissive answer.
func (c *Controller) ancestorsOf(ctx context.Context, childID string) ([]string, error) {
	if childID == "" {
		return nil, nil
	}
	if c.forks == nil {
		return nil, fmt.Errorf("no child store to resolve the parent chain of %s", childID)
	}
	seen := map[string]bool{childID: true}
	chain := make([]string, 0, 4)
	cur := childID
	for range forkAncestorHopLimit {
		src, ok, err := c.forks.ForkSource(ctx, cur)
		if err != nil {
			return nil, fmt.Errorf("parent chain of %s: %w", childID, err)
		}
		if !ok {
			return nil, fmt.Errorf("parent chain of %s names unknown child %s", childID, cur)
		}
		parent := src.ParentID
		if parent == "" {
			return chain, nil
		}
		if seen[parent] {
			return nil, fmt.Errorf("parent chain of %s is a cycle at %s", childID, parent)
		}
		seen[parent] = true
		chain = append(chain, parent)
		cur = parent
	}
	return nil, fmt.Errorf("parent chain of %s is deeper than %d hops", childID, forkAncestorHopLimit)
}

// position resolves a caller's place in its owner's tree. A user credential
// has no child id and so no position; anything else is a child, whose chain a
// failed lookup refuses.
func (c *Controller) position(ctx context.Context, callerChildID string) (tags.Position, error) {
	if callerChildID == "" {
		return tags.Position{}, nil
	}
	ancestors, err := c.ancestorsOf(ctx, callerChildID)
	if err != nil {
		return tags.Position{}, err
	}
	return tags.Position{ChildID: callerChildID, Ancestors: ancestors}, nil
}

// localOrAdmin reports whether the request carries the daemon's own local
// trust (the unix socket's nil identity) or an admin user credential — the
// two callers the owner guard also exempts. TagCaller.Owner carries no admin
// bit, so this is the only way the NULL-owner rule is decidable at all.
func localOrAdmin(ctx context.Context) bool {
	id := server.IdentityFromContext(ctx)
	return id == nil || (id.IsUserCredential() && id.IsAdmin)
}

// mayRead reports whether owner/callerChildID may read — and so fork from or
// tag — src. Same owner first; a source with NO owner is readable only by the
// daemon's own local trust or an admin, exactly as the owner guard treats an
// unowned child. Cross-owner is always refused, admin included: owner
// isolation is absolute. A child caller must additionally be the source
// itself or one of its ancestors (so it can fork itself and its descendants,
// never a sibling or its own parent); a user is satisfied by ownership alone.
func (c *Controller) mayRead(ctx context.Context, owner users.Identity, callerChildID string, src childstore.ForkSource) bool {
	switch src.OwnerUserID {
	case owner.UserID:
	case "":
		if !localOrAdmin(ctx) {
			return false
		}
	default:
		return false
	}
	if callerChildID == "" {
		return true
	}
	if src.ChildID == callerChildID {
		return true
	}
	ancestors, err := c.ancestorsOf(ctx, src.ChildID)
	if err != nil {
		return false
	}
	return slices.Contains(ancestors, callerChildID)
}

// forkSource reads a child's durable row. An id that names no child answers
// the same permission-denied text a foreign one does, so a fork attempt cannot
// enumerate children.
func (c *Controller) forkSource(ctx context.Context, childID string) (childstore.ForkSource, error) {
	if c.forks == nil {
		return childstore.ForkSource{}, fmt.Errorf("no child store to resolve fork source %s", childID)
	}
	src, ok, err := c.forks.ForkSource(ctx, childID)
	if err != nil {
		return childstore.ForkSource{}, err
	}
	if !ok {
		return childstore.ForkSource{}, notVisibleErr(childID)
	}
	return src, nil
}

// isCompletedTurn reports whether n is a turn a fork may start after: an
// assistant message that left no tool call pending. A dangling tool_use is the
// one shape a fork must skip, because the borrowed history would ask the new
// child's model to answer a call that was never answered.
func isCompletedTurn(n *conv.Node) bool {
	if n == nil || n.Msg.Role != msg.Assistant {
		return false
	}
	for _, block := range n.Msg.Content {
		if _, ok := block.(msg.ToolUse); ok {
			return false
		}
	}
	return true
}

// parseForkCursor reads "m:<ordinal>" or "m:<ordinal>.<sub>". <sub> names a
// message within a row and is ignored: a fork point is a node, not a message.
func parseForkCursor(cursor string) (int, bool) {
	rest, ok := strings.CutPrefix(cursor, "m:")
	if !ok {
		return 0, false
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		if !allDigits(rest[i+1:]) {
			return 0, false
		}
		rest = rest[:i]
	}
	if !allDigits(rest) {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// malformedCursorErr and notCompletedErr are the two cursor refusals; both
// name the cursor as the caller wrote it.
func malformedCursorErr(cursor string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: fmt.Sprintf("malformed cursor %q (want m:<ordinal>)", cursor),
	}
}

func notCompletedErr(cursor string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: fmt.Sprintf("cursor %s is not a completed turn (an assistant message with no pending tool call)", cursor),
	}
}

// forkNode resolves the node a fork of convID starts after. An empty cursor
// means the last completed turn on the working path; a cursor names an
// ordinal in the whole tree, which must itself be a completed turn.
//
// The handle is opened WITHOUT AsOwner: the source conversation's writer may
// be another process, so this read must ask the backend for the head.
func (c *Controller) forkNode(ctx context.Context, convID, cursor string) (string, error) {
	if c.convs == nil {
		return "", forkNodeErr()
	}
	h := c.convs.Open(convID)
	if cursor == "" {
		nodes, err := h.Working(ctx)
		if err != nil {
			return "", fmt.Errorf("fork: read working path of %s: %w", convID, err)
		}
		for i := len(nodes) - 1; i >= 0; i-- {
			if isCompletedTurn(nodes[i]) {
				return nodes[i].ID, nil
			}
		}
		return "", &connectapi.ControllerError{
			Code:    protocol.ErrFailedPrecondition,
			Message: "agent has no completed turn to fork from",
		}
	}
	ordinal, ok := parseForkCursor(cursor)
	if !ok {
		return "", malformedCursorErr(cursor)
	}
	snap, err := h.Snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("fork: read conversation %s: %w", convID, err)
	}
	for n := range snap.History() {
		if n.Ordinal != ordinal {
			continue
		}
		if !isCompletedTurn(n) {
			return "", notCompletedErr(cursor)
		}
		return n.ID, nil
	}
	return "", notCompletedErr(cursor)
}

// visibleTag reports whether pos may fork from t, resolving the lineage root's
// own ancestor chain only when the scope needs it.
//
// A caller with no position is a user credential with no parent: it belongs to
// no lineage and no tree, so it sees owner-scope tags only. A caller that
// NAMED a parent forks with that parent's position (see resolveFork), so it is
// filtered like a child.
func (c *Controller) visibleTag(ctx context.Context, pos tags.Position, t tags.Tag) (bool, error) {
	if pos.ChildID == "" {
		return t.Scope == tags.ScopeOwner, nil
	}
	var rootAncestors []string
	if t.Scope == tags.ScopeLineage {
		ancestors, err := c.ancestorsOf(ctx, t.ScopeRoot)
		if err != nil {
			return false, err
		}
		rootAncestors = ancestors
	}
	return tags.InScope(t, pos, rootAncestors), nil
}

// resolveFork turns a spawn's fork_from / from_tag into the concrete node the
// new child's conversation forks from, or "" when the spawn asks for no fork.
//
// It runs ONCE, before anything is minted or written, and the node it returns
// is what the child carries from then on: moving a tag never reaches an
// existing fork, and a resume never re-resolves.
//
// The caller's position is req.ParentChildID. Connect forces that to the
// calling child for a child credential and the fundi spawner sets it to
// itself; a USER that names a parent therefore forks with that parent's
// authority, which is deliberate — naming a parent is asking to act as one.
//
// The second return is the SOURCE child id, the provenance the spawn stamps
// as the rafiki/forked-from label: for fork_from it is the child the caller
// named, for from_tag the child whose conversation the winning tag's node
// belongs to (SourceChildID — a MOVED tag keeps the node's child, not the
// mover's).
func (c *Controller) resolveFork(ctx context.Context, req protocol.SpawnRequest, owner users.Identity) (nodeID, sourceChild string, err error) {
	if req.ForkFrom == nil && req.FromTag == "" {
		return "", "", nil
	}
	if req.ForkFrom != nil && req.FromTag != "" {
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "fork_from and from_tag are mutually exclusive",
		}
	}
	if req.Kind != protocol.KindFundi {
		return "", "", &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "a fork runs as kind=fundi; claude and script children cannot be forked to",
		}
	}
	callerChildID := req.ParentChildID
	if req.ForkFrom != nil {
		src, err := c.forkSource(ctx, req.ForkFrom.ChildID)
		if err != nil {
			return "", "", err
		}
		if !c.mayRead(ctx, owner, callerChildID, src) {
			return "", "", notVisibleErr(req.ForkFrom.ChildID)
		}
		if src.ConversationID == "" {
			return "", "", noConversationErr(src.ChildID)
		}
		node, err := c.forkNode(ctx, src.ConversationID, req.ForkFrom.Cursor)
		return node, src.ChildID, err
	}
	tag, err := c.resolveTagFork(ctx, owner, callerChildID, req.FromTag)
	if err != nil {
		return "", "", err
	}
	return tag.NodeID, tag.SourceChildID, nil
}

// resolveTagFork picks the nearest tag named name that the caller may fork
// from: lineage before tree before owner, and nearest root first within
// lineage. A name that resolves to nothing — foreign, out of scope or
// unknown — answers the ONE permission-denied text. The winning tag is
// returned whole: its node is the fork point and its child the provenance.
func (c *Controller) resolveTagFork(ctx context.Context, owner users.Identity, callerChildID, name string) (tags.Tag, error) {
	if c.tagStore == nil {
		return tags.Tag{}, tagUnavailableErr()
	}
	pos, err := c.position(ctx, callerChildID)
	if err != nil {
		return tags.Tag{}, err
	}
	candidates, err := c.tagStore.ByName(ctx, owner.UserID, name)
	if err != nil {
		return tags.Tag{}, err
	}
	inScope := make([]tags.Tag, 0, len(candidates))
	for _, t := range candidates {
		ok, err := c.visibleTag(ctx, pos, t)
		if err != nil {
			return tags.Tag{}, err
		}
		if ok {
			inScope = append(inScope, t)
		}
	}
	if len(inScope) == 0 {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrPermissionDenied,
			Message: fmt.Sprintf("tag %q is not visible to you", name),
		}
	}
	return tags.Rank(inScope, pos)[0], nil
}
