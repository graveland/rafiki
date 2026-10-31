// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tags"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

func tagCaller(ownerUserID, childID string) connectapi.TagCaller {
	return connectapi.TagCaller{Owner: users.Identity{UserID: ownerUserID}, ChildID: childID}
}

func (f *forkFixture) createTag(t *testing.T, caller connectapi.TagCaller, in connectapi.TagInput) tags.Tag {
	t.Helper()
	got, err := f.manager().Tag(context.Background(), caller, in)
	assert.NewAborting(t).NoError(err, "create tag %q", in.Name)
	return got
}

// lineage is one T -> P -> C tree plus a sibling of C, each with a completed
// turn in its own conversation.
type lineage struct {
	top, parent, child, sibling string
	childConv                   string
}

func (f *forkFixture) lineageTree(t *testing.T) lineage {
	t.Helper()
	l := lineage{
		top:     uniqueForkName("top"),
		parent:  uniqueForkName("parent"),
		child:   uniqueForkName("child"),
		sibling: uniqueForkName("sib"),
	}
	f.forkableChild(t, l.top, "", l.top, f.owner, 1)
	f.forkableChild(t, l.parent, l.top, l.top, f.owner, 1)
	l.childConv, _ = f.forkableChild(t, l.child, l.parent, l.top, f.owner, 1)
	f.forkableChild(t, l.sibling, l.parent, l.top, f.owner, 1)
	return l
}

// turns appends n completed turns to convID and returns the reply rows.
func (f *forkFixture) turns(t *testing.T, convID string, n int) []store.Appended {
	t.Helper()
	out := make([]store.Appended, 0, n)
	for range n {
		out = append(out, f.turn(t, convID))
	}
	return out
}

func cursorAt(n store.Appended) string { return fmt.Sprintf("m:%d", n.Ordinal) }

func TestTagNonCreatorInScopeCanForkButNotMove(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	name := uniqueForkName("shared")

	// The parent creates the tag on itself, so the tag's lineage root is the
	// parent: its child is in scope but is neither the creator nor an ancestor
	// of it.
	created := f.createTag(t, tagCaller(f.owner, l.parent), connectapi.TagInput{Name: name, ChildID: l.parent})
	node, err := f.ctrl.resolveFork(ctx, forkRequest(l.child, nil, name), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "an in-scope non-creator may fork from the tag")
	ck.Eq(created.NodeID, node, "the tag's node")

	refusal := fmt.Sprintf("only the tag's creator, its ancestors or a user credential may move or delete tag %q", name)
	_, err = f.manager().Tag(ctx, tagCaller(f.owner, l.child), connectapi.TagInput{
		TagID: created.TagID, ExpectNodeID: created.NodeID,
	})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied, refusal)

	requireExactControllerError(t, f.manager().Untag(ctx, tagCaller(f.owner, l.child), created.TagID),
		protocol.ErrPermissionDenied, refusal)
}

func TestTagAncestorOfClosedCreatorCanMove(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	nodes := f.turns(t, l.childConv, 2)

	created := f.createTag(t, tagCaller(f.owner, l.child), connectapi.TagInput{
		Name: uniqueForkName("closed"), ChildID: l.child, Cursor: cursorAt(nodes[0]),
	})
	ck.Require().NoError(f.store.Delete(ctx, l.child), "close the creator")

	moved, err := f.manager().Tag(ctx, tagCaller(f.owner, l.parent), connectapi.TagInput{
		TagID: created.TagID, ExpectNodeID: nodes[0].ID, Cursor: cursorAt(nodes[1]),
	})
	ck.Require().NoError(err, "an ancestor of a CLOSED creator may still move the tag")
	ck.Eq(nodes[1].ID, moved.NodeID, "the tag moved")
}

func TestTagMoveWithStaleExpectedNodeFails(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	self := uniqueForkName("stale")
	_, nodes := f.forkableChild(t, self, "", self, f.owner, 2)
	name := uniqueForkName("stale")

	created := f.createTag(t, tagCaller(f.owner, self), connectapi.TagInput{Name: name, ChildID: self})
	ck.Eq(nodes[1].ID, created.NodeID, "the tag is on the last completed turn")

	_, err := f.manager().Tag(ctx, tagCaller(f.owner, self), connectapi.TagInput{
		TagID: created.TagID, ExpectNodeID: nodes[0].ID, Cursor: cursorAt(nodes[0]),
	})
	requireExactControllerError(t, err, protocol.ErrFailedPrecondition,
		fmt.Sprintf("tag %q points at %s, not %s; re-read it and retry", name, nodes[1].ID, nodes[0].ID))
}

