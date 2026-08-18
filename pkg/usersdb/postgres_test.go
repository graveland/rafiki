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
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/store/migrate_test.go's testPool rather than the DSN-and-
// DELETE-FROM pattern, so this never touches a developer's real database.
func testStore(t *testing.T) (users.Store, *pgxpool.Pool) {
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

	name := fmt.Sprintf("rafiki_users_%d", time.Now().UnixNano())
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

func TestCreateReturnsPlaintextOnceAndAuthenticates(t *testing.T) {
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, "brent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID == "" || u.Username != "brent" {
		t.Fatalf("create returned %+v", u)
	}
	if len(token) < 20 || token[:4] != "rfk_" {
		t.Fatalf("token %q does not look like rfk_<base64url>", token)
	}

	// The plaintext is never stored.
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT token_sha256 FROM conversations.users WHERE id=$1`, u.ID).Scan(&stored); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if stored == token {
		t.Fatal("plaintext token was stored in token_sha256")
	}
	if stored != users.HashToken(token) {
		t.Fatalf("stored digest %q != HashToken(token)", stored)
	}

	id, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id.UserID != u.ID || id.Username != "brent" {
		t.Fatalf("identity = %+v, want %s/brent", id, u.ID)
	}
}

func TestAuthenticateUnknownTokenIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if _, err := s.Authenticate(ctx, "rfk_nope"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDuplicateActiveUsernameIsRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, "brent"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, _, err := s.Create(ctx, "brent"); !errors.Is(err, users.ErrUsernameTaken) {
		t.Fatalf("err = %v, want ErrUsernameTaken", err)
	}
}

func TestDeleteTombstonesRevokesAndFreesTheName(t *testing.T) {
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, "brent")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(ctx, "brent"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The row survives — history keeps resolving to it.
	var deletedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at::text FROM conversations.users WHERE id=$1`, u.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("row was hard-deleted: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("deleted_at is still NULL after Delete")
	}

	// The token stops working immediately.
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}

	// And the name is reusable.
	if _, _, err := s.Create(ctx, "brent"); err != nil {
		t.Fatalf("recreate after tombstone: %v", err)
	}
}

func TestCountActiveIgnoresTombstones(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)

	n, err := s.CountActive(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountActive on empty table = %d, want 0", n)
	}
	if _, _, err := s.Create(ctx, "brent"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(ctx, "brent"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	n, err = s.CountActive(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountActive after tombstoning the only user = %d, want 0 (bootstrap mode)", n)
	}
}

func TestListExcludesTombstonesUnlessAsked(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, "alice"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, _, err := s.Create(ctx, "bob"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if err := s.Delete(ctx, "bob"); err != nil {
		t.Fatalf("delete bob: %v", err)
	}

	active, err := s.List(ctx, false, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 1 || active[0].Username != "alice" {
		t.Fatalf("active list = %+v, want [alice]", active)
	}

	all, err := s.List(ctx, true, 100)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("full list = %d rows, want 2", len(all))
	}
}

// Tokens must never collide, and Create must not be the thing that notices.
func TestTokensAreDistinctAcrossUsers(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	seen := map[string]bool{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		_, tok, err := s.Create(ctx, name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token minted for %s", name)
		}
		seen[tok] = true
	}
}
