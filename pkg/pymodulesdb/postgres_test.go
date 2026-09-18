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
