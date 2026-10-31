// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tags"
	"go.graveland.dev/rafiki/pkg/tagsdb"
)

// tagStore is the tag persistence surface this file drives. Declared at the
// consumer over *tagsdb.Store, which is the one implementation.
type tagStore interface {
	Create(ctx context.Context, t tags.Tag, replace bool) (tags.Tag, error)
	Move(ctx context.Context, ownerUserID, tagID, expectNodeID string, to tags.Tag) (tags.Tag, error)
	Untag(ctx context.Context, ownerUserID, tagID, actorChildID string) error
	Get(ctx context.Context, ownerUserID, tagID string) (tags.Tag, bool, error)
	ByName(ctx context.Context, ownerUserID, name string) ([]tags.Tag, error)
	List(ctx context.Context, ownerUserID, namePrefix string) ([]tags.Tag, error)
	History(ctx context.Context, ownerUserID, tagID string) ([]tags.Tag, error)
}

var _ tagStore = (*tagsdb.Store)(nil)

// tagManager is the daemon's connectapi.TagManager: it resolves a tag's
// authority from the caller's owner and position in the tree, and the node it
// names from the source child's conversation. Everything it reads and writes
// is keyed on the connection's owner, so a foreign tag id is simply not
// found.
type tagManager struct {
	c     *Controller
	store tagStore
}

var _ connectapi.TagManager = (*tagManager)(nil)

func newTagManager(c *Controller, store tagStore) *tagManager {
	return &tagManager{c: c, store: store}
}

// wireTagManager attaches the tag backend to the Connect server and to the
// Controller's from_tag resolution, sharing ONE store. A daemon with no
// database leaves both unwired, so the Tag RPCs answer Unavailable and a
// from_tag spawn refuses rather than nil-panicking.
func wireTagManager(srv *connectapi.Server, ctrl *Controller, store tagStore) {
	if srv == nil || ctrl == nil || store == nil {
		return
	}
	ctrl.SetTagStore(store)
	srv.SetTagManager(newTagManager(ctrl, store))
}

// tagNotFoundErr is the one answer for a tag that does not exist, is foreign,
// or is out of the caller's reach.
func tagNotFoundErr(tagID string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("tag %s not found", tagID),
	}
}

func noConversationErr(childID string) error {
	return &connectapi.ControllerError{
		Code:    protocol.ErrFailedPrecondition,
		Message: fmt.Sprintf("agent %s has no conversation to fork from yet", childID),
	}
}

// Tag creates a tag (TagID empty) or moves it.
func (m *tagManager) Tag(ctx context.Context, caller connectapi.TagCaller, in connectapi.TagInput) (tags.Tag, error) {
	if in.TagID != "" {
		return m.move(ctx, caller, in)
	}
	return m.create(ctx, caller, in)
}

func (m *tagManager) create(ctx context.Context, caller connectapi.TagCaller, in connectapi.TagInput) (tags.Tag, error) {
	if in.ChildID == "" {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "child_id is required to create a tag",
		}
	}
	if err := tags.ValidName(in.Name); err != nil {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}
	if len(in.Note) > tags.MaxNoteLen {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("note must be at most %d bytes", tags.MaxNoteLen),
		}
	}
	// TAG_SCOPE_UNSPECIFIED reaches the store as ScopeUnset and means lineage
	// at CREATE time only — the one place the wire's default is given a
	// meaning. A move never carries a scope at all.
	scope := in.Scope
	if scope == tags.ScopeUnset {
		scope = tags.ScopeLineage
	}
	// replace tombstones the live holder of the name, which is a user's
	// authority alone: a child taking a name over would let it silently
	// repoint a tag its own ancestor created.
	if in.Replace && caller.ChildID != "" {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrPermissionDenied,
			Message: "only a user credential may take a tag name over",
		}
	}
	src, err := m.c.forkSource(ctx, in.ChildID)
	if err != nil {
		return tags.Tag{}, err
	}
	if !m.c.mayRead(ctx, caller.Owner, caller.ChildID, src) {
		return tags.Tag{}, notVisibleErr(in.ChildID)
	}
	if src.ConversationID == "" {
		return tags.Tag{}, noConversationErr(src.ChildID)
	}
	nodeID, err := m.c.forkNode(ctx, src.ConversationID, in.Cursor)
	if err != nil {
		return tags.Tag{}, err
	}
	scopeRoot, err := m.scopeRoot(ctx, caller, src, scope)
	if err != nil {
		return tags.Tag{}, err
	}
	t, err := m.store.Create(ctx, tags.Tag{
		OwnerUserID:    caller.Owner.UserID,
		Name:           in.Name,
		Scope:          scope,
		ScopeRoot:      scopeRoot,
		SourceChildID:  src.ChildID,
		ConversationID: src.ConversationID,
		NodeID:         nodeID,
		CreatorChildID: caller.ChildID,
		ActorChildID:   caller.ChildID,
		Note:           in.Note,
	}, in.Replace)
	if err != nil {
		return tags.Tag{}, tagStoreErr(err, "", in.Name, scope, "")
	}
	return t, nil
}

