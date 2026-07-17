package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The migrator tests need a real TimescaleDB (>= 2.22, PostgreSQL 18 for
// uuidv7()). Set RAFIKI_TEST_DSN to run them, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable" go test ./store/...
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

func applyScadminChain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, files ...string) {
	t.Helper()
	for _, f := range files {
		sql, err := os.ReadFile("testdata/scadmin/" + f)
		if err != nil {
			t.Fatalf("read scadmin migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply scadmin migration %s: %v", f, err)
		}
	}
}

func assertBaselineSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	shape := probeShape(t, ctx, pool)
	if shape != schemaComplete {
		t.Fatalf("schema shape = %v, want complete", shape)
	}
}

func probeShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) schemaShape {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	shape, err := probeConversationsSchema(ctx, conn)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return shape
}

func baselineRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (name string, adopted bool) {
	t.Helper()
	err := pool.QueryRow(ctx,
		"SELECT name, adopted FROM "+migrationsTable+" WHERE version=$1", baselineVersion,
	).Scan(&name, &adopted)
	if err != nil {
		t.Fatalf("read baseline row: %v", err)
	}
	return name, adopted
}

func TestMigrateFreshDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (fresh): %v", err)
	}
	assertBaselineSchema(t, ctx, pool)
	if name, adopted := baselineRow(t, ctx, pool); adopted || name != "baseline" {
		t.Errorf("baseline row = (%q, adopted=%v), want (baseline, false): fresh DBs execute the baseline", name, adopted)
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

func TestMigrateAdoptsScadminSchema(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	applyScadminChain(t, ctx, pool,
		"0007_conversations.up.sql", "0008_turn_provenance.up.sql", "0009_turn_prefix_hash.up.sql")

	// Pre-existing data must survive adoption untouched.
	var convID string
	err := pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('pre-adopt','client') RETURNING id::text`).Scan(&convID)
	if err != nil {
		t.Fatalf("seed pre-adopt row: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (adopt): %v", err)
	}
	if name, adopted := baselineRow(t, ctx, pool); !adopted || name != "baseline" {
		t.Errorf("baseline row = (%q, adopted=%v), want (baseline, true): scadmin shape must be adopted, not executed", name, adopted)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&n); err != nil || n != 1 {
		t.Errorf("pre-adopt row: count=%d err=%v, want 1/nil", n, err)
	}

	// Re-run after adoption is a no-op.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate (re-run after adopt): %v", err)
	}
}

func TestMigrateRefusesPartialSchema(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// 0007 only: tables exist but the 0008/0009 columns are missing.
	applyScadminChain(t, ctx, pool, "0007_conversations.up.sql")

	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate must refuse a partial (0007-only) schema")
	}
	if !strings.Contains(err.Error(), "baseline shape") {
		t.Errorf("error should describe the shape mismatch, got: %v", err)
	}

	// It must not have half-initialized the chain: no migrations table.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, migrationsTable).Scan(&exists); err != nil {
		t.Fatalf("probe migrations table: %v", err)
	}
	if exists {
		t.Error("migrations table must not be created when the schema probe fails")
	}
}

func TestMigrateRefusesTablesWithoutColumns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// All three tables but only 0008 applied (prefix_hash missing) → partial.
	applyScadminChain(t, ctx, pool, "0007_conversations.up.sql", "0008_turn_provenance.up.sql")

	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("Migrate must refuse a 0007+0008-only schema (prefix_hash missing)")
	}
}

func TestMigrateRefusesMissingTables(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Some-but-not-all tables (the table-presence partial branch, as opposed
	// to the column-presence one the tests above exercise).
	if _, err := pool.Exec(ctx, `CREATE SCHEMA conversations;
		CREATE TABLE conversations.conversation (id UUID PRIMARY KEY DEFAULT uuidv7())`); err != nil {
		t.Fatalf("seed partial schema: %v", err)
	}

	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("Migrate must refuse a schema with only some baseline tables")
	}
	if shape := probeShape(t, ctx, pool); shape != schemaPartial {
		t.Fatalf("shape = %v, want partial", shape)
	}
}

// TestMigrateBaselineMatchesScadminChain proves adopt and execute converge:
// a database built by scadmin's 0007-0009 chain and then ADOPTED + migrated
// to head must be structurally identical (columns + indexes) to one migrated
// fresh by the full rafiki chain — otherwise the two histories diverge.
func TestMigrateBaselineMatchesScadminChain(t *testing.T) {
	ctx := context.Background()
	rafikiDB := testPool(t)
	scadminDB := testPool(t)

	if err := Migrate(ctx, rafikiDB); err != nil {
		t.Fatalf("Migrate (rafiki fresh): %v", err)
	}
	applyScadminChain(t, ctx, scadminDB,
		"0007_conversations.up.sql", "0008_turn_provenance.up.sql", "0009_turn_prefix_hash.up.sql")
	// Adopt the baseline, then bring the adopted DB to head.
	if err := Migrate(ctx, scadminDB); err != nil {
		t.Fatalf("Migrate (adopt + head): %v", err)
	}

	const columnsQ = `SELECT table_name, column_name, data_type, is_nullable, coalesce(column_default,'')
		FROM information_schema.columns WHERE table_schema='conversations'
		ORDER BY table_name, column_name`
	const indexesQ = `SELECT tablename, indexname, indexdef FROM pg_indexes
		WHERE schemaname='conversations' ORDER BY tablename, indexname`

	for name, q := range map[string]string{"columns": columnsQ, "indexes": indexesQ} {
		got := dumpRows(t, ctx, rafikiDB, q)
		want := dumpRows(t, ctx, scadminDB, q)
		if got != want {
			t.Errorf("%s differ between rafiki baseline and scadmin chain\nrafiki:\n%s\nscadmin:\n%s", name, got, want)
		}
	}
}

func dumpRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string) string {
	t.Helper()
	rows, err := pool.Query(ctx, q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		fmt.Fprintf(&b, "%v\n", vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return b.String()
}

// TestMigrateAdoptsSchemaWithDecompositionColumns proves the baseline probe is a
// presence check, not an exact-set check: a scadmin DB that has been further
// migrated through admindb 0013 (the decomposition columns) still has all
// four required provenance/prefix_hash columns, plus extras the probe doesn't
// know about — it must still be classified as an adoptable baseline.
func TestMigrateAdoptsSchemaWithDecompositionColumns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	applyScadminChain(t, ctx, pool,
		"0007_conversations.up.sql", "0008_turn_provenance.up.sql", "0009_turn_prefix_hash.up.sql")
	// Simulate admindb 0013: the decomposition columns, applied directly
	// (not via the rafiki chain) so the DB looks like a scadmin instance that
	// has moved past 0009 but has no rafiki_schema_migrations table yet.
	if _, err := pool.Exec(ctx, `ALTER TABLE conversations.conversation_turn
		ADD COLUMN IF NOT EXISTS response_ordinal INT,
		ADD COLUMN IF NOT EXISTS prefix_content   JSONB,
		ADD COLUMN IF NOT EXISTS cache_breakpoints JSONB;
		ALTER TABLE conversations.conversation_turn ALTER COLUMN request DROP NOT NULL`); err != nil {
		t.Fatalf("seed decomposition columns: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate must adopt a baseline-plus-decomposition schema, got: %v", err)
	}
	name, adopted := baselineRow(t, ctx, pool)
	if !adopted || name != "baseline" {
		t.Errorf("baseline row = (%q, adopted=%v), want (baseline, true): extra decomposition columns must not block adoption", name, adopted)
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
