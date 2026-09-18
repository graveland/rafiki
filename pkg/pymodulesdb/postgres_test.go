// SPDX-License-Identifier: Apache-2.0

package pymodulesdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/skillsdb/postgres_test.go's testStore, so this never touches a
// developer's real database.
func testStore(t *testing.T) (pymodules.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_pymodules_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewPostgresStore(pool), pool
}

// newOwner creates a real conversations.users row and returns its id:
// owner_user_id is a uuid FK to that table, so attribution must go through
// real users, exactly as production callers resolve it.
func newOwner(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(), username, false)
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u.ID
}

// Put is insert-only: versioning is the identity column, never an UPDATE.
// Two Puts under one (owner, name) must leave two distinct rows, and List
// must serve the newer one — the higher id.
func TestPutAlwaysInsertsNeverUpdates(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "user-a")

	first, err := st.Put(ctx, owner, "helpers", "code v1", "first version")
	if err != nil {
		t.Fatalf("put first: %v", err)
	}
	second, err := st.Put(ctx, owner, "helpers", "code v2", "second version")
	if err != nil {
		t.Fatalf("put second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("second Put returned id %d — an UPDATE happened, not an insert", first.ID)
	}
	if second.ID <= first.ID {
		t.Fatalf("ids not increasing: first %d, second %d", first.ID, second.ID)
	}

	rows, err := st.List(ctx, owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the latest of the two versions): %+v", len(rows), rows)
	}
	if rows[0].ID != second.ID || rows[0].Code != "code v2" {
		t.Fatalf("got %+v, want the higher id's row (id %d, code v2)", rows[0], second.ID)
	}
}

// List is the latest-per-name view: two names, each saved twice, collapse to
// exactly two rows, each the higher-id version of its name.
func TestListReturnsLatestPerNameForOwner(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "user-b")

	for _, p := range []struct{ name, code string }{
		{"alpha", "a v1"}, {"beta", "b v1"},
		{"alpha", "a v2"}, {"beta", "b v2"},
	} {
		if _, err := st.Put(ctx, owner, p.name, p.code, "d"); err != nil {
			t.Fatalf("put %s %s: %v", p.name, p.code, err)
		}
	}

	rows, err := st.List(ctx, owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one per name): %+v", len(rows), rows)
	}
	byName := map[string]pymodules.Record{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if byName["alpha"].Code != "a v2" {
		t.Fatalf("alpha: got %+v, want the higher-id a v2 row", byName["alpha"])
	}
	if byName["beta"].Code != "b v2" {
		t.Fatalf("beta: got %+v, want the higher-id b v2 row", byName["beta"])
	}
}

// Owner scoping is absolute: owner is NULL or a user id, there is no global
// scope and no admin override anywhere on this path, so owner B's "shared"
// must never appear in owner A's List.
func TestListNeverReturnsAnotherOwnersRows(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "owner-a")
	ownerB := newOwner(t, pool, "owner-b")

	a, err := st.Put(ctx, ownerA, "shared", "code a", "owner a's copy")
	if err != nil {
		t.Fatalf("put a: %v", err)
	}
	b, err := st.Put(ctx, ownerB, "shared", "code b", "owner b's copy")
	if err != nil {
		t.Fatalf("put b: %v", err)
	}

	for owner, want := range map[string]pymodules.Record{ownerA: a, ownerB: b} {
		rows, err := st.List(ctx, owner)
		if err != nil {
			t.Fatalf("list %s: %v", owner, err)
		}
		if len(rows) != 1 {
			t.Fatalf("list %s: got %d rows, want exactly 1: %+v", owner, len(rows), rows)
		}
		if rows[0].ID != want.ID || rows[0].Code != want.Code || rows[0].OwnerUserID != owner {
			t.Fatalf("list %s: got %+v, want exactly own row id %d", owner, rows[0], want.ID)
		}
	}
}

// ownerUserID == "" is one shared unattributed bucket, NOT a wildcard and not
// its own empty-string identity: unattributed rows see each other, and an
// attributed row must stay invisible to List("").
func TestUnattributedRowsShareOneBucket(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	attributed := newOwner(t, pool, "owner-c")

	if _, err := st.Put(ctx, "", "one", "code one", "unattributed"); err != nil {
		t.Fatalf("put one: %v", err)
	}
	if _, err := st.Put(ctx, "", "two", "code two", "unattributed"); err != nil {
		t.Fatalf("put two: %v", err)
	}
	if _, err := st.Put(ctx, attributed, "three", "code three", "attributed"); err != nil {
		t.Fatalf("put three: %v", err)
	}

	rows, err := st.List(ctx, "")
	if err != nil {
		t.Fatalf("list unattributed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want exactly the 2 unattributed ones: %+v", len(rows), rows)
	}
	codes := map[string]bool{}
	for _, r := range rows {
		if r.OwnerUserID != "" {
			t.Errorf("unattributed List returned OwnerUserID %q", r.OwnerUserID)
		}
		codes[r.Code] = true
	}
	if !codes["code one"] || !codes["code two"] || codes["code three"] {
		t.Fatalf("got %+v, want code one and code two, never code three", rows)
	}
}