// scopeRoot resolves what a scope is rooted at. Lineage is the source child
// itself; tree is the caller's top level, or the source's for a user
// credential, which has none; owner is the owner's own user id.
func (m *tagManager) scopeRoot(ctx context.Context, caller connectapi.TagCaller, src childstore.ForkSource, scope tags.Scope) (string, error) {
	switch scope {
	case tags.ScopeLineage:
		return src.ChildID, nil
	case tags.ScopeTree:
		pos, err := m.c.position(ctx, caller.ChildID)
		if err != nil {
			return "", err
		}
		if pos.ChildID != "" {
			return pos.TopLevel(), nil
		}
		if src.RootID != "" {
			return src.RootID, nil
		}
		return src.ChildID, nil
	case tags.ScopeOwner:
		return caller.Owner.UserID, nil
	}
	return "", &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: "scope must be LINEAGE, TREE or OWNER",
	}
}

func (m *tagManager) move(ctx context.Context, caller connectapi.TagCaller, in connectapi.TagInput) (tags.Tag, error) {
	if in.ExpectNodeID == "" {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "expect_node_id is required to move a tag",
		}
	}
	// The handler does not police the move shape, so the manager does: a move
	// names the tag, where the caller believes it points, and where it goes.
	// A create-only field arriving here is a caller that meant to create.
	if in.Name != "" || in.Scope != tags.ScopeUnset || in.Replace {
		return tags.Tag{}, &connectapi.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "a move sets only tag_id, child_id, cursor and expect_node_id",
		}
	}
	t, err := m.get(ctx, caller.Owner.UserID, in.TagID)
	if err != nil {
		return tags.Tag{}, err
	}
	if err := m.authorizeModify(ctx, caller.ChildID, t); err != nil {
		return tags.Tag{}, err
	}
	// A move that names no child re-points the tag inside the conversation it
	// already names, which is what the wire's move shape sends.
	targetChild := in.ChildID
	if targetChild == "" {
		targetChild = t.SourceChildID
	}
	src, err := m.c.forkSource(ctx, targetChild)
	if err != nil {
		return tags.Tag{}, err
	}
	if !m.c.mayRead(ctx, caller.Owner, caller.ChildID, src) {
		return tags.Tag{}, notVisibleErr(targetChild)
	}
	if src.ConversationID == "" {
		return tags.Tag{}, noConversationErr(src.ChildID)
	}
	nodeID, err := m.c.forkNode(ctx, src.ConversationID, in.Cursor)
	if err != nil {
		return tags.Tag{}, err
	}
	moved, err := m.store.Move(ctx, caller.Owner.UserID, in.TagID, in.ExpectNodeID, tags.Tag{
		SourceChildID:  src.ChildID,
		ConversationID: src.ConversationID,
		NodeID:         nodeID,
		ActorChildID:   caller.ChildID,
	})
	if err != nil {
		return tags.Tag{}, tagStoreErr(err, in.TagID, t.Name, t.Scope, in.ExpectNodeID)
	}
	return moved, nil
}

// Untag tombstones the tag's live row.
func (m *tagManager) Untag(ctx context.Context, caller connectapi.TagCaller, tagID string) error {
	t, err := m.get(ctx, caller.Owner.UserID, tagID)
	if err != nil {
		return err
	}
	if err := m.authorizeModify(ctx, caller.ChildID, t); err != nil {
		return err
	}
	if err := m.store.Untag(ctx, caller.Owner.UserID, tagID, caller.ChildID); err != nil {
		return tagStoreErr(err, tagID, t.Name, t.Scope, "")
	}
	return nil
}

// ListTags answers one tag's whole history when tagID is set — an untagged
// tag's too — and otherwise the live tags the caller can see, nearest scope
// first and truncated to limit.
func (m *tagManager) ListTags(ctx context.Context, caller connectapi.TagCaller, namePrefix string, limit int, tagID string) ([]tags.Tag, error) {
	pos, err := m.c.position(ctx, caller.ChildID)
	if err != nil {
		return nil, err
	}
	if tagID != "" {
		return m.tagHistory(ctx, pos, caller.Owner.UserID, tagID)
	}
	rows, err := m.store.List(ctx, caller.Owner.UserID, namePrefix)
	if err != nil {
		return nil, err
	}
	// A user credential sees every tag it owns. A child sees the ones in its
	// own scope, which is what makes a listing agree with what it could fork
	// from.
	kept := rows
	if pos.ChildID != "" {
		kept = make([]tags.Tag, 0, len(rows))
		for _, t := range rows {
			ok, err := m.c.visibleTag(ctx, pos, t)
			if err != nil {
				return nil, err
			}
			if ok {
				kept = append(kept, t)
			}
		}
	}
	ranked := tags.Rank(kept, pos)
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked, nil
}