func TestTagShadowingLineageBeatsTreeBeatsOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	nodes := f.turns(t, l.childConv, 3)
	name := uniqueForkName("shadow")
	caller := tagCaller(f.owner, l.child)

	lineageTag := f.createTag(t, caller, connectapi.TagInput{Name: name, ChildID: l.child, Cursor: cursorAt(nodes[0])})
	treeTag := f.createTag(t, caller, connectapi.TagInput{
		Name: name, ChildID: l.child, Scope: tags.ScopeTree, Cursor: cursorAt(nodes[1]),
	})
	ownerTag := f.createTag(t, caller, connectapi.TagInput{
		Name: name, ChildID: l.child, Scope: tags.ScopeOwner, Cursor: cursorAt(nodes[2]),
	})

	for _, want := range []struct {
		drop   string
		nodeID string
		label  string
	}{
		{lineageTag.TagID, nodes[0].ID, "lineage wins"},
		{treeTag.TagID, nodes[1].ID, "tree is next"},
		{ownerTag.TagID, nodes[2].ID, "owner is last"},
	} {
		got, err := f.ctrl.resolveFork(ctx, forkRequest(l.child, nil, name), users.Identity{UserID: f.owner})
		ck.Require().NoError(err, want.label)
		ck.Eq(want.nodeID, got, want.label)
		ck.Require().NoError(f.manager().Untag(ctx, caller, want.drop), "untag %s", want.label)
	}
}

func TestTagTreeScopeVisibleToCousin(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)

	name := uniqueForkName("tree")
	created := f.createTag(t, tagCaller(f.owner, l.child), connectapi.TagInput{
		Name: name, ChildID: l.child, Scope: tags.ScopeTree,
	})
	node, err := f.ctrl.resolveFork(ctx, forkRequest(l.sibling, nil, name), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a cousin in the same tree is in scope")
	ck.Eq(created.NodeID, node, "the tree tag's node")
}

func TestTagLineageScopeNotVisibleToSibling(t *testing.T) {
	f := newForkFixture(t)
	l := f.lineageTree(t)
	name := uniqueForkName("lineage")
	f.createTag(t, tagCaller(f.owner, l.child), connectapi.TagInput{Name: name, ChildID: l.child})

	_, err := f.ctrl.resolveFork(context.Background(), forkRequest(l.sibling, nil, name),
		users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied,
		fmt.Sprintf("tag %q is not visible to you", name))
}

func TestTagUserWithoutParentSeesOwnerScopeOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	self := uniqueForkName("owneronly")
	_, nodes := f.forkableChild(t, self, "", self, f.owner, 2)
	name := uniqueForkName("mine")
	user := tagCaller(f.owner, "")

	f.createTag(t, user, connectapi.TagInput{Name: name, ChildID: self, Cursor: cursorAt(nodes[0])})
	_, err := f.ctrl.resolveFork(ctx, forkRequest("", nil, name), users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied,
		fmt.Sprintf("tag %q is not visible to you", name))

	ownerTag := f.createTag(t, user, connectapi.TagInput{
		Name: name, ChildID: self, Scope: tags.ScopeOwner, Cursor: cursorAt(nodes[1]),
	})
	node, err := f.ctrl.resolveFork(ctx, forkRequest("", nil, name), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a user with no parent resolves the owner-scope tag")
	ck.Eq(ownerTag.NodeID, node, "the owner-scope tag's node")
}

func TestTagReplaceRefusedToChild(t *testing.T) {
	f := newForkFixture(t)
	self := uniqueForkName("replace")
	f.forkableChild(t, self, "", self, f.owner, 1)

	_, err := f.manager().Tag(context.Background(), tagCaller(f.owner, self), connectapi.TagInput{
		Name: uniqueForkName("taken"), ChildID: self, Replace: true,
	})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied,
		"only a user credential may take a tag name over")
}

func TestTagCrossOwnerResolutionIsIdenticalPermissionDenied(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	self := uniqueForkName("crossthing")
	f.forkableChild(t, self, "", self, f.owner, 1)
	name := uniqueForkName("theirs")
	f.createTag(t, tagCaller(f.owner, ""), connectapi.TagInput{Name: name, ChildID: self, Scope: tags.ScopeOwner})

	other, _, err := usersdb.NewPostgresStore(f.pool).Create(ctx,
		users.NewUser{Username: uniqueForkName("other")})
	ck.Require().NoError(err, "create the other owner")
	unknown := uniqueForkName("nobody")

	_, foreignErr := f.ctrl.resolveFork(ctx, forkRequest("", nil, name), users.Identity{UserID: other.ID})
	_, unknownErr := f.ctrl.resolveFork(ctx, forkRequest("", nil, unknown), users.Identity{UserID: other.ID})
	// The two texts differ only in the name the caller itself supplied, so it
	// learns nothing about which tags exist: one template answers both.
	requireExactControllerError(t, foreignErr, protocol.ErrPermissionDenied,
		fmt.Sprintf("tag %q is not visible to you", name))
	requireExactControllerError(t, unknownErr, protocol.ErrPermissionDenied,
		fmt.Sprintf("tag %q is not visible to you", unknown))
}

