// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tags"

	"github.com/multigres/testkit/assert"
)

// fakeTagManager records what the tag handlers asked it to do, so a test can
// prove both the wire->domain mapping and that a refused call never reached
// the store.
type fakeTagManager struct {
	rows []tags.Tag

	calls []string

	gotCaller connectapi.TagCaller
	gotInput  connectapi.TagInput

	gotUntagID string

	gotPrefix string
	gotLimit  int
	gotTagID  string
}

func (f *fakeTagManager) Tag(_ context.Context, caller connectapi.TagCaller, in connectapi.TagInput) (tags.Tag, error) {
	f.calls = append(f.calls, "Tag")
	f.gotCaller, f.gotInput = caller, in
	if len(f.rows) == 0 {
		return tags.Tag{}, nil
	}
	return f.rows[0], nil
}

func (f *fakeTagManager) Untag(_ context.Context, caller connectapi.TagCaller, tagID string) error {
	f.calls = append(f.calls, "Untag")
	f.gotCaller, f.gotUntagID = caller, tagID
	return nil
}

func (f *fakeTagManager) ListTags(_ context.Context, caller connectapi.TagCaller, namePrefix string, limit int, tagID string) ([]tags.Tag, error) {
	f.calls = append(f.calls, "ListTags")
	f.gotCaller, f.gotPrefix, f.gotLimit, f.gotTagID = caller, namePrefix, limit, tagID
	return f.rows, nil
}