// tagHistory answers one tag's whole history, oldest first. The live row is
// the ordinary case; a tag that has been untagged has no live row left, so the
// visibility check runs against the newest row the history still holds — a
// tag's scope and scope root are fixed for its life (a move refuses `scope`),
// so any row speaks for the tag — and a history with no rows keeps the same
// not-found a foreign id gets.
func (m *tagManager) tagHistory(ctx context.Context, pos tags.Position, ownerUserID, tagID string) ([]tags.Tag, error) {
	t, ok, err := m.store.Get(ctx, ownerUserID, tagID)
	if err != nil {
		return nil, err
	}
	if ok {
		if err := m.requireTagVisible(ctx, pos, t, tagID); err != nil {
			return nil, err
		}
		return m.store.History(ctx, ownerUserID, tagID)
	}
	hist, err := m.store.History(ctx, ownerUserID, tagID)
	if err != nil {
		return nil, err
	}
	if len(hist) == 0 {
		return nil, tagNotFoundErr(tagID)
	}
	if err := m.requireTagVisible(ctx, pos, hist[len(hist)-1], tagID); err != nil {
		return nil, err
	}
	return hist, nil
}

// requireTagVisible refuses a tag the caller can neither see nor modify. One
// not-found for all of those, so an id cannot be probed.
func (m *tagManager) requireTagVisible(ctx context.Context, pos tags.Position, t tags.Tag, tagID string) error {
	visible, err := m.c.visibleTag(ctx, pos, t)
	if err != nil {
		return err
	}
	if !visible {
		if visible, err = m.mayModify(ctx, pos, t); err != nil {
			return err
		}
	}
	if !visible {
		return tagNotFoundErr(tagID)
	}
	return nil
}

// get reads the caller's own live tag, mapping an unknown id to not-found.
func (m *tagManager) get(ctx context.Context, ownerUserID, tagID string) (tags.Tag, error) {
	t, ok, err := m.store.Get(ctx, ownerUserID, tagID)
	if err != nil {
		return tags.Tag{}, err
	}
	if !ok {
		return tags.Tag{}, tagNotFoundErr(tagID)
	}
	return t, nil
}

// authorizeModify refuses a move or untag the caller has no authority over.
func (m *tagManager) authorizeModify(ctx context.Context, callerChildID string, t tags.Tag) error {
	pos, err := m.c.position(ctx, callerChildID)
	if err != nil {
		return err
	}
	ok, err := m.mayModify(ctx, pos, t)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return &connectapi.ControllerError{
		Code:    protocol.ErrPermissionDenied,
		Message: fmt.Sprintf("only the tag's creator, its ancestors or a user credential may move or delete tag %q", t.Name),
	}
}

// mayModify is tags.CanModify with the creator's chain resolved. A
// user-created tag needs no walk: only a user credential may touch it, and
// CanModify answers that from the position alone.
func (m *tagManager) mayModify(ctx context.Context, pos tags.Position, t tags.Tag) (bool, error) {
	if pos.ChildID == "" || t.CreatorChildID == "" {
		return tags.CanModify(t, pos, nil), nil
	}
	creatorAncestors, err := m.c.ancestorsOf(ctx, t.CreatorChildID)
	if err != nil {
		return false, err
	}
	return tags.CanModify(t, pos, creatorAncestors), nil
}

// tagStoreErr maps the store's sentinels onto the caller-visible errors.
func tagStoreErr(err error, tagID, name string, scope tags.Scope, expectNodeID string) error {
	var moved tagsdb.ErrMoved
	switch {
	case errors.As(err, &moved):
		return &connectapi.ControllerError{
			Code:    protocol.ErrFailedPrecondition,
			Message: fmt.Sprintf("tag %q points at %s, not %s; re-read it and retry", name, moved.Current, expectNodeID),
		}
	case errors.Is(err, tagsdb.ErrNameTaken):
		return &connectapi.ControllerError{
			Code:    protocol.ErrAlreadyExists,
			Message: fmt.Sprintf("tag %q already exists in this %s scope; move it by tag_id or pick another name", name, scope),
		}
	case errors.Is(err, tagsdb.ErrNotFound):
		if tagID == "" {
			return err
		}
		return tagNotFoundErr(tagID)
	}
	return err
}
