// SPDX-License-Identifier: Apache-2.0

package rail_test

import (
	"testing"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// tree is root -> kid -> grand, plus a sibling root, with the only unread
// events on the grandchild.
func foldTree() []rail.Node {
	return []rail.Node{
		{ChildID: "root", Depth: 0, Attention: 1},
		{ChildID: "kid", Depth: 1, Attention: 2},
		{ChildID: "grand", Depth: 2, Attention: 4},
		{ChildID: "other", Depth: 0, Attention: 8},
	}
}

func ids(ns []rail.Node) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.ChildID)
	}
	return out
}

func TestFoldHidesDescendantsAtAnyDepthAndSumsAttention(t *testing.T) {
	ck := assert.NewCollecting(t)
	got := rail.Fold(foldTree(), map[string]bool{"root": true})
	ck.EqDeep([]string{"root", "other"}, ids(got), "rows")
	ck.True(got[0].Folded, "root should be Folded")
	ck.Eq(7, got[0].Attention, "root badge = own + kid + grandkid")
	ck.Eq(8, got[1].Attention, "unrelated row untouched")
}

func TestFoldNestedCollapseCountsOnceAndKeepsInnerState(t *testing.T) {
	ck := assert.NewCollecting(t)
	got := rail.Fold(foldTree(), map[string]bool{"kid": true})
	ck.EqDeep([]string{"root", "kid", "other"}, ids(got), "rows")
	ck.Eq(1, got[0].Attention, "an open parent shows only its own count")
	ck.Eq(6, got[1].Attention, "folded kid = own + grandkid")

	both := rail.Fold(foldTree(), map[string]bool{"root": true, "kid": true})
	ck.Eq(7, both[0].Attention, "inner fold must not double count")

	reopened := rail.Fold(foldTree(), map[string]bool{"kid": true})
	ck.True(reopened[1].Folded, "kid keeps its own fold while root is open")
}

func TestFoldLeafAndEmptyAreUnchanged(t *testing.T) {
	ck := assert.NewCollecting(t)
	got := rail.Fold(foldTree(), map[string]bool{"grand": true, "other": true})
	ck.Eq(4, len(got), "rows")
	for _, n := range got {
		ck.False(n.Folded, "%s: a childless row never folds", n.ChildID)
	}
	ck.EqDeep(ids(foldTree()), ids(rail.Fold(foldTree(), nil)), "no folds")
}

func TestAncestorsNearestFirst(t *testing.T) {
	r := rail.New()
	r.Seed([]*rafikiv1.ChildSummary{
		summary("root", "r", "", "idle", 0),
		summary("kid", "k", "root", "idle", 0),
		summary("grand", "g", "kid", "idle", 0),
	})
	assert.NewCollecting(t).EqDeep([]string{"kid", "root"}, r.Ancestors("grand"), "ancestors")
}