func TestListTagsNearestFirstAndLimited(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	user := tagCaller(f.owner, "")

	atSelf := f.createTag(t, user, connectapi.TagInput{Name: uniqueForkName("a"), ChildID: l.child})
	atParent := f.createTag(t, user, connectapi.TagInput{Name: uniqueForkName("b"), ChildID: l.parent})
	atTop := f.createTag(t, user, connectapi.TagInput{Name: uniqueForkName("c"), ChildID: l.top})
	atTree := f.createTag(t, user, connectapi.TagInput{
		Name: uniqueForkName("d"), ChildID: l.top, Scope: tags.ScopeTree,
	})
	atOwner := f.createTag(t, user, connectapi.TagInput{
		Name: uniqueForkName("e"), ChildID: l.top, Scope: tags.ScopeOwner,
	})

	all, err := f.manager().ListTags(ctx, tagCaller(f.owner, l.child), "", 500, "")
	ck.Require().NoError(err, "list")
	ck.EqDeep([]string{atSelf.TagID, atParent.TagID, atTop.TagID, atTree.TagID, atOwner.TagID},
		tagIDs(all), "lineage nearest first, then tree, then owner")

	limited, err := f.manager().ListTags(ctx, tagCaller(f.owner, l.child), "", 2, "")
	ck.Require().NoError(err, "list with a limit")
	ck.EqDeep([]string{atSelf.TagID, atParent.TagID}, tagIDs(limited), "the limit keeps the nearest")
}

// An untag tombstones the tag's live row, so a history read must not go
// through the live-only lookup: the tombstone is part of the audit trail.
func TestListTagsReadsAnUntaggedTagsHistory(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	caller := tagCaller(f.owner, l.child)

	created := f.createTag(t, caller, connectapi.TagInput{Name: uniqueForkName("history"), ChildID: l.child})
	ck.Require().NoError(f.manager().Untag(ctx, caller, created.TagID), "untag")

	hist, err := f.manager().ListTags(ctx, caller, "", 0, created.TagID)
	ck.Require().NoError(err, "an untagged tag's history is still readable")
	ck.Require().Eq(1, len(hist), "one version")
	ck.Eq(created.TagID, hist[0].TagID, "the same tag id")
	ck.Eq(created.NodeID, hist[0].NodeID, "the node it named")
	ck.NotNil(hist[0].DeletedAt, "the tombstone is part of the history")
}

// A move appends a row and supersedes the one it replaces, so a history read
// lists every version of the tag, oldest first, live row included.
func TestListTagsReadsEveryVersionOfAMovedTag(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	nodes := f.turns(t, l.childConv, 2)
	caller := tagCaller(f.owner, l.child)

	created := f.createTag(t, caller, connectapi.TagInput{
		Name: uniqueForkName("moved"), ChildID: l.child, Cursor: cursorAt(nodes[0]),
	})
	_, err := f.manager().Tag(ctx, caller, connectapi.TagInput{
		TagID: created.TagID, ExpectNodeID: nodes[0].ID, Cursor: cursorAt(nodes[1]),
	})
	ck.Require().NoError(err, "move")

	hist, err := f.manager().ListTags(ctx, caller, "", 0, created.TagID)
	ck.Require().NoError(err, "list")
	ck.Require().Eq(2, len(hist), "both versions")
	ck.EqDeep([]string{nodes[0].ID, nodes[1].ID}, tagNodeIDs(hist), "oldest first")
	ck.NotNil(hist[0].SupersededAt, "the first row is superseded")
	ck.True(hist[1].SupersededAt == nil && hist[1].DeletedAt == nil, "the last row is live")
}

// The history fallback must not widen what a caller may probe: an id that has
// no row at all and another owner's id answer the one not-found.
func TestListTagsUnknownAndForeignTagIDsAreNotFound(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	l := f.lineageTree(t)
	created := f.createTag(t, tagCaller(f.owner, l.child), connectapi.TagInput{
		Name: uniqueForkName("mine"), ChildID: l.child,
	})

	other, _, err := usersdb.NewPostgresStore(f.pool).Create(ctx,
		users.NewUser{Username: uniqueForkName("other")})
	ck.Require().NoError(err, "create the other owner")

	for _, want := range []struct {
		tagID string
		label string
	}{
		{uuid.Must(uuid.NewV7()).String(), "an unknown id"},
		{created.TagID, "a foreign owner's id"},
	} {
		_, err := f.manager().ListTags(ctx, tagCaller(other.ID, ""), "", 0, want.tagID)
		ck.True(err != nil, "%s must not answer a history", want.label)
		requireExactControllerError(t, err, protocol.ErrNotFound,
			fmt.Sprintf("tag %s not found", want.tagID))
	}
}

func tagNodeIDs(rows []tags.Tag) []string {
	out := make([]string, 0, len(rows))
	for _, t := range rows {
		out = append(out, t.NodeID)
	}
	return out
}

func tagIDs(rows []tags.Tag) []string {
	out := make([]string, 0, len(rows))
	for _, t := range rows {
		out = append(out, t.TagID)
	}
	return out
}
