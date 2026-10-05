package childstoredb

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// insertLineageChildRaw writes a child row with a verbatim labels JSON value,
// so a test can inject a shape the normal Marshal never produces (a non-object,
// a non-string walk value, an extra key).
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

// A returned row whose parent key is PRESENT but not a string leaves the chain
// through it undeterminable: Lineage must fail closed (a budget caller then
// skips rather than under-counting), naming the ambiguous row by child id.
func TestLineageErrorsOnNonStringParentLabel(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root, mid := p+"_root", p+"_mid"
	cleanupLineage(t, pool, root, mid)

	insertLineageChild(t, s, root, "", "", false)
	insertLineageChildRaw(t, pool, mid,
		fmt.Sprintf(`{"rafiki/root":%q,"rafiki/parent":5}`, root))

	_, err := s.Lineage(ctx, root)
	c.Require().Error(err, "a non-string parent on a returned row must be an error")
	c.StrContains(err.Error(), mid, "the error must name the ambiguous row: %v", err)
}

// A returned row whose labels value is not a JSON object at all is ambiguous by
// shape: Lineage must fail closed. The root's OWN row (selected by child_id) is
// the returned row here — a non-object value can never match the @> containment,
// so no descendant row can carry this shape.
func TestLineageErrorsOnNonObjectLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	p := lineagePrefix()
	root := p + "_root"
	cleanupLineage(t, pool, root)

	insertLineageChildRaw(t, pool, root, `"not-an-object"`)

	_, err := s.Lineage(ctx, root)
	c.Require().Error(err, "non-object labels on a returned row must be an error")
	c.StrContains(err.Error(), root, "the error must name the ambiguous row: %v", err)
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
