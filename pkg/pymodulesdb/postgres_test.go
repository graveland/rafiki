// SPDX-License-Identifier: Apache-2.0

package pymodulesdb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/skillsdb/postgres_test.go's testStore, so this never touches a
// developer's real database.
func testStore(t *testing.T) (pymodules.Store, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_pymodules_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect scratch db")
	t.Cleanup(pool.Close)

	c.NoError(store.Migrate(ctx, pool), "migrate")
	return NewPostgresStore(pool), pool
}

// newOwner creates a real conversations.users row and returns its id:
// owner_user_id is a uuid FK to that table, so attribution must go through
// real users, exactly as production callers resolve it.
func newOwner(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(), users.NewUser{Username: username})
	assert.NewAborting(t).NoError(err, "create user %s", username)
	return u.ID
}

// Put is insert-only: versioning is the identity column, never an UPDATE.
// Two Puts under one (owner, name) must leave two distinct rows, and List
// must serve the newer one — the higher id.
func TestPutAlwaysInsertsNeverUpdates(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "user-a")

	first, err := st.Put(ctx, owner, "helpers", "code v1", "first version")
	c.NoError(err, "put first")
	second, err := st.Put(ctx, owner, "helpers", "code v2", "second version")
	c.NoError(err, "put second")
	c.NotEq(second.ID, first.ID, "second Put returned id")
	c.Greater(first.ID, second.ID, "ids not increasing: first")

	rows, err := st.List(ctx, owner)
	c.NoError(err, "list")
	c.Len(rows, 1, "got %d rows, want 1 (the latest of the two versions)", len(rows))
	if rows[0].ID != second.ID || rows[0].Code != "code v2" {
		t.Fatalf("got %+v, want the higher id's row (id %d, code v2)", rows[0], second.ID)
	}
}

// List is the latest-per-name view: two names, each saved twice, collapse to
// exactly two rows, each the higher-id version of its name.
func TestListReturnsLatestPerNameForOwner(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(err, "list")
	c.Len(rows, 2, "got %d rows, want 2 (one per name)", len(rows))
	byName := map[string]pymodules.Record{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	c.Eq("a v2", byName["alpha"].Code, "alpha: got %+v, want the higher-id a v2 row", byName["alpha"])
	c.Eq("b v2", byName["beta"].Code, "beta: got %+v, want the higher-id b v2 row", byName["beta"])
}

// Owner scoping is absolute: owner is NULL or a user id, there is no global
// scope and no admin override anywhere on this path, so owner B's "shared"
// must never appear in owner A's List.
func TestListNeverReturnsAnotherOwnersRows(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "owner-a")
	ownerB := newOwner(t, pool, "owner-b")

	a, err := st.Put(ctx, ownerA, "shared", "code a", "owner a's copy")
	c.NoError(err, "put a")
	b, err := st.Put(ctx, ownerB, "shared", "code b", "owner b's copy")
	c.NoError(err, "put b")

	for owner, want := range map[string]pymodules.Record{ownerA: a, ownerB: b} {
		rows, err := st.List(ctx, owner)
		c.NoError(err, "list %s", owner)
		c.Len(rows, 1, "list %s: got %d rows, want exactly 1", owner, len(rows))
		if rows[0].ID != want.ID || rows[0].Code != want.Code || rows[0].OwnerUserID != owner {
			t.Fatalf("list %s: got %+v, want exactly own row id %d", owner, rows[0], want.ID)
		}
	}
}

// ownerUserID == "" is one shared unattributed bucket, NOT a wildcard and not
// its own empty-string identity: unattributed rows see each other, and an
// attributed row must stay invisible to List("").
func TestUnattributedRowsShareOneBucket(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "list unattributed")
	c.Require().Len(rows, 2, "got %d rows, want exactly the 2 unattributed ones", len(rows))
	codes := map[string]bool{}
	for _, r := range rows {
		c.Eq("", r.OwnerUserID, "unattributed List returned OwnerUserID")
		codes[r.Code] = true
	}
	c.Require().False(!codes["code one"] || !codes["code two"] || codes["code three"], "got %+v, want code one and code two, never code three", rows)
}

// Delete stamps deleted_at on every version of the name: the name leaves
// List, and a later Put appends a live row that brings the name back with
// the new code. History is kept the whole time.
func TestDeleteHidesNameAndReputRestores(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-reput")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, "helpers", "B", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	c.NoError(st.Delete(ctx, owner, "helpers"), "delete")
	rows, err := st.List(ctx, owner)
	c.NoError(err, "list after delete")
	c.Empty(rows, "after delete got %d rows, want 0", len(rows))

	if _, err := st.Put(ctx, owner, "helpers", "C", "v3"); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	rows, err = st.List(ctx, owner)
	c.NoError(err, "list after re-put")
	c.False(len(rows) != 1 || rows[0].Code != "C", "after re-put got %+v, want exactly one row with code C", rows)
}

// A delete stamps every version of the name, so List returns zero rows no
// matter how many live versions preceded it.
func TestDeleteDoesNotResurrectOlderVersions(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-noresurrect")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, "helpers", "B", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	c.NoError(st.Delete(ctx, owner, "helpers"), "delete")
	rows, err := st.List(ctx, owner)
	c.NoError(err, "list after delete")
	c.Empty(rows, "List after delete returned")
}

