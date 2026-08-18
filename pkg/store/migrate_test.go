// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The migrator tests need a real TimescaleDB (>= 2.22, PostgreSQL 18 for
// uuidv7()). Set RAFIKI_TEST_DSN to run them, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable" go test ./pkg/store/...
//
// Each subtest runs in its own scratch database created from that DSN.

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_mig_%d", time.Now().UnixNano())
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
	return pool
}

// assertBaselineSchema checks the chain actually produced the conversations
// schema: the three tables the baseline creates, plus the provenance and
// prefix_hash columns later migrations depend on. Asserted directly against the
// catalog so a migration that records itself without doing its DDL is caught.
func assertBaselineSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"conversation", "conversation_turn", "conversation_attachment"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass('conversations.'||$1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("probe conversations.%s: %v", table, err)
		}
		if !exists {
			t.Errorf("conversations.%s missing after Migrate", table)
		}
	}
	for _, col := range []string{"source", "author", "author_kind", "prefix_hash"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema='conversations' AND table_name='conversation_turn'
			   AND column_name=$1)`, col).Scan(&exists); err != nil {
			t.Fatalf("probe conversation_turn.%s: %v", col, err)
		}
		if !exists {
			t.Errorf("conversation_turn.%s missing after Migrate", col)
		}
	}
}

func baselineName(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(ctx,
		"SELECT name FROM "+migrationsTable+" WHERE version=$1", baselineVersion,
	).Scan(&name); err != nil {
		t.Fatalf("read baseline row: %v", err)
	}
	return name
}

func TestMigrateFreshDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (fresh): %v", err)
	}
	assertBaselineSchema(t, ctx, pool)
	if name := baselineName(t, ctx, pool); name != "baseline" {
		t.Errorf("baseline row name = %q, want baseline", name)
	}

	// The executed schema must actually work: uuidv7 default, hypertable insert.
	var convID string
	err := pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO conversations.conversation_turn (conversation_id, ordinal, request, prefix_hash)
		VALUES ($1::uuid, 0, '{}'::jsonb, 'abc')`, convID)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	// Re-run is a no-op.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (re-run): %v", err)
	}
}

// TestMigrateConcurrent runs Migrate from two pools against the same fresh
// database: the advisory lock must serialize them, both must return nil, and
// the chain must be applied exactly once.
func TestMigrateConcurrent(t *testing.T) {
	pool1 := testPool(t)
	ctx := context.Background()

	// Second pool onto the SAME scratch database as pool1.
	cfg := pool1.Config().Copy()
	pool2, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(pool2.Close)

	errs := make(chan error, 2)
	for _, p := range []*pgxpool.Pool{pool1, pool2} {
		go func(p *pgxpool.Pool) { errs <- Migrate(ctx, p) }(p)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}

	chain, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var n, distinct int
	if err := pool1.QueryRow(ctx, `SELECT count(*), count(DISTINCT version) FROM `+migrationsTable).Scan(&n, &distinct); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != len(chain) || distinct != len(chain) {
		t.Fatalf("chain recorded %d rows / %d versions, want %d each (applied exactly once)", n, distinct, len(chain))
	}
	assertBaselineSchema(t, ctx, pool1)
}

func TestMigrate0018UsersTable(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The table exists with the columns the auth path reads.
	var cols int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema='conversations' AND table_name='users'
		   AND column_name IN ('id','username','token_sha256','created_at','deleted_at')`,
	).Scan(&cols); err != nil {
		t.Fatalf("probe users columns: %v", err)
	}
	if cols != 5 {
		t.Fatalf("users columns = %d, want 5", cols)
	}

	// The digest is globally unique: two users can never share a token.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ('a','dup'),('b','dup')`); err == nil {
		t.Fatal("duplicate token_sha256 was accepted; the UNIQUE constraint is missing")
	}

	// Usernames are unique among ACTIVE users only, so a name is reusable
	// after a tombstone. This is what makes `user rm` + `user create` a
	// working rotation story.
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256, deleted_at)
		 VALUES ('brent','h1', now())`); err != nil {
		t.Fatalf("insert tombstoned user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ('brent','h2')`); err != nil {
		t.Fatalf("reusing a tombstoned username must be allowed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.users (username, token_sha256) VALUES ('brent','h3')`); err == nil {
		t.Fatal("two ACTIVE users share a username; the partial unique index is missing")
	}
}
