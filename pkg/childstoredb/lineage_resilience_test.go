package childstoredb

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// One malformed labels row must not take the whole tree down with it: it cannot
// be proven a descendant, so Lineage skips it, returns every other row, and
// reports no error. Without the skip, every budget sweep for this tree would
// fail on the same row forever.
//
// The malformed row is the ROOT's own row (selected by child_id = root, so it is
// returned even though a non-object value can never match the @> containment).
// Its labels are valid JSON but not a JSON object — a shape the normal Marshal
// can never produce, so it is inserted directly.
//
// Fails against the pre-change Lineage, which returned the unmarshal error for
// the whole call.
func TestLineageSkipsARowWithUnparseableLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, mid, leaf := p+"_root", p+"_mid", p+"_leaf"
	cleanupLineage(t, pool, root, mid, leaf)

	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels)
		VALUES ($1, 'fundi', 'idle', now(), '"not-an-object"'::jsonb)`, root)
	c.Require().NoError(err, "seed the malformed root row")

	insertLineageChild(t, s, mid, root, root, false)
	insertLineageChild(t, s, leaf, mid, root, true) // a closed descendant

	got, err := s.Lineage(ctx, mid)
	c.Require().NoError(err, "one malformed row must not fail the whole tree")
	c.ElementsMatch([]string{mid, leaf}, lineageIDs(got), "Lineage = %v", lineageIDs(got))
}

// A missing ancestor row with a caller-supplied root still resolves the
// subtree: the database cannot read an unpersisted ancestor's root, but the
// caller's live store can, so its closed descendants are not lost. Without a
// fallback the (nil, nil) live-set-only contract stands.
func TestLineageWithRootResolvesAMissingAncestorRow(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, mid, leaf := p+"_root", p+"_mid", p+"_leaf"
	cleanupLineage(t, pool, root, mid, leaf)

	insertLineageChild(t, s, root, "", "", false)
	// mid's OWN row is deliberately absent.
	insertLineageChild(t, s, leaf, mid, root, true) // closed descendant of the missing mid

	none, err := s.Lineage(ctx, mid)
	c.Require().NoError(err, "Lineage(missing ancestor)")
	c.Nil(none, "an ancestor with no row and no fallback must stay (nil, nil)")

	got, err := s.LineageWithRoot(ctx, mid, root)
	c.Require().NoError(err, "LineageWithRoot(missing ancestor, live root)")
	c.ElementsMatch([]string{leaf}, lineageIDs(got), "LineageWithRoot = %v", lineageIDs(got))
}

// The walk must cross a CLOSED middle row: a grandchild closed beneath a closed
// child stays in the root's lineage. This is the middle-row case the walk (and
// the spawn-fatal insert that guarantees the row exists) exists for.
func TestLineageWalksThroughAClosedMiddleRow(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, mid, leaf := p+"_root", p+"_mid", p+"_leaf"
	cleanupLineage(t, pool, root, mid, leaf)

	insertLineageChild(t, s, root, "", "", false)
	insertLineageChild(t, s, mid, root, root, true)
	insertLineageChild(t, s, leaf, mid, root, true)

	got, err := s.Lineage(ctx, root)
	c.Require().NoError(err, "Lineage(%s)", root)
	c.ElementsMatch([]string{root, mid, leaf}, lineageIDs(got),
		"a grandchild closed beneath a closed child must stay in the root's lineage; got %v", lineageIDs(got))
}
