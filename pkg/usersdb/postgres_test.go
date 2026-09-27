// SPDX-License-Identifier: Apache-2.0

package usersdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/store/migrate_test.go's testPool rather than the DSN-and-
// DELETE-FROM pattern, so this never touches a developer's real database.
func testStore(t *testing.T) (users.Store, *pgxpool.Pool) {
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

	name := fmt.Sprintf("rafiki_users_%d", time.Now().UnixNano())
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

func TestCreateReturnsPlaintextOnceAndAuthenticates(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	c.False(u.ID == "" || u.Username != "brent", "create returned %+v", u)
	c.False(len(token) < 20 || token[:4] != "rfk_", "token %q does not look like rfk_<base64url>", token)

	// The plaintext is never stored.
	var stored string
	c.NoError(pool.QueryRow(ctx,
		`SELECT token_sha256 FROM conversations.users WHERE id=$1`, u.ID).Scan(&stored), "read row")
	c.NotEq(token, stored, "plaintext token was stored in token_sha256")
	c.Eq(users.HashToken(token), stored, "stored digest")

	id, err := s.Authenticate(ctx, token)
	c.NoError(err, "authenticate")
	c.False(id.UserID != u.ID || id.Username != "brent", "identity = %+v, want %s/brent", id, u.ID)
}

func TestAuthenticateUnknownTokenIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	_, err := s.Authenticate(ctx, "rfk_nope")
	assert.NewAborting(t).ErrorIs(err, users.ErrNotFound, "err")
}

func TestDuplicateActiveUsernameIsRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, "brent", false); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, _, err := s.Create(ctx, "brent", false)
	assert.NewAborting(t).ErrorIs(err, users.ErrUsernameTaken, "err")
}

