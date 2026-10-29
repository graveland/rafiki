// SPDX-License-Identifier: Apache-2.0

package modelaliasdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/modelalias"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/presetsdb/postgres_test.go's testStore, so this never touches a
// developer's real database.
func testStore(t *testing.T) (modelalias.Store, *pgxpool.Pool) {
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

	name := fmt.Sprintf("rafiki_modelalias_%d", time.Now().UnixNano())
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

func TestPostgresPutGetListOrdering(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, pool := testStore(t)
	owner := newOwner(t, pool, "alias-order")

	for _, kv := range [][2]string{{"zeta", "openrouter/z"}, {"alpha", "openrouter/a"}, {"mid", "anthropic/m"}} {
		if _, err := st.Put(ctx, owner, kv[0], kv[1]); err != nil {
			t.Fatalf("put %s: %v", kv[0], err)
		}
	}
	got, err := st.Get(ctx, owner, "mid")
	c.NoError(err, "get")
	c.Eq("mid", got.Name, "name")
	c.Eq("anthropic/m", got.Target, "target")
	c.False(got.CreatedAt.IsZero() || got.UpdatedAt.IsZero(), "timestamps must be set: %+v", got)

	list, err := st.List(ctx, owner)
	c.NoError(err, "list")
	var names []string
	for _, a := range list {
		names = append(names, a.Name)
	}
	c.EqDeep([]string{"alpha", "mid", "zeta"}, names, "list must be ordered by name")

	empty, err := st.List(ctx, newOwner(t, pool, "alias-order-empty"))
	c.NoError(err, "list empty")
	c.NotNil(empty, "List must never return nil")
	c.Len(empty, 0, "list of an owner with no aliases")
}

// A replace updates the target in place: CreatedAt is preserved and UpdatedAt
// moves forward, and no second row appears.
func TestPostgresReplaceKeepsCreatedAt(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, pool := testStore(t)
	owner := newOwner(t, pool, "alias-replace")

	first, err := st.Put(ctx, owner, "ds", "openrouter/a")
	c.NoError(err, "first put")
	time.Sleep(2 * time.Millisecond)
	second, err := st.Put(ctx, owner, "ds", "openrouter/b")
	c.NoError(err, "second put")

	c.Eq("openrouter/b", second.Target, "replace must set the new target")
	c.True(second.CreatedAt.Equal(first.CreatedAt), "replace must keep CreatedAt: %v -> %v", first.CreatedAt, second.CreatedAt)
	c.True(second.UpdatedAt.After(first.UpdatedAt), "replace must move UpdatedAt forward: %v -> %v", first.UpdatedAt, second.UpdatedAt)

	var rows int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_alias`).Scan(&rows), "count rows")
	c.Eq(1, rows, "a replace must not add a row")
}

func TestPostgresDelete(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, pool := testStore(t)
	owner := newOwner(t, pool, "alias-delete")

	if _, err := st.Put(ctx, owner, "ds", "openrouter/a"); err != nil {
		t.Fatalf("put: %v", err)
	}
	c.NoError(st.Delete(ctx, owner, "ds"), "delete")
	_, err := st.Get(ctx, owner, "ds")
	c.ErrorIs(err, modelalias.ErrNotFound, "get after delete")
	list, err := st.List(ctx, owner)
	c.NoError(err, "list after delete")
	c.Len(list, 0, "list after delete")
	c.ErrorIs(st.Delete(ctx, owner, "ds"), modelalias.ErrNotFound, "delete of a missing alias")
}

// Owner scoping is absolute: owner A's alias is invisible to owner B and to
// the unattributed bucket, and each bucket can hold the same name independently.
func TestPostgresOwnerScoping(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, pool := testStore(t)
	ownerA := newOwner(t, pool, "alias-owner-a")
	ownerB := newOwner(t, pool, "alias-owner-b")

	targets := map[string]string{ownerA: "openrouter/a", ownerB: "openrouter/b", "": "openrouter/x"}
	for owner, target := range targets {
		if _, err := st.Put(ctx, owner, "ds", target); err != nil {
			t.Fatalf("put for owner %q: %v", owner, err)
		}
	}
	for owner, target := range targets {
		got, err := st.Get(ctx, owner, "ds")
		c.NoError(err, "get for owner %q", owner)
		c.Eq(target, got.Target, "owner %q must read its own target: another owner's row must not leak", owner)
		list, err := st.List(ctx, owner)
		c.NoError(err, "list for owner %q", owner)
		c.Len(list, 1, "owner %q must see exactly its own alias", owner)
	}
	c.NoError(st.Delete(ctx, ownerA, "ds"), "delete owner-a's")
	if _, err := st.Get(ctx, ownerB, "ds"); err != nil {
		t.Fatalf("deleting owner-a's alias must not touch owner-b's: %v", err)
	}
	if _, err := st.Get(ctx, ownerA, "ds"); !errors.Is(err, modelalias.ErrNotFound) {
		t.Fatalf("owner-a's alias survived its own delete: %v", err)
	}
}

// The unattributed bucket is ONE bucket: two puts with owner "" reach the same
// row rather than being stored as distinct empty-string owners.
func TestPostgresNullOwnerIsOneBucket(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, pool := testStore(t)

	if _, err := st.Put(ctx, "", "ds", "openrouter/a"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if _, err := st.Put(ctx, "", "ds", "openrouter/b"); err != nil {
		t.Fatalf("second put: %v", err)
	}
	var rows int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_alias`).Scan(&rows), "count rows")
	c.Eq(1, rows, "two puts in the unattributed bucket must hit one row")

	got, err := st.Get(ctx, "", "ds")
	c.NoError(err, "get")
	c.Eq("openrouter/b", got.Target, "the second put must replace the first")

	list, err := st.List(ctx, "")
	c.NoError(err, "list")
	c.Len(list, 1, "list of the unattributed bucket")
}

// Put validates before writing: an invalid name or target is refused and
// leaves nothing behind.
func TestPostgresPutRefusesInvalid(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, pool := testStore(t)
	owner := newOwner(t, pool, "alias-invalid")

	_, err := st.Put(ctx, owner, "Ds", "openrouter/a")
	c.ErrorIs(err, modelalias.ErrInvalid, "invalid name")
	_, err = st.Put(ctx, owner, "ds", "otheralias")
	c.ErrorIs(err, modelalias.ErrInvalid, "invalid target")
	list, err := st.List(ctx, owner)
	c.NoError(err, "list")
	c.Len(list, 0, "a refused put must write nothing")
}