// Delete appends a tombstone (an INSERT, deleted_at set): the name leaves
// List, and a later Put appends a live row that resurrects the name with the
// new code. History is kept the whole time.
func TestDeleteTombstonesAndReputRestores(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-reput")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, "helpers", "B", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if err := st.Delete(ctx, owner, "helpers"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err := st.List(ctx, owner)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("after delete got %d rows, want 0: %+v", len(rows), rows)
	}

	if _, err := st.Put(ctx, owner, "helpers", "C", "v3"); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	rows, err = st.List(ctx, owner)
	if err != nil {
		t.Fatalf("list after re-put: %v", err)
	}
	if len(rows) != 1 || rows[0].Code != "C" {
		t.Fatalf("after re-put got %+v, want exactly one row with code C", rows)
	}
}

// The tombstone must hide every version of a deleted name, not just the latest
// one: with the tombstone filter inside the DISTINCT ON subquery, List would
// resurface v1 (code "A") as the surviving latest-live row and this test would
// see one row instead of zero.
func TestDeleteDoesNotResurrectOlderVersions(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-noresurrect")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, "helpers", "B", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if err := st.Delete(ctx, owner, "helpers"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err := st.List(ctx, owner)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("List after delete returned %+v, want zero rows: the tombstone must hide older versions, not expose v1", rows)
	}
}

// A delete of a name with zero rows reports ErrNotFound and inserts nothing:
// no tombstone for a module that never existed.
func TestDeleteNotFoundForUnknownName(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-unknown")

	err := st.Delete(ctx, owner, "ghost_mod")
	if !errors.Is(err, pymodules.ErrNotFound) {
		t.Fatalf("Delete on unknown name = %v, want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.pymodules
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(owner), "ghost_mod").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("row count after failed delete = %d, want 0: nothing may be inserted", n)
	}
}

// Deleting an already-deleted name reports ErrNotFound rather than stacking a
// second tombstone: the probe reads the latest row overall, tombstones
// included, and the total row count stays at v1 + one tombstone.
func TestDeleteNotFoundWhenAlreadyDeleted(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-twice")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.Delete(ctx, owner, "helpers"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	err := st.Delete(ctx, owner, "helpers")
	if !errors.Is(err, pymodules.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.pymodules
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(owner), "helpers").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 2 {
		t.Fatalf("row count = %d, want 2 (v1 + tombstone): the second delete must not stack another tombstone", n)
	}
}

// Delete is owner-scoped like every other operation: A deleting "shared"
// must not touch B's independent copy.
func TestDeleteIsOwnerScoped(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "del-owner-a")
	ownerB := newOwner(t, pool, "del-owner-b")

	if _, err := st.Put(ctx, ownerA, "shared", "code a", "a's copy"); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, err := st.Put(ctx, ownerB, "shared", "code b", "b's copy"); err != nil {
		t.Fatalf("put b: %v", err)
	}

	if err := st.Delete(ctx, ownerA, "shared"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	aRows, err := st.List(ctx, ownerA)
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	if len(aRows) != 0 {
		t.Fatalf("a's list = %+v, want 0 rows after a's delete", aRows)
	}
	bRows, err := st.List(ctx, ownerB)
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(bRows) != 1 || bRows[0].Name != "shared" || bRows[0].Code != "code b" {
		t.Fatalf("b's list = %+v, want exactly b's shared copy with code b", bRows)
	}
	// B's copy is independent: B's own delete succeeds too.
	if err := st.Delete(ctx, ownerB, "shared"); err != nil {
		t.Fatalf("delete b: %v", err)
	}
}

// ownerUserID "" is the one shared unattributed bucket: Delete("") tombstones
// only an unattributed row, and an attributed module is untouched.
func TestDeleteUnattributedBucket(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	attributed := newOwner(t, pool, "del-unattr")

	if _, err := st.Put(ctx, "", "mine", "code mine", "unattributed"); err != nil {
		t.Fatalf("put unattributed: %v", err)
	}
	if _, err := st.Put(ctx, attributed, "theirs", "code theirs", "attributed"); err != nil {
		t.Fatalf("put attributed: %v", err)
	}

	if err := st.Delete(ctx, "", "mine"); err != nil {
		t.Fatalf("delete unattributed: %v", err)
	}
	rows, err := st.List(ctx, "")
	if err != nil {
		t.Fatalf("list unattributed: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unattributed list = %+v, want 0 rows after the delete", rows)
	}
	attrRows, err := st.List(ctx, attributed)
	if err != nil {
		t.Fatalf("list attributed: %v", err)
	}
	if len(attrRows) != 1 || attrRows[0].Name != "theirs" {
		t.Fatalf("attributed list = %+v, want exactly theirs: the delete must not touch it", attrRows)
	}
}