func TestDeleteTombstonesRevokesAndFreesTheName(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	c.NoError(s.Delete(ctx, "brent"), "delete")

	// The row survives — history keeps resolving to it.
	var deletedAt *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT deleted_at::text FROM conversations.users WHERE id=$1`, u.ID).Scan(&deletedAt), "row was hard-deleted")
	c.NotNil(deletedAt, "deleted_at is still NULL after Delete")

	// The token stops working immediately.
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}

	// And the name is reusable.
	if _, _, err := s.Create(ctx, "brent", false); err != nil {
		t.Fatalf("recreate after tombstone: %v", err)
	}
}

func TestCountActiveIgnoresTombstones(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	n, err := s.CountActive(ctx)
	c.NoError(err, "count")
	c.Eq(0, n, "CountActive on empty table")
	if _, _, err := s.Create(ctx, "brent", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "delete")
	n, err = s.CountActive(ctx)
	c.NoError(err, "count")
	c.Eq(0, n, "CountActive after tombstoning the only user")
}

func TestListExcludesTombstonesUnlessAsked(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, "alice", false); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, _, err := s.Create(ctx, "bob", false); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	c.NoError(s.Delete(ctx, "bob"), "delete bob")

	active, err := s.List(ctx, false, 100)
	c.NoError(err, "list")
	c.False(len(active) != 1 || active[0].Username != "alice", "active list = %+v, want [alice]", active)

	all, err := s.List(ctx, true, 100)
	c.NoError(err, "list all")
	c.Len(all, 2, "full list = %d rows, want 2", len(all))
}

// Tokens must never collide, and Create must not be the thing that notices.
func TestTokensAreDistinctAcrossUsers(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	seen := map[string]bool{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		_, tok, err := s.Create(ctx, name, false)
		c.NoError(err, "create %s", name)
		c.False(seen[tok], "duplicate token minted for %s", name)
		seen[tok] = true
	}
}

// A store that cannot reach the database has not learned the credential is
// invalid — it has learned nothing. Authenticate must not collapse "I could
// not check" into ErrNotFound, because a caller maps ErrNotFound to 401 and
// everything else to 503; the reference implementation this package was
// modelled on (executorsdb.pgStore.authenticateByHash) gets this wrong today,
// returning ErrNotFound for every error including a closed pool.
func TestAuthenticateOnClosedPoolIsNotErrNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	pool.Close()

	_, err := s.Authenticate(ctx, "rfk_whatever")
	c.Error(err, "expected an error against a closed pool, got nil")
	c.False(errors.Is(err, users.ErrNotFound), "closed-pool error must not be ErrNotFound (that means the credential is invalid, not that the check failed): %v", err)
}

// List's limit parameter must actually reach the query, not just be accepted
// and ignored.
func TestListRespectsLimit(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := s.Create(ctx, name, false); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	limited, err := s.List(ctx, false, 2)
	c.NoError(err, "list")
	c.Len(limited, 2, "List(limit=2) over 3 active users returned %d rows, want 2", len(limited))
}

func TestDeleteUnknownUsernameIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	assert.NewAborting(t).ErrorIs(s.Delete(ctx, "nobody"), users.ErrNotFound, "err")
}

func TestDeleteAlreadyTombstonedUsernameIsErrNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, "brent", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "first delete")
	c.ErrorIs(s.Delete(ctx, "brent"), users.ErrNotFound, "second delete on an already-tombstoned user: err")
}

// LookupUsername resolves ACTIVE rows only. A username is unique only among
// active users, so a lookup landing on a tombstone would attribute work to a
// deleted account — the whole reason this method exists beside List.
func TestLookupUsernameResolvesTheActiveRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	got, err := s.LookupUsername(ctx, "brent")
	c.NoError(err, "lookup")
	c.Eq(u.ID, got, "lookup")
}

func TestLookupUsernameMissesATombstone(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, "brent", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "delete")
	_, err := s.LookupUsername(ctx, "brent")
	c.ErrorIs(err, users.ErrNotFound, "lookup of a tombstoned name: err")
}

// One name, one active row plus any number of tombstones: the lookup must
// return the ACTIVE row, never the most recent row overall — created_at DESC
// over the whole table would hand back the tombstone here.
func TestLookupUsernameWithActiveAndTombstonesReturnsTheActiveRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	if _, _, err := s.Create(ctx, "brent", false); err != nil {
		t.Fatalf("first create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "delete")
	again, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "recreate")

	got, err := s.LookupUsername(ctx, "brent")
	c.NoError(err, "lookup")
	c.Eq(again.ID, got, "lookup")
}

func TestLookupUsernameUnknownIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	_, err := s.LookupUsername(ctx, "nobody")
	assert.NewAborting(t).ErrorIs(err, users.ErrNotFound, "err")
}

// Same rule as Authenticate: a store that cannot reach the database has not
// learned the name is absent.
func TestLookupUsernameOnClosedPoolIsNotErrNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	pool.Close()

	_, err := s.LookupUsername(ctx, "brent")
	c.Error(err, "expected an error against a closed pool, got nil")
	c.False(errors.Is(err, users.ErrNotFound), "closed-pool error must not be ErrNotFound (that means no such active user, not that the check failed): %v", err)
}

// The guard lives in the store so every caller gets it, not just the CLI.
func TestCreateNormalizesAndRejectsBadUsernames(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	// Padding is trimmed, not stored — otherwise "brent" and "brent " would be
	// two different people and the partial unique index would allow both.
	u, _, err := s.Create(ctx, "  brent\t", false)
	c.NoError(err, "create with padding")
	c.Eq("brent", u.Username, "username")
	if _, _, err := s.Create(ctx, "brent  ", false); !errors.Is(err, users.ErrUsernameTaken) {
		t.Fatalf("a padded duplicate was accepted (err = %v); trimming must happen BEFORE the uniqueness check", err)
	}

	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, _, err := s.Create(ctx, bad, false); !errors.Is(err, users.ErrInvalidUsername) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidUsername", bad, err)
		}
	}
}

// The admin bit must survive the full round trip: minted by Create, stored in
// the row, and read back by Authenticate — the ONLY path that ever populates
// Identity.IsAdmin. A plain user's identity must carry the bit false.
func TestCreateAdminSetsIsAdmin(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, token, err := s.Create(ctx, "root", true)
	c.NoError(err, "create admin")
	c.True(u.IsAdmin, "returned User = %+v, want IsAdmin true", u)

	id, err := s.Authenticate(ctx, token)
	c.NoError(err, "authenticate")
	c.False(id.UserID != u.ID || !id.IsAdmin, "identity = %+v, want IsAdmin true for %s", id, u.ID)

	// The ordinary user created in the same database stays non-admin, so the
	// assertion above cannot be passing on a default-true column.
	plain, plainToken, err := s.Create(ctx, "peon", false)
	c.NoError(err, "create plain user")
	c.False(plain.IsAdmin, "returned User = %+v, want IsAdmin false", plain)
	plainID, err := s.Authenticate(ctx, plainToken)
	c.NoError(err, "authenticate plain")
	c.False(plainID.IsAdmin, "plain identity = %+v, want IsAdmin false", plainID)
}