// TestSetTagManagerNilIsRefused pins the nil-interface trap: storing &m for a
// nil manager would make the pointer non-nil, so the handler would take the
// wired path and nil-panic on the first call instead of answering Unavailable.
func TestSetTagManagerNilIsRefused(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(nil)
	s.SetTagManager(nil)

	_, err := s.Tag(context.Background(), connect.NewRequest(&rafikiv1.TagRequest{
		Name: "t", ChildId: "c_1",
	}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "Tag after SetTagManager(nil) = %v, want Unavailable", err)
}

// TestTagUnwiredIsUnavailable pins the unwired answer for all three verbs: a
// daemon that has not wired the tag store fails closed rather than reporting a
// tag that reached nothing.
func TestTagUnwiredIsUnavailable(t *testing.T) {
	c := assert.NewAborting(t)
	s := connectapi.NewServer(nil)
	ctx := context.Background()

	_, err := s.Tag(ctx, connect.NewRequest(&rafikiv1.TagRequest{Name: "t", ChildId: "c_1"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "Tag unwired = %v, want Unavailable", err)
	c.StrContains(err.Error(), "tags not yet wired", "Tag unwired message")

	_, err = s.Untag(ctx, connect.NewRequest(&rafikiv1.UntagRequest{TagId: "t1"}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "Untag unwired = %v, want Unavailable", err)

	_, err = s.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "ListTags unwired = %v, want Unavailable", err)
}

// TestTagForeignChildRefusedLikeUnknown pins the owner guard on Tag's
// child_id: a non-admin user may tag only a child it owns, and a foreign id
// gets the guard's one identical PermissionDenied — the same answer an unknown
// id gets — before the store is touched. A user caller naming no child is not
// the guard's business and reaches the manager.
func TestTagForeignChildRefusedLikeUnknown(t *testing.T) {
	c := assert.NewAborting(t)
	for _, childID := range []string{"c_foreign", "c_unknown"} {
		m := &fakeTagManager{}
		s := connectapi.NewServer(nil)
		s.SetTagManager(m)
		s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard {
			return &fakeOwnerGuard{owned: map[string]bool{"c_mine": true}}
		})

		_, err := s.Tag(context.Background(), connect.NewRequest(&rafikiv1.TagRequest{
			Name: "t", ChildId: childID,
		}))
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Tag on %q = %v, want PermissionDenied", childID, err)
		c.Eq(0, len(m.calls), "Tag on %q reached the store: %v", childID, m.calls)
	}

	m := &fakeTagManager{}
	s := connectapi.NewServer(nil)
	s.SetTagManager(m)
	s.SetOwnerGuardSource(func(context.Context) connectapi.OwnerGuard {
		return &fakeOwnerGuard{owned: map[string]bool{"c_mine": true}}
	})
	_, err := s.Tag(context.Background(), connect.NewRequest(&rafikiv1.TagRequest{
		Name: "t", ChildId: "c_mine",
	}))
	c.NoError(err, "Tag on an owned child")
	c.Eq(1, len(m.calls), "Tag on an owned child reached the store")

	_, err = s.Tag(context.Background(), connect.NewRequest(&rafikiv1.TagRequest{Name: "t"}))
	c.NoError(err, "a user naming no child is not a guarded target")
	c.Eq(2, len(m.calls), "Tag with no child_id reached the store")
}

// TestTagRowRoundTrip pins both directions of the wire mapping: the request's
// scope, cursor and replace reach the manager, and the stored row comes back
// with the scope enum, creator_is_user for a user-created tag, and no
// timestamps for the two unset ones.
func TestTagRowRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	created := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	superseded := created.Add(time.Hour)

	m := &fakeTagManager{rows: []tags.Tag{{
		TagID:         "t1",
		Name:          "fix-bug",
		Scope:         tags.ScopeTree,
		ScopeRoot:     "c_root",
		SourceChildID: "c_src",
		NodeID:        "n1",
		Ordinal:       7,
		Note:          "before the rewrite",
		CreatedAt:     created,
		SupersededAt:  &superseded,
	}}}
	s := connectapi.NewServer(nil)
	s.SetTagManager(m)

	resp, err := s.Tag(context.Background(), connect.NewRequest(&rafikiv1.TagRequest{
		Name:         "fix-bug",
		Scope:        rafikiv1.TagScope_TAG_SCOPE_TREE,
		ChildId:      "c_src",
		Cursor:       "m:7",
		Note:         "before the rewrite",
		Replace:      true,
		TagId:        "t1",
		ExpectNodeId: "n1",
	}))
	c.NoError(err, "Tag")
	c.Eq(tags.ScopeTree, m.gotInput.Scope, "scope reached the manager")
	c.Eq("m:7", m.gotInput.Cursor, "cursor reached the manager")
	c.Eq("n1", m.gotInput.ExpectNodeID, "expect_node_id reached the manager")
	c.Eq(true, m.gotInput.Replace, "replace reached the manager")
	c.Eq("", m.gotCaller.ChildID, "a user caller carries no child id")

	row := resp.Msg.GetTag()
	c.Require().NotNil(row, "Tag response carries no row")
	c.Eq("t1", row.GetTagId(), "tag_id")
	c.Eq("fix-bug", row.GetName(), "name")
	c.Eq(rafikiv1.TagScope_TAG_SCOPE_TREE, row.GetScope(), "scope")
	c.Eq("c_root", row.GetScopeRoot(), "scope_root")
	c.Eq("c_src", row.GetChildId(), "child_id")
	c.Eq("n1", row.GetNodeId(), "node_id")
	c.Eq(int32(7), row.GetOrdinal(), "ordinal")
	c.Eq(true, row.GetCreatorIsUser(), "an empty creator child is a user credential")
	c.Eq("before the rewrite", row.GetNote(), "note")
	c.Require().NotNil(row.GetCreatedAt(), "created_at")
	c.Eq(created, row.GetCreatedAt().AsTime(), "created_at")
	c.Require().NotNil(row.GetSupersededAt(), "superseded_at")
	c.Nil(row.GetDeletedAt(), "a superseded tag is not deleted")

	// A create's unspecified scope is ScopeUnset, never lineage: only the
	// store defaults it.
	_, err = s.Tag(context.Background(), connect.NewRequest(&rafikiv1.TagRequest{Name: "t", ChildId: "c_src"}))
	c.NoError(err, "Tag with no scope")
	c.Eq(tags.ScopeUnset, m.gotInput.Scope, "TAG_SCOPE_UNSPECIFIED is ScopeUnset")
}

// TestListTagsLimitIsResolved pins the limit rule: 0 is the default 50,
// anything over 500 is clamped, and a negative limit is the caller's mistake.
func TestListTagsLimitIsResolved(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	for _, tc := range []struct {
		wire int32
		want int
	}{
		{0, 50},
		{1, 1},
		{500, 500},
		{501, 500},
		{5000, 500},
	} {
		m := &fakeTagManager{}
		s := connectapi.NewServer(nil)
		s.SetTagManager(m)
		_, err := s.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{NamePrefix: "fix", Limit: tc.wire}))
		c.NoError(err, "ListTags limit %d", tc.wire)
		c.Eq(tc.want, m.gotLimit, "ListTags limit %d reached the manager as %d", tc.wire, m.gotLimit)
		c.Eq("fix", m.gotPrefix, "name_prefix reached the manager")
	}

	m := &fakeTagManager{}
	s := connectapi.NewServer(nil)
	s.SetTagManager(m)
	_, err := s.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{Limit: -1}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "ListTags limit -1 = %v, want InvalidArgument", err)
	c.Eq(0, len(m.calls), "a negative limit reached the store: %v", m.calls)
}
