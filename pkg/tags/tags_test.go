// SPDX-License-Identifier: Apache-2.0

package tags_test

import (
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/tags"
)

func TestScopeString(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("", tags.ScopeUnset.String(), "unset has no stored spelling")
	c.Eq("lineage", tags.ScopeLineage.String(), "lineage")
	c.Eq("tree", tags.ScopeTree.String(), "tree")
	c.Eq("owner", tags.ScopeOwner.String(), "owner")
}

func TestParseScope(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		in   string
		want tags.Scope
	}{
		{"lineage", tags.ScopeLineage},
		{"tree", tags.ScopeTree},
		{"owner", tags.ScopeOwner},
	} {
		got, err := tags.ParseScope(tc.in)
		c.NoError(err, "ParseScope(%q)", tc.in)
		c.Eq(tc.want, got, "ParseScope(%q)", tc.in)
	}
	for _, in := range []string{"", "Lineage", "TREE", " lineage", "lineage ", "bogus", "tree\n"} {
		got, err := tags.ParseScope(in)
		c.Error(err, "ParseScope(%q) must refuse a name that is not exactly a scope", in)
		c.Eq(tags.ScopeUnset, got, "a refused parse answers ScopeUnset")
	}
}

func TestPositionTopLevel(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("root", tags.Position{ChildID: "leaf", Ancestors: []string{"mid", "root"}}.TopLevel(), "the last ancestor is the top level")
	c.Eq("solo", tags.Position{ChildID: "solo"}.TopLevel(), "a child with no ancestors is its own top level")
	c.Eq("", tags.Position{}.TopLevel(), "a user credential has no top level")
}

func TestValidName(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, name := range []string{"a", "abc", "a1", "fix-bug_2.1", "9", strings.Repeat("a", tags.MaxNameLen)} {
		c.NoError(tags.ValidName(name), "ValidName(%q) must accept", name)
	}
	for _, name := range []string{
		"",
		strings.Repeat("a", tags.MaxNameLen+1),
		"Upper",
		"-lead",
		"_lead",
		".lead",
		"has space",
		"has/slash",
		"has:colon",
		"ünïcode",
	} {
		err := tags.ValidName(name)
		c.Error(err, "ValidName(%q) must refuse", name)
	}
	err := tags.ValidName("Bad")
	c.EqualError(err, `tag name "Bad" must be 1-64 of [a-z0-9._-], starting with a letter or digit`, "the refusal message is caller-visible")
}

func TestInScopeLineage(t *testing.T) {
	c := assert.NewCollecting(t)
	tag := tags.Tag{Scope: tags.ScopeLineage, ScopeRoot: "root"}
	// rootAncestors is the lineage root's own ancestor chain.
	rootAncestors := []string{"grandparent"}

	c.True(tags.InScope(tag, tags.Position{ChildID: "root"}, nil), "the root itself is in scope")
	c.True(tags.InScope(tag, tags.Position{ChildID: "leaf", Ancestors: []string{"mid", "root"}}, nil), "a descendant of the root is in scope")
	c.True(tags.InScope(tag, tags.Position{ChildID: "grandparent"}, rootAncestors), "an ancestor of the root is in scope")
	c.False(tags.InScope(tag, tags.Position{ChildID: "cousin", Ancestors: []string{"aunt"}}, nil), "an unrelated child is out of scope")
	c.False(tags.InScope(tag, tags.Position{ChildID: "sibling"}, nil), "a sibling of the root is out of scope")
	c.False(tags.InScope(tag, tags.Position{ChildID: "grandparent"}, nil), "without the root's ancestor chain an ancestor is out of scope")
}

func TestInScopeTree(t *testing.T) {
	c := assert.NewCollecting(t)
	tag := tags.Tag{Scope: tags.ScopeTree, ScopeRoot: "top"}

	c.True(tags.InScope(tag, tags.Position{ChildID: "top"}, nil), "the tree root is in scope")
	c.True(tags.InScope(tag, tags.Position{ChildID: "leaf", Ancestors: []string{"mid", "top"}}, nil), "a deep member of the tree is in scope")
	c.False(tags.InScope(tag, tags.Position{ChildID: "leaf", Ancestors: []string{"mid", "other"}}, nil), "a member of another tree is out of scope")
	c.False(tags.InScope(tag, tags.Position{ChildID: "other"}, nil), "another tree's root is out of scope")
}

func TestInScopeOwner(t *testing.T) {
	c := assert.NewCollecting(t)
	tag := tags.Tag{Scope: tags.ScopeOwner, ScopeRoot: "whatever"}
	c.True(tags.InScope(tag, tags.Position{ChildID: "anyone", Ancestors: []string{"else"}}, nil), "owner scope is unfiltered by position; the store filters the owner")
	c.True(tags.InScope(tag, tags.Position{}, nil), "a user credential is in scope")
}

func TestInScopeUserCredential(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, scope := range []tags.Scope{tags.ScopeLineage, tags.ScopeTree, tags.ScopeOwner} {
		c.True(tags.InScope(tags.Tag{Scope: scope, ScopeRoot: "elsewhere"}, tags.Position{}, nil), "a user credential is in scope for scope %s", scope)
	}
}

