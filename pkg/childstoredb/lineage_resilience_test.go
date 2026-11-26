package childstoredb

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// insertLineageChildRaw writes a child row with a verbatim labels JSON value,
// so a test can inject a shape the normal Marshal never produces (extra keys,
// including non-string ones the lineage walk never reads). A labels value that
// is not an object, or a non-string parent/root, cannot be written at all:
// migration 0076's child_labels_lineage_shape refuses it, which is pinned by
// TestMigrate0076CheckRefusesAmbiguousLabels (pkg/store).
func insertLineageChildRaw(t *testing.T, pool *pgxpool.Pool, id, labelsJSON string) {
	t.Helper()
	c := assert.NewAborting(t)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels)
		VALUES ($1, 'fundi', 'idle', now(), $2::jsonb)`, id, labelsJSON)
	c.NoError(err, "insert raw %s", id)
}

// A returned row may carry unrelated non-string labels (numbers, bools, nested
// objects) alongside the parent/root keys the walk reads. Those extra keys must
// be IGNORED: the row stays in the tree AND the walk crosses it, so the row's
// own child is not dropped from the rollup. Fails against the old skip, which
// dropped the row (and with it the whole sub-subtree beneath it).
func TestLineageKeepsARowWithExtraNonStringLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, mid, leaf := p+"_root", p+"_mid", p+"_leaf"
	cleanupLineage(t, pool, root, mid, leaf)

	insertLineageChild(t, s, root, "", "", false)
	insertLineageChildRaw(t, pool, mid,
		fmt.Sprintf(`{"rafiki/root":%q,"rafiki/parent":%q,"n":1}`, root, root))
	insertLineageChildRaw(t, pool, leaf,
		fmt.Sprintf(`{"rafiki/root":%q,"rafiki/parent":%q}`, root, mid))

	got, err := s.Lineage(ctx, root)
	c.Require().NoError(err, "Lineage(%s): an extra non-string label must not fail the call", root)
	c.ElementsMatch([]string{root, mid, leaf}, lineageIDs(got),
		"Lineage = %v; the extra-label row and its child must both stay", lineageIDs(got))
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
