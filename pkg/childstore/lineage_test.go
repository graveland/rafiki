package childstore_test

import (
	"fmt"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"

	"github.com/multigres/testkit/assert"
)

// member builds a LineageRow with the given parent/root labels.
func member(id, parent, root string, closed bool) childstore.LineageRow {
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	if root != "" {
		labels[childstore.LabelRoot] = root
	}
	return childstore.LineageRow{
		Member: childstore.LineageMember{ChildID: id, Closed: closed},
		Labels: labels,
	}
}

func memberIDs(ms []childstore.LineageMember) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ChildID)
	}
	return out
}

func rowIDs(rows []childstore.LineageRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Member.ChildID)
	}
	return out
}

// TestLineageRowsOfKeepsLabelsForADeeperWalk pins the row-shaped form of the
// walk: the labels survive, so ONE read of a tree answers for a subtree inside
// it (a list of script children under one root costs one read).
func TestLineageRowsOfKeepsLabelsForADeeperWalk(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []childstore.LineageRow{
		member("r", "", "", false),
		member("a", "r", "r", false),
		member("b", "a", "r", true),
		member("c", "r", "r", false),
	}

	kept := childstore.LineageRowsOf(rows, "r")
	c.ElementsMatch([]string{"r", "a", "b", "c"}, rowIDs(kept), "LineageRowsOf(r) = %v", rowIDs(kept))

	deeper := childstore.LineageRowsOf(kept, "a")
	c.ElementsMatch([]string{"a", "b"}, rowIDs(deeper), "the same rows answer a deeper subtree: LineageRowsOf(a) = %v", rowIDs(deeper))
	c.EqDeep(rows[1].Labels, deeper[0].Labels, "a row keeps the labels it was read with")

	// DescendantsOf is the same walk, projected.
	c.ElementsMatch(memberIDs(childstore.DescendantsOf(rows, "a")), rowIDs(deeper), "DescendantsOf must be LineageRowsOf's projection")
}

// TestDescendantsOfIncludesClosedAndNestedMembers pins the core semantics: the
// ancestor's own row is returned, every descendant at any depth is returned,
// and a closed descendant comes back flagged rather than filtered out. Fails
// if the ancestor's own row is skipped, if the walk stops at depth 1, or if
// Closed is never carried.
func TestDescendantsOfIncludesClosedAndNestedMembers(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []childstore.LineageRow{
		member("r", "", "", false),
		member("a", "r", "r", false),
		member("b", "a", "r", true), // a closed descendant
		member("c", "r", "r", false),
	}

	got := childstore.DescendantsOf(rows, "r")
	c.ElementsMatch([]string{"r", "a", "b", "c"}, memberIDs(got), "DescendantsOf(r) = %v", memberIDs(got))

	closed := map[string]bool{}
	for _, m := range got {
		closed[m.ChildID] = m.Closed
	}
	c.True(closed["b"], "the closed descendant must come back Closed=true")
	c.False(closed["a"], "a live descendant must not be marked closed")
	c.False(closed["r"], "the ancestor must not be marked closed")

	c.ElementsMatch([]string{"a", "b"}, memberIDs(childstore.DescendantsOf(rows, "a")), "DescendantsOf(a) = %v", memberIDs(childstore.DescendantsOf(rows, "a")))
}

// TestDescendantsOfExcludesOtherRoots pins that a row under the same root
// label but not on the ancestor's chain is excluded, and an unrelated root is
// absent entirely.
func TestDescendantsOfExcludesOtherRoots(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []childstore.LineageRow{
		member("r", "", "", false),
		member("a", "r", "r", false),
		member("orphan", "someoneelse", "r", false), // same root label, wrong chain
		member("z", "zroot", "zroot", false),        // different tree
	}

	got := memberIDs(childstore.DescendantsOf(rows, "r"))
	c.ElementsMatch([]string{"r", "a"}, got, "DescendantsOf(r) = %v; the orphan and other tree must be excluded", got)
}

// TestDescendantsOfBoundsACycleAndDepth pins the maxChainDepth bound: a cycle
// terminates (the ancestor appears as its own row plus the one node that
// reaches it), and a chain longer than the bound drops its over-deep tail.
func TestDescendantsOfBoundsACycleAndDepth(t *testing.T) {
	c := assert.NewCollecting(t)

	// 2-cycle: x's parent is y, y's parent is x.
	cycle := []childstore.LineageRow{member("x", "y", "x", false), member("y", "x", "x", false)}
	got := memberIDs(childstore.DescendantsOf(cycle, "x"))
	c.ElementsMatch([]string{"x", "y"}, got, "DescendantsOf(x) on a 2-cycle = %v", got)

	// A chain of 70 nodes c_00..c_69: c_k is k hops below c_00. Depth 64 is
	// the last reachable one; depth 65 is the dropped tail.
	var rows []childstore.LineageRow
	for i := 0; i < 70; i++ {
		id := fmt.Sprintf("c_%02d", i)
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("c_%02d", i-1)
		}
		rows = append(rows, member(id, parent, "c_00", false))
	}
	got = memberIDs(childstore.DescendantsOf(rows, "c_00"))
	c.Contains(got, "c_64", "the node at the walk bound must be included")
	c.NotContains(got, "c_65", "the over-deep tail must be excluded")
}

// TestDescendantsOfIgnoresAPreRenameParentKey pins that the walk reads ONE
// spelling: migration 0076 rewrites every stored fundi/parent to rafiki/parent,
// so a map carrying only the old key describes no parent at all rather than a
// second, tolerated one.
func TestDescendantsOfIgnoresAPreRenameParentKey(t *testing.T) {
	c := assert.NewCollecting(t)
	legacy := childstore.LineageRow{
		Member: childstore.LineageMember{ChildID: "old"},
		Labels: map[string]string{"fundi/parent": "r", "fundi/root": "r"},
	}
	rows := []childstore.LineageRow{
		member("r", "", "", false),
		legacy,
	}
	c.ElementsMatch([]string{"r"}, memberIDs(childstore.DescendantsOf(rows, "r")), "only the canonical key describes a parent")
}

// TestRootLabelReadsTheCanonicalKey pins RootLabel: it answers the one stored
// key and "" for anything else (a pre-rename map is the migration's subject).
func TestRootLabelReadsTheCanonicalKey(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("new", childstore.RootLabel(map[string]string{"rafiki/root": "new"}), "rafiki/root")
	c.Eq("new", childstore.RootLabel(map[string]string{"rafiki/root": "new", "fundi/root": "old"}), "the canonical key is the only one read")
	c.Eq("", childstore.RootLabel(map[string]string{"fundi/root": "old"}), "a pre-rename key is not a root")
	c.Eq("", childstore.RootLabel(map[string]string{"rafiki/parent": "p"}), "no root label")
	c.Eq("", childstore.RootLabel(nil), "nil labels")
}
