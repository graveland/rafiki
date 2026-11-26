// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"

	"github.com/multigres/testkit/assert"
)

// migrate0076ChildID returns a per-run unique child id. The scratch database is
// fresh, but the ids stay distinctive so a failure names the run it came from.
func migrate0076ChildID(tag string) string {
	return fmt.Sprintf("c_m76%s%d", tag, time.Now().UnixNano())
}

// insertMigrate0076Child writes a child row with a verbatim labels JSON value,
// so a test can seed shapes the normal marshal never produces.
func insertMigrate0076Child(t *testing.T, ctx context.Context, pool *pgxpool.Pool, childID, labelsJSON string) error {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, labels)
		VALUES ($1, 'fundi', 'idle', now(), $2::jsonb)`, childID, labelsJSON)
	return err
}

// TestMigrate0076RewritesLegacyKeys pins 0076's key rewrite: every stored
// fundi/<k> becomes rafiki/<k>, the rafiki/<k> value wins when both spellings
// are present (the fundi/ key is dropped, never merged), a row that already
// carries only rafiki/* is untouched, and unrelated keys are carried through
// verbatim.
func TestMigrate0076RewritesLegacyKeys(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(migrateTo(ctx, pool, 75), "migrate to 0075, one short of 0076")

	legacyOnly := migrate0076ChildID("legacy")
	both := migrate0076ChildID("both")
	current := migrate0076ChildID("current")

	c.Require().NoError(insertMigrate0076Child(t, ctx, pool, legacyOnly,
		`{"fundi/parent":"p1","fundi/root":"r1","fundi/kind":"fundi","env":"work"}`),
		"seed a row with only the pre-rename spellings")
	c.Require().NoError(insertMigrate0076Child(t, ctx, pool, both,
		`{"rafiki/parent":"new","fundi/parent":"old","rafiki/root":"NEWROOT","fundi/root":"OLDROOT","fundi/model":"m","env":"work"}`),
		"seed a row carrying BOTH spellings with different values")
	c.Require().NoError(insertMigrate0076Child(t, ctx, pool, current,
		`{"rafiki/parent":"p2","rafiki/root":"r2","env":"work"}`),
		"seed a row already canonical")

	c.Require().NoError(migrateTo(ctx, pool, 76), "migrate to 0076")

	labelsOf := func(childID string) map[string]string {
		t.Helper()
		var labels map[string]string
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT labels FROM conversations.child WHERE child_id = $1`, childID).Scan(&labels),
			"read labels of %s", childID)
		return labels
	}

	// A whole-map comparison, so a surviving fundi/ key is a failure rather
	// than something to remember to look for.
	c.EqDeep(map[string]string{
		"env": "work", "rafiki/kind": "fundi", "rafiki/parent": "p1", "rafiki/root": "r1",
	}, labelsOf(legacyOnly), "every fundi/<k> key becomes rafiki/<k>")
	c.EqDeep(map[string]string{
		"env": "work", "rafiki/model": "m", "rafiki/parent": "new", "rafiki/root": "NEWROOT",
	}, labelsOf(both), "the rafiki/ value wins and the fundi/ key is dropped")
	c.EqDeep(map[string]string{
		"env": "work", "rafiki/parent": "p2", "rafiki/root": "r2",
	}, labelsOf(current), "a canonical row is untouched")
}

// TestMigrate0076GeneratedColumns pins the two generated columns: each equals
// the childstore label reader over the same labels map, a top-level child has
// NULL in both, and an UPDATE of labels moves them (they are generated, not
// backfilled once).
func TestMigrate0076GeneratedColumns(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	childID := migrate0076ChildID("gen")
	topID := migrate0076ChildID("top")
	labels := map[string]string{
		childstore.LabelParent: "c_parent0076",
		childstore.LabelRoot:   "c_root0076",
		"env":                  "work",
	}
	c.Require().NoError(insertMigrate0076Child(t, ctx, pool, childID,
		`{"rafiki/parent":"c_parent0076","rafiki/root":"c_root0076","env":"work"}`),
		"seed a parented row")
	c.Require().NoError(insertMigrate0076Child(t, ctx, pool, topID, `{"env":"work"}`),
		"seed a top-level row")

	read := func(id string) (parent, root *string) {
		t.Helper()
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT parent_id, root_id FROM conversations.child WHERE child_id = $1`, id).
			Scan(&parent, &root), "read the generated columns of %s", id)
		return parent, root
	}

	wantParent, _ := childstore.ParentLabel(labels)
	wantRoot := childstore.RootLabel(labels)
	parent, root := read(childID)
	c.Require().True(parent != nil && root != nil, "a parented row must generate both columns")
	c.Eq(wantParent, *parent, "parent_id must be the childstore parent label")
	c.Eq(wantRoot, *root, "root_id must be the childstore root label")

	topParent, topRoot := read(topID)
	c.True(topParent == nil, "a top-level row generates a NULL parent_id; got %v", topParent)
	c.True(topRoot == nil, "a top-level row generates a NULL root_id; got %v", topRoot)

	// A later UPDATE of labels followed by a read of the columns: generated
	// columns are recomputed on write, so no second migration is needed.
	_, err := pool.Exec(ctx, `
		UPDATE conversations.child
		   SET labels = '{"rafiki/parent":"c_parent0076b","rafiki/root":"c_root0076b"}'::jsonb
		 WHERE child_id = $1`, childID)
	c.Require().NoError(err, "update labels")
	parent, root = read(childID)
	c.Require().True(parent != nil && root != nil, "both columns after the update")
	c.Eq("c_parent0076b", *parent, "parent_id follows the labels update")
	c.Eq("c_root0076b", *root, "root_id follows the labels update")
}

// TestMigrate0076CheckRefusesAmbiguousLabels pins the shape constraint: labels
// that are not a JSON object, and a parent/root key present with a non-string
// value, are refused at the write naming the constraint — so no reader has to
// decide what such a row's lineage is.
func TestMigrate0076CheckRefusesAmbiguousLabels(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	for _, tc := range []struct {
		name   string
		labels string
	}{
		{"labels is an array", `[]`},
		{"rafiki/parent is a number", `{"rafiki/parent": 1}`},
		{"rafiki/root is JSON null", `{"rafiki/root": null}`},
	} {
		childID := migrate0076ChildID("bad")
		err := insertMigrate0076Child(t, ctx, pool, childID, tc.labels)
		c.Require().Error(err, "%s: the insert must be refused", tc.name)
		var pgErr *pgconn.PgError
		c.Require().True(errors.As(err, &pgErr), "%s: a postgres error, got %v", tc.name, err)
		c.Eq("23514", pgErr.Code, "%s: a check violation", tc.name)
		c.Eq("child_labels_lineage_shape", pgErr.ConstraintName, "%s: the refusal must name the constraint", tc.name)
	}
}
