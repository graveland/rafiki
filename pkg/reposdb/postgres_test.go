// SPDX-License-Identifier: Apache-2.0

package reposdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/repos"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/pymodulesdb/postgres_test.go's testStore, so this never
// touches a developer's real database.
func testStore(t *testing.T) (repos.Store, *pgxpool.Pool) {
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

	name := fmt.Sprintf("rafiki_repos_%d", time.Now().UnixNano())
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

// "local" is the blob store's sentinel across the whole pymodule tool
// surface; a repo registered under it would make repo="local"
// ambiguous. Put must reject it (and any name failing
// repos.ValidateName) before touching the database.
func TestPostgresStorePutRejectsReservedLocalName(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "git-reserved")

	_, err := st.Put(ctx, owner, "local", "https://example.com/ops.git", "main")
	c.ErrorIs(err, repos.ErrReservedName, "Put(\"local\") = %v, want ErrReservedName", err)
	var n int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.repos`).Scan(&n), "count rows")
	c.Eq(0, n, "row count after rejected Put")

	// Other invalid names are rejected too, with the ValidName verdict, not
	// the reserved-name sentinel.
	if _, err := st.Put(ctx, owner, "has space", "https://example.com/x.git", "main"); err == nil {
		t.Fatal("Put(\"has space\") = nil error, want a validation error")
	} else if errors.Is(err, repos.ErrReservedName) {
		t.Fatalf("Put(\"has space\") = %v, want the pymodules.ValidName verdict", err)
	}
}

// A repo registration is a pointer to repoint, not an append-only
// snippet history: two Puts under one (owner, name) must leave ONE row with
// the url/ref of the second Put — updated in place, same id. This must hold
// for the unattributed bucket too, which is exactly what the unique index's
// NULLS NOT DISTINCT is for (SQL's default NULLs-distinct would let two
// (NULL, name) rows coexist and the upsert would never fire).
func TestPostgresStorePutUpdatesExistingName(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "git-repoint")

	first, err := st.Put(ctx, owner, "ops_tools", "https://example.com/ops.git", "main")
	c.NoError(err, "put first")
	second, err := st.Put(ctx, owner, "ops_tools", "https://example.com/ops-moved.git", "v2.1")
	c.NoError(err, "put second")
	c.Eq(second.ID, first.ID, "ids differ (first")
	rows, err := st.List(ctx, owner)
	c.NoError(err, "list")
	c.False(len(rows) != 1 || rows[0].ID != first.ID || rows[0].URL != "https://example.com/ops-moved.git" || rows[0].Ref != "v2.1", "rows = %+v, want exactly one ops_tools row repointed to ops-moved.git@v2.1 with id %d", rows, first.ID)

	// Unattributed bucket: same rule with owner_user_id NULL.
	if _, err := st.Put(ctx, "", "shared_tools", "https://example.com/a.git", "main"); err != nil {
		t.Fatalf("put unattributed first: %v", err)
	}
	if _, err := st.Put(ctx, "", "shared_tools", "https://example.com/b.git", "dev"); err != nil {
		t.Fatalf("put unattributed second: %v", err)
	}
	rows, err = st.List(ctx, "")
	c.NoError(err, "list unattributed")
	c.False(len(rows) != 1 || rows[0].Name != "shared_tools" || rows[0].URL != "https://example.com/b.git" || rows[0].Ref != "dev", "unattributed rows = %+v, want exactly one shared_tools row repointed to b.git@dev (NULLS NOT DISTINCT)", rows)
	var n int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.repos
		 WHERE owner_user_id IS NOT DISTINCT FROM $1 AND name = $2`,
		ownerArg(""), "shared_tools").Scan(&n), "count unattributed rows")
	c.Eq(1, n, "unattributed row count")
}

// Owner scoping is absolute, same as pymodules: no global scope, no admin
// override anywhere on this path, and "" is the one shared unattributed
// bucket — never a wildcard.
func TestPostgresStoreListScopesByOwner(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "git-owner-a")
	ownerB := newOwner(t, pool, "git-owner-b")

	a, err := st.Put(ctx, ownerA, "shared", "https://example.com/a.git", "main")
	c.NoError(err, "put a")
	b, err := st.Put(ctx, ownerB, "shared", "https://example.com/b.git", "main")
	c.NoError(err, "put b")
	if _, err := st.Put(ctx, "", "shared", "https://example.com/unattr.git", "main"); err != nil {
		t.Fatalf("put unattributed: %v", err)
	}

	for owner, want := range map[string]repos.Record{ownerA: a, ownerB: b} {
		rows, err := st.List(ctx, owner)
		c.NoError(err, "list %s", owner)
		c.Len(rows, 1, "list %s: got %d rows, want exactly 1", owner, len(rows))
		if rows[0].ID != want.ID || rows[0].URL != want.URL || rows[0].OwnerUserID != owner {
			t.Fatalf("list %s: got %+v, want exactly own row id %d", owner, rows[0], want.ID)
		}
	}

	rows, err := st.List(ctx, "")
	c.NoError(err, "list unattributed")
	c.False(len(rows) != 1 || rows[0].URL != "https://example.com/unattr.git" || rows[0].OwnerUserID != "", "unattributed list = %+v, want exactly the one unattributed row", rows)
}

