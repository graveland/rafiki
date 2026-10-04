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

// TestDescendantsOfUsesLegacyLabels pins the fundi/ fallback: rows persisted
// before the rafiki rename still resolve.
func TestDescendantsOfUsesLegacyLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	legacy := childstore.LineageRow{
		Member: childstore.LineageMember{ChildID: "old"},
		Labels: map[string]string{"fundi/parent": "r", "fundi/root": "r"},
	}
	rows := []childstore.LineageRow{
		member("r", "", "", false),
		legacy,
	}
	c.ElementsMatch([]string{"r", "old"}, memberIDs(childstore.DescendantsOf(rows, "r")), "a fundi/-labelled descendant must still resolve")
}

// TestRootLabelPrecedence pins RootLabel: the current key wins, the legacy key
// is the fallback, and neither yields "".
func TestRootLabelPrecedence(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("new", childstore.RootLabel(map[string]string{"rafiki/root": "new", "fundi/root": "old"}), "rafiki/root must win")
	c.Eq("old", childstore.RootLabel(map[string]string{"fundi/root": "old"}), "legacy fallback")
	c.Eq("", childstore.RootLabel(map[string]string{"rafiki/parent": "p"}), "no root label")
	c.Eq("", childstore.RootLabel(nil), "nil labels")
}