// A delete of a name with zero rows reports ErrNotFound and writes nothing:
// no stamped row for a module that never existed.
func TestDeleteNotFoundForUnknownName(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-unknown")

	err := st.Delete(ctx, owner, "ghost_mod")
	c.ErrorIs(err, pymodules.ErrNotFound, "Delete on unknown name")
	var n int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.pymodules
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(owner), "ghost_mod").Scan(&n), "count rows")
	c.Eq(0, n, "row count after failed delete")
}

// Deleting an already-deleted name reports ErrNotFound rather than
// restamping: Delete's UPDATE matches only live rows (deleted_at IS NULL),
// so the second delete writes nothing and the total row count stays at
// 2 -- both versions stamped in place, no third row.
func TestDeleteNotFoundWhenAlreadyDeleted(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "del-twice")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, "helpers", "B", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	c.NoError(st.Delete(ctx, owner, "helpers"), "first delete")
	err := st.Delete(ctx, owner, "helpers")
	c.ErrorIs(err, pymodules.ErrNotFound, "second delete")
	var n int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.pymodules
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(owner), "helpers").Scan(&n), "count rows")
	c.Eq(2, n, "row count")
}

// Delete is owner-scoped like every other operation: A deleting "shared"
// must not touch B's independent copy.
func TestDeleteIsOwnerScoped(t *testing.T) {
	c := assert.NewAborting(t)
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

	c.NoError(st.Delete(ctx, ownerA, "shared"), "delete a")
	aRows, err := st.List(ctx, ownerA)
	c.NoError(err, "list a")
	c.Empty(aRows, "a's list")
	bRows, err := st.List(ctx, ownerB)
	c.NoError(err, "list b")
	c.False(len(bRows) != 1 || bRows[0].Name != "shared" || bRows[0].Code != "code b", "b's list = %+v, want exactly b's shared copy with code b", bRows)
	// B's copy is independent: B's own delete succeeds too.
	c.NoError(st.Delete(ctx, ownerB, "shared"), "delete b")
}

// ownerUserID "" is the one shared unattributed bucket: Delete("") stamps
// only unattributed rows, and an attributed module is untouched.
func TestDeleteUnattributedBucket(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	attributed := newOwner(t, pool, "del-unattr")

	if _, err := st.Put(ctx, "", "mine", "code mine", "unattributed"); err != nil {
		t.Fatalf("put unattributed: %v", err)
	}
	if _, err := st.Put(ctx, attributed, "theirs", "code theirs", "attributed"); err != nil {
		t.Fatalf("put attributed: %v", err)
	}

	c.NoError(st.Delete(ctx, "", "mine"), "delete unattributed")
	rows, err := st.List(ctx, "")
	c.NoError(err, "list unattributed")
	c.Empty(rows, "unattributed list")
	attrRows, err := st.List(ctx, attributed)
	c.NoError(err, "list attributed")
	c.False(len(attrRows) != 1 || attrRows[0].Name != "theirs", "attributed list = %+v, want exactly theirs: the delete must not touch it", attrRows)
}

// Get is the single-name view of List's latest-per-name rule: two versions
// under one name collapse to the higher-id row.
func TestGetPymoduleLatestLiveRow(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "get-latest")

	if _, err := st.Put(ctx, owner, "helpers", "code v1", "first version"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	second, err := st.Put(ctx, owner, "helpers", "code v2", "second version")
	c.NoError(err, "put v2")

	r, err := st.Get(ctx, owner, "helpers")
	c.NoError(err, "get")
	c.False(r.ID != second.ID || r.Code != "code v2" || r.Name != "helpers" || r.OwnerUserID != owner, "get = %+v, want the higher-id row (id %d, code v2, owner %s)", r, second.ID, owner)
}

// A Get after a delete finds nothing -- the delete stamped every live
// version -- and must not resurrect an older version.
func TestGetPymoduleNotFoundAfterDelete(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "get-after-delete")

	if _, err := st.Put(ctx, owner, "helpers", "A", "v1"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if _, err := st.Put(ctx, owner, "helpers", "B", "v2"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	c.NoError(st.Delete(ctx, owner, "helpers"), "delete")
	_, err := st.Get(ctx, owner, "helpers")
	c.ErrorIs(err, pymodules.ErrNotFound, "get after delete")
}

// Get is owner-scoped with the same IS NOT DISTINCT FROM rule as every other
// operation: a name saved under one owner is invisible to the other owner.
func TestGetPymoduleOwnerScoped(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "get-owner-a")
	ownerB := newOwner(t, pool, "get-owner-b")

	if _, err := st.Put(ctx, ownerA, "shared", "code a", "a's copy"); err != nil {
		t.Fatalf("put a: %v", err)
	}

	_, err := st.Get(ctx, ownerB, "shared")
	c.ErrorIs(err, pymodules.ErrNotFound, "get by other owner")

	// The unattributed bucket is likewise isolated: an attributed caller must
	// not see unattributed rows, and vice versa.
	if _, err := st.Put(ctx, "", "shared", "code unattr", "unattributed"); err != nil {
		t.Fatalf("put unattributed: %v", err)
	}
	_, err = st.Get(ctx, ownerB, "shared")
	c.ErrorIs(err, pymodules.ErrNotFound, "get by attributed owner")
	r, err := st.Get(ctx, "", "shared")
	c.NoError(err, "get unattributed")
	c.Eq("code unattr", r.Code, "unattributed get = %+v, want code unattr", r)
}