// Delete removes the row outright — there is no version history to
// soft-delete — is owner-scoped, and reports ErrNotFound (writing nothing)
// when the name does not exist for that owner.
func TestPostgresStoreDeleteRemovesAndReportsNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "git-del-a")
	ownerB := newOwner(t, pool, "git-del-b")

	if _, err := st.Put(ctx, ownerA, "shared", "https://example.com/a.git", "main"); err != nil {
		t.Fatalf("put a: %v", err)
	}
	b, err := st.Put(ctx, ownerB, "shared", "https://example.com/b.git", "main")
	c.NoError(err, "put b")

	c.NoError(st.Delete(ctx, ownerA, "shared"), "delete a")
	c.ErrorIs(st.Delete(ctx, ownerA, "shared"), repos.ErrNotFound, "second delete")
	c.ErrorIs(st.Delete(ctx, ownerB, "never_registered"), repos.ErrNotFound, "delete unknown name")
	// B's independent copy survives A's delete, with its own row intact.
	rows, err := st.List(ctx, ownerB)
	c.NoError(err, "list b")
	c.False(len(rows) != 1 || rows[0].ID != b.ID || rows[0].URL != "https://example.com/b.git", "b's list = %+v, want exactly b's own row", rows)
}

// TestPostgresStoreSetResolvedRecordsCommit pins the resolved state a refresh
// writes: the commit and the instant land on the row, they come back on the
// next read, and the write is owner-scoped — an unknown (owner, name) is
// ErrNotFound, writing nothing.
func TestPostgresStoreSetResolvedRecordsCommit(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "git-resolved-a")
	ownerB := newOwner(t, pool, "git-resolved-b")

	rec, err := st.Put(ctx, ownerA, "resolved_repo", "https://example.com/a.git", "main")
	c.NoError(err, "put")
	// A fresh registration knows nothing about a commit: empty, never-resolved,
	// on demand — the three zero values, not a guess.
	c.False(rec.Commit != "" || rec.RefreshInterval != 0 || !rec.ResolvedAt.IsZero(),
		"fresh row = commit %q interval %v resolvedAt %v, want the unresolved zero values", rec.Commit, rec.RefreshInterval, rec.ResolvedAt)

	const sha = "0123456789abcdef0123456789abcdef01234567"
	at := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
	c.NoError(st.SetResolved(ctx, ownerA, "resolved_repo", sha, at), "set resolved")

	rows, err := st.List(ctx, ownerA)
	c.NoError(err, "list")
	c.Require().Len(rows, 1, "list returned %d rows, want 1", len(rows))
	c.Eq(sha, rows[0].Commit, "stored commit")
	c.True(rows[0].ResolvedAt.Equal(at), "stored resolved_at = %v, want %v", rows[0].ResolvedAt, at)

	// Owner-scoped: b owns no row named resolved_repo, so the update finds
	// nothing and reports it rather than creating one.
	c.ErrorIs(st.SetResolved(ctx, ownerB, "resolved_repo", sha, at), repos.ErrNotFound, "SetResolved for another owner")
	rowsB, err := st.List(ctx, ownerB)
	c.NoError(err, "list b")
	c.Len(rowsB, 0, "b's rows after a refused SetResolved")
}

// TestPostgresStorePutLeavesResolvedStateAlone: a repoint changes url/ref and
// nothing else. The stored commit belongs to the ref the row is LEAVING, and
// must survive until the next refresh resolves the new one — clearing it here
// would make `repo list` claim the source was never resolved.
func TestPostgresStorePutLeavesResolvedStateAlone(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "git-repoint-state")

	if _, err := st.Put(ctx, owner, "kept_state", "https://example.com/a.git", "main"); err != nil {
		t.Fatalf("put: %v", err)
	}
	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	at := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	c.NoError(st.SetResolved(ctx, owner, "kept_state", sha, at), "set resolved")
	c.NoError(st.SetRefreshInterval(ctx, owner, "kept_state", 15*time.Minute), "set interval")

	// Repoint to a different ref.
	rec, err := st.Put(ctx, owner, "kept_state", "https://example.com/moved.git", "v2")
	c.NoError(err, "repoint")
	c.False(rec.URL != "https://example.com/moved.git" || rec.Ref != "v2", "repoint returned %+v, want the new url/ref", rec)
	c.False(rec.Commit != sha || !rec.ResolvedAt.Equal(at) || rec.RefreshInterval != 15*time.Minute,
		"repoint returned commit %q resolvedAt %v interval %v, want the previous resolved state kept", rec.Commit, rec.ResolvedAt, rec.RefreshInterval)

	rows, err := st.List(ctx, owner)
	c.NoError(err, "list")
	c.Require().Len(rows, 1, "list returned %d rows, want 1", len(rows))
	c.False(rows[0].Commit != sha || !rows[0].ResolvedAt.Equal(at) || rows[0].RefreshInterval != 15*time.Minute,
		"stored row = commit %q resolvedAt %v interval %v, want the previous resolved state kept", rows[0].Commit, rows[0].ResolvedAt, rows[0].RefreshInterval)
}

