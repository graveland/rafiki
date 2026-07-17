// Package store owns the conversations schema: its migration chain and (in
// later phases) conversation/message/turn persistence. The chain baselines at
// scadmin's 0007–0009 state; Migrate adopts an existing scadmin-shaped schema
// by recording the baseline as applied without executing it, so scadmin
// databases join the chain without double-applying or forking history.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.up.sql
var migrationsFS embed.FS

const (
	// Schema-qualified so a caller's search_path can never split-brain the
	// chain across two migration tables.
	migrationsTable = "public.rafiki_schema_migrations"
	baselineVersion = 1
	// advisoryLockKey serializes concurrent Migrate calls (e.g. two servers
	// booting at once) on one arbitrary-but-fixed key.
	advisoryLockKey int64 = 0x7261_6669_6b69 // "rafiki"
)

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate brings the database to the head of the embedded migration chain.
//
// Baseline adoption: when the rafiki migrations table does not exist yet, the
// conversations schema is probed. A complete scadmin-shaped schema (0007–0009
// applied: all three tables plus the provenance and prefix_hash columns) is
// adopted — the baseline version is recorded as applied without executing it.
// An absent schema runs the chain from the start. Anything in between is a
// hard error: a partial schema cannot be classified safely, so a human must
// reconcile it rather than Migrate guessing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	// Connection disposal is owned by the unlock defer below (Release on clean
	// unlock, session kill on unlock failure) — no separate Release defer.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		conn.Release()
		return fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer func() {
		bg := context.WithoutCancel(ctx)
		if _, err := conn.Exec(bg, `SELECT pg_advisory_unlock($1)`, advisoryLockKey); err != nil {
			// A connection returned to the pool while its session still holds
			// the advisory lock would block every future Migrate forever. Kill
			// the session instead: closing it releases the lock server-side.
			_ = conn.Hijack().Close(bg)
			return
		}
		conn.Release()
	}()

	hasTable, err := migrationsTableExists(ctx, conn)
	if err != nil {
		return err
	}
	if !hasTable {
		if err := initChain(ctx, conn, migrations[0]); err != nil {
			return err
		}
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := apply(ctx, conn, m); err != nil {
			return err
		}
	}
	return nil
}

// initChain creates the migrations table and, when the conversations schema
// already exists in full scadmin shape, records the baseline as adopted
// (applied without execution). Table creation and the adopted-baseline row are
// one transaction: a crash between them would otherwise leave an empty
// migrations table over an adopted schema, and the next run would execute the
// baseline instead of adopting.
func initChain(ctx context.Context, conn *pgxpool.Conn, baseline migration) error {
	if baseline.version != baselineVersion {
		return fmt.Errorf("migrate: first migration is %04d_%s, want baseline %04d", baseline.version, baseline.name, baselineVersion)
	}
	shape, err := probeConversationsSchema(ctx, conn)
	if err != nil {
		return err
	}
	if shape == schemaPartial {
		return errors.New("migrate: conversations schema exists but does not match the scadmin 0007-0009 baseline shape; " +
			"refusing to guess — reconcile the schema manually (complete or drop it), then re-run migrate")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: init chain: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+migrationsTable+` (
		version    INT PRIMARY KEY,
		name       TEXT NOT NULL,
		adopted    BOOLEAN NOT NULL DEFAULT false,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("migrate: create migrations table: %w", err)
	}
	if shape == schemaComplete {
		if _, err := tx.Exec(ctx,
			`INSERT INTO `+migrationsTable+` (version, name, adopted) VALUES ($1, $2, true)`,
			baseline.version, baseline.name); err != nil {
			return fmt.Errorf("migrate: record adopted baseline: %w", err)
		}
	}
	return tx.Commit(ctx)
}

type schemaShape int

const (
	schemaAbsent schemaShape = iota
	schemaComplete
	schemaPartial
)

// probeConversationsSchema classifies the pre-existing conversations schema
// against the scadmin 0007–0009 baseline: the three tables plus the 0008
// provenance columns and the 0009 prefix_hash column on conversation_turn.
func probeConversationsSchema(ctx context.Context, conn *pgxpool.Conn) (schemaShape, error) {
	var conv, turn, attach bool
	err := conn.QueryRow(ctx, `SELECT
		to_regclass('conversations.conversation') IS NOT NULL,
		to_regclass('conversations.conversation_turn') IS NOT NULL,
		to_regclass('conversations.conversation_attachment') IS NOT NULL`,
	).Scan(&conv, &turn, &attach)
	if err != nil {
		return schemaAbsent, fmt.Errorf("migrate: probe tables: %w", err)
	}
	if !conv && !turn && !attach {
		return schemaAbsent, nil
	}
	if !conv || !turn || !attach {
		return schemaPartial, nil
	}
	rows, err := conn.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema='conversations' AND table_name='conversation_turn'
		  AND column_name IN ('source','author','author_kind','prefix_hash')`)
	if err != nil {
		return schemaAbsent, fmt.Errorf("migrate: probe columns: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return schemaAbsent, fmt.Errorf("migrate: scan column: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return schemaAbsent, fmt.Errorf("migrate: probe columns: %w", err)
	}
	// required baseline columns on conversation_turn for an adoptable scadmin
	// shape. This is a presence check, not an exact-set check: a schema that
	// has since grown further columns (e.g. the 0005 turn-decomposition
	// columns, applied out-of-band by scadmin before rafiki's chain ever ran
	// against it) is still adoptable as long as these four are present.
	required := []string{"source", "author", "author_kind", "prefix_hash"}
	for _, c := range required {
		if !present[c] {
			return schemaPartial, nil
		}
	}
	return schemaComplete, nil
}

func migrationsTableExists(ctx context.Context, conn *pgxpool.Conn) (bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, migrationsTable).Scan(&exists); err != nil {
		return false, fmt.Errorf("migrate: probe migrations table: %w", err)
	}
	return exists, nil
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM `+migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("migrate: scan version: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// apply runs one migration and records it, atomically.
func apply(ctx context.Context, conn *pgxpool.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate %04d_%s: begin: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("migrate %04d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+migrationsTable+` (version, name) VALUES ($1, $2)`,
		m.version, m.name); err != nil {
		return fmt.Errorf("migrate %04d_%s: record: %w", m.version, m.name, err)
	}
	return tx.Commit(ctx)
}

// loadMigrations reads the embedded chain, sorted by version, and validates it
// is contiguous from the baseline.
func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".up.sql")
		if !ok {
			continue
		}
		numStr, rest, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migrate: bad migration filename %q (want NNNN_name.up.sql)", e.Name())
		}
		v, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("migrate: bad migration version in %q: %w", e.Name(), err)
		}
		sql, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: rest, sql: string(sql)})
	}
	if len(out) == 0 {
		return nil, errors.New("migrate: no embedded migrations")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != baselineVersion+i {
			return nil, fmt.Errorf("migrate: chain not contiguous at %04d_%s (want version %d)", m.version, m.name, baselineVersion+i)
		}
	}
	return out, nil
}