func TestInScopeUnsetScope(t *testing.T) {
	c := assert.NewCollecting(t)
	c.False(tags.InScope(tags.Tag{Scope: tags.ScopeUnset, ScopeRoot: "root"}, tags.Position{ChildID: "root"}, nil), "an unset scope is never in scope for a child")
}

func TestCanModify(t *testing.T) {
	c := assert.NewCollecting(t)
	tag := tags.Tag{CreatorChildID: "creator"}
	creatorAncestors := []string{"parent", "top"}

	c.True(tags.CanModify(tag, tags.Position{}, nil), "a user credential may modify anything")
	c.True(tags.CanModify(tag, tags.Position{ChildID: "creator"}, nil), "the creating child may modify")
	c.True(tags.CanModify(tag, tags.Position{ChildID: "parent"}, creatorAncestors), "an ancestor of the creator may modify")
	c.True(tags.CanModify(tag, tags.Position{ChildID: "top"}, creatorAncestors), "a higher ancestor of the creator may modify")
	c.False(tags.CanModify(tag, tags.Position{ChildID: "parent"}, nil), "without the creator's ancestor chain an ancestor may not modify")
	c.False(tags.CanModify(tag, tags.Position{ChildID: "sibling"}, creatorAncestors), "a sibling of the creator may not modify")

	userTag := tags.Tag{}
	c.False(tags.CanModify(userTag, tags.Position{ChildID: "anyone"}, []string{"anyone"}), "a user-created tag is not modifiable by a child")
	c.True(tags.CanModify(userTag, tags.Position{}, nil), "a user-created tag is modifiable by a user")
}

func TestRankOrdersScopes(t *testing.T) {
	c := assert.NewCollecting(t)
	p := tags.Position{ChildID: "me", Ancestors: []string{"root"}}
	now := time.Now()
	got := tags.Rank([]tags.Tag{
		{TagID: "o", Scope: tags.ScopeOwner, ScopeRoot: "root", CreatedAt: now},
		{TagID: "t", Scope: tags.ScopeTree, ScopeRoot: "root", CreatedAt: now},
		{TagID: "l", Scope: tags.ScopeLineage, ScopeRoot: "me", CreatedAt: now},
	}, p)
	c.EqDeep([]string{"l", "t", "o"}, tagIDs(got), "lineage before tree before owner")
}

func TestRankLineageNearestFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	p := tags.Position{ChildID: "me", Ancestors: []string{"mid", "root"}}
	now := time.Now()
	got := tags.Rank([]tags.Tag{
		{TagID: "far", Scope: tags.ScopeLineage, ScopeRoot: "root", CreatedAt: now.Add(3 * time.Hour)},
		{TagID: "near", Scope: tags.ScopeLineage, ScopeRoot: "mid", CreatedAt: now},
		{TagID: "self", Scope: tags.ScopeLineage, ScopeRoot: "me", CreatedAt: now.Add(-time.Hour)},
	}, p)
	c.EqDeep([]string{"self", "near", "far"}, tagIDs(got), "the caller's own tag first, then ancestors nearest first, then the rest")
}

func TestRankAncestorOfRootNewestFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	// The caller is an ancestor of both roots, so neither has a distance.
	p := tags.Position{ChildID: "top"}
	now := time.Now()
	got := tags.Rank([]tags.Tag{
		{TagID: "old", Scope: tags.ScopeLineage, ScopeRoot: "child-a", CreatedAt: now},
		{TagID: "new", Scope: tags.ScopeLineage, ScopeRoot: "child-b", CreatedAt: now.Add(time.Hour)},
	}, p)
	c.EqDeep([]string{"new", "old"}, tagIDs(got), "among roots the caller is an ancestor of, the newest comes first")
}

func TestRankTies(t *testing.T) {
	c := assert.NewCollecting(t)
	p := tags.Position{ChildID: "me"}
	now := time.Now()
	got := tags.Rank([]tags.Tag{
		{TagID: "b", Scope: tags.ScopeTree, ScopeRoot: "me", CreatedAt: now},
		{TagID: "a", Scope: tags.ScopeTree, ScopeRoot: "me", CreatedAt: now},
		{TagID: "z", Scope: tags.ScopeTree, ScopeRoot: "me", CreatedAt: now.Add(time.Minute)},
	}, p)
	c.EqDeep([]string{"z", "a", "b"}, tagIDs(got), "newer first, then the smaller TagID")

	in := []tags.Tag{
		{TagID: "b", Scope: tags.ScopeTree, ScopeRoot: "me", CreatedAt: now},
		{TagID: "a", Scope: tags.ScopeTree, ScopeRoot: "me", CreatedAt: now},
	}
	tags.Rank(in, p)
	c.EqDeep([]string{"b", "a"}, tagIDs(in), "Rank must not reorder its input")
}

func tagIDs(ts []tags.Tag) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.TagID)
	}
	return out
}