// TestPostgresStoreSetRefreshInterval pins the schedule column's three answers:
// a real interval is stored, zero stores NULL (on demand), and anything
// between zero and one minute is refused with the named sentinel rather than
// reaching the table's CHECK constraint.
func TestPostgresStoreSetRefreshInterval(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := newOwner(t, pool, "git-interval")

	if _, err := st.Put(ctx, owner, "scheduled", "https://example.com/a.git", "main"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// On demand is the default and stays NULL in the table.
	var nullCount int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.repos
		 WHERE name = $1 AND refresh_interval IS NULL`, "scheduled").Scan(&nullCount), "count NULL intervals")
	c.Eq(1, nullCount, "a fresh row's refresh_interval is not NULL")

	c.NoError(st.SetRefreshInterval(ctx, owner, "scheduled", 10*time.Minute), "set 10m")
	rows, err := st.List(ctx, owner)
	c.NoError(err, "list")
	c.Require().Len(rows, 1, "list returned %d rows, want 1", len(rows))
	c.Eq(10*time.Minute, rows[0].RefreshInterval, "stored interval")

	// Below the floor: refused, and the stored value is untouched.
	for _, bad := range []time.Duration{time.Second, 59 * time.Second, -time.Minute} {
		c.ErrorIs(st.SetRefreshInterval(ctx, owner, "scheduled", bad), repos.ErrIntervalTooShort,
			"SetRefreshInterval(%v) = %v, want ErrIntervalTooShort", bad)
	}
	c.ErrorIs(st.SetRefreshInterval(ctx, owner, "scheduled", 59*time.Second), repos.ErrIntervalTooShort, "59s")
	if err := st.SetRefreshInterval(ctx, owner, "scheduled", 59*time.Second); err == nil {
		t.Fatal("59s accepted")
	} else {
		c.Eq("refresh interval must be at least 1m", err.Error(), "short-interval error text")
	}
	rows, err = st.List(ctx, owner)
	c.NoError(err, "list after refusals")
	c.Eq(10*time.Minute, rows[0].RefreshInterval, "a refused interval changed the stored one")

	// Exactly one minute is the floor, so it is accepted.
	c.NoError(st.SetRefreshInterval(ctx, owner, "scheduled", time.Minute), "set 1m")
	// Zero clears it back to on demand (NULL), which List reports as zero.
	c.NoError(st.SetRefreshInterval(ctx, owner, "scheduled", 0), "clear interval")
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.repos
		 WHERE name = $1 AND refresh_interval IS NULL`, "scheduled").Scan(&nullCount), "count NULL intervals after clear")
	c.Eq(1, nullCount, "SetRefreshInterval(0) did not store NULL")
	rows, err = st.List(ctx, owner)
	c.NoError(err, "list after clear")
	c.Eq(time.Duration(0), rows[0].RefreshInterval, "cleared interval")

	c.ErrorIs(st.SetRefreshInterval(ctx, owner, "never_registered", time.Minute), repos.ErrNotFound, "interval for an unknown name")
}

// TestPostgresStoreListAllSpansOwners pins the scheduler's read: every owner's
// rows come back in one call, ordered by (owner, name) with the unattributed
// bucket first — unlike List, which is owner-scoped by construction.
func TestPostgresStoreListAllSpansOwners(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool, "git-all-a")
	ownerB := newOwner(t, pool, "git-all-b")

	for _, put := range []struct{ owner, name string }{
		{ownerB, "b_second"},
		{ownerA, "a_second"},
		{"", "anon_repo"},
		{ownerA, "a_first"},
	} {
		if _, err := st.Put(ctx, put.owner, put.name, "https://example.com/x.git", "main"); err != nil {
			t.Fatalf("put %s/%s: %v", put.owner, put.name, err)
		}
	}

	rows, err := st.ListAll(ctx)
	c.NoError(err, "list all")
	c.Require().Len(rows, 4, "ListAll returned %d rows, want 4", len(rows))
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.OwnerUserID+"/"+r.Name)
	}
	c.EqDeep([]string{
		"/anon_repo",        // the unattributed bucket sorts first
		ownerA + "/a_first", // then owner, then name
		ownerA + "/a_second",
		ownerB + "/b_second",
	}, got, "ListAll order")
}
