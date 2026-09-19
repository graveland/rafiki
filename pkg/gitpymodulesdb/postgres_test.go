// SPDX-License-Identifier: Apache-2.0

package gitpymodulesdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/pymodulesdb/postgres_test.go's testStore, so this never
// touches a developer's real database.
func testStore(t *testing.T) (gitpymodules.Store, *pgxpool.Pool) {
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

	name := fmt.Sprintf("rafiki_gitpymodules_%d", time.Now().UnixNano())
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

// "local" is the blob store's sentinel across the whole pymodule tool
// surface; a git source registered under it would make repo="local"
// ambiguous. Put must reject it (and any name failing
// gitpymodules.ValidateName) before touching the database.
func TestPostgresStorePutRejectsReservedLocalName(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "git-reserved")

	_, err := st.Put(ctx, owner, "local", "https://example.com/ops.git", "main")
	if !errors.Is(err, gitpymodules.ErrReservedName) {
		t.Fatalf("Put(\"local\") = %v, want ErrReservedName", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.pymodule_git_sources`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("row count after rejected Put = %d, want 0: nothing may be written", n)
	}

	// Other invalid names are rejected too, with the ValidName verdict, not
	// the reserved-name sentinel.
	if _, err := st.Put(ctx, owner, "has space", "https://example.com/x.git", "main"); err == nil {
		t.Fatal("Put(\"has space\") = nil error, want a validation error")
	} else if errors.Is(err, gitpymodules.ErrReservedName) {
		t.Fatalf("Put(\"has space\") = %v, want the pymodules.ValidName verdict", err)
	}
}

// A git source registration is a pointer to repoint, not an append-only
// snippet history: two Puts under one (owner, name) must leave ONE row with
// the url/ref of the second Put — updated in place, same id. This must hold
// for the unattributed bucket too, which is exactly what the unique index's
// NULLS NOT DISTINCT is for (SQL's default NULLs-distinct would let two
// (NULL, name) rows coexist and the upsert would never fire).
func TestPostgresStorePutUpdatesExistingName(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "git-repoint")

	first, err := st.Put(ctx, owner, "ops_tools", "https://example.com/ops.git", "main")
	if err != nil {
		t.Fatalf("put first: %v", err)
	}
	second, err := st.Put(ctx, owner, "ops_tools", "https://example.com/ops-moved.git", "v2.1")
	if err != nil {
		t.Fatalf("put second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("ids differ (first %d, second %d): an UPDATE must have happened in place, not a new row", first.ID, second.ID)
	}
	rows, err := st.List(ctx, owner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != first.ID || rows[0].URL != "https://example.com/ops-moved.git" || rows[0].Ref != "v2.1" {
		t.Fatalf("rows = %+v, want exactly one ops_tools row repointed to ops-moved.git@v2.1 with id %d", rows, first.ID)
	}

	// Unattributed bucket: same rule with owner_user_id NULL.
	if _, err := st.Put(ctx, "", "shared_tools", "https://example.com/a.git", "main"); err != nil {
		t.Fatalf("put unattributed first: %v", err)
	}
	if _, err := st.Put(ctx, "", "shared_tools", "https://example.com/b.git", "dev"); err != nil {
		t.Fatalf("put unattributed second: %v", err)
	}
	rows, err = st.List(ctx, "")
	if err != nil {
		t.Fatalf("list unattributed: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "shared_tools" || rows[0].URL != "https://example.com/b.git" || rows[0].Ref != "dev" {
		t.Fatalf("unattributed rows = %+v, want exactly one shared_tools row repointed to b.git@dev (NULLS NOT DISTINCT)", rows)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.pymodule_git_sources
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(""), "shared_tools").Scan(&n); err != nil {
		t.Fatalf("count unattributed rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("unattributed row count = %d, want 1: no duplicate rows for one name", n)
	}
}

// Owner scoping is absolute, same as pymodules: no global scope, no admin
// override anywhere on this path, and "" is the one shared unattributed
// bucket — never a wildcard.
func TestPostgresStoreListScopesByOwner(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "git-owner-a")
	ownerB := newOwner(t, pool, "git-owner-b")

	a, err := st.Put(ctx, ownerA, "shared", "https://example.com/a.git", "main")
	if err != nil {
		t.Fatalf("put a: %v", err)
	}
	b, err := st.Put(ctx, ownerB, "shared", "https://example.com/b.git", "main")
	if err != nil {
		t.Fatalf("put b: %v", err)
	}
	if _, err := st.Put(ctx, "", "shared", "https://example.com/unattr.git", "main"); err != nil {
		t.Fatalf("put unattributed: %v", err)
	}

	for owner, want := range map[string]gitpymodules.GitSourceRecord{ownerA: a, ownerB: b} {
		rows, err := st.List(ctx, owner)
		if err != nil {
			t.Fatalf("list %s: %v", owner, err)
		}
		if len(rows) != 1 {
			t.Fatalf("list %s: got %d rows, want exactly 1: %+v", owner, len(rows), rows)
		}
		if rows[0].ID != want.ID || rows[0].URL != want.URL || rows[0].OwnerUserID != owner {
			t.Fatalf("list %s: got %+v, want exactly own row id %d", owner, rows[0], want.ID)
		}
	}

	rows, err := st.List(ctx, "")
	if err != nil {
		t.Fatalf("list unattributed: %v", err)
	}
	if len(rows) != 1 || rows[0].URL != "https://example.com/unattr.git" || rows[0].OwnerUserID != "" {
		t.Fatalf("unattributed list = %+v, want exactly the one unattributed row", rows)
	}
}

// Delete removes the row outright — there is no version history to
// soft-delete — is owner-scoped, and reports ErrNotFound (writing nothing)
// when the name does not exist for that owner.
func TestPostgresStoreDeleteRemovesAndReportsNotFound(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "git-del-a")
	ownerB := newOwner(t, pool, "git-del-b")

	if _, err := st.Put(ctx, ownerA, "shared", "https://example.com/a.git", "main"); err != nil {
		t.Fatalf("put a: %v", err)
	}
	b, err := st.Put(ctx, ownerB, "shared", "https://example.com/b.git", "main")
	if err != nil {
		t.Fatalf("put b: %v", err)
	}

	if err := st.Delete(ctx, ownerA, "shared"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if err := st.Delete(ctx, ownerA, "shared"); !errors.Is(err, gitpymodules.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if err := st.Delete(ctx, ownerB, "never_registered"); !errors.Is(err, gitpymodules.ErrNotFound) {
		t.Fatalf("delete unknown name = %v, want ErrNotFound", err)
	}
	// B's independent copy survives A's delete, with its own row intact.
	rows, err := st.List(ctx, ownerB)
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != b.ID || rows[0].URL != "https://example.com/b.git" {
		t.Fatalf("b's list = %+v, want exactly b's own row", rows)
	}
}
