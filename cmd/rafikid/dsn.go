// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.graveland.dev/rafiki/pkg/embedpg"
	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
)

// embeddedDSN is the RAFIKI_DB value that means "the rafikid-managed PostgreSQL
// under this user's data dir" (pkg/embedpg). It is installstate's database mode
// value, not a second literal: `rafiki setup` writes that mode into service.env
// as RAFIKI_DB, so the two binaries must spell it the same way.
const embeddedDSN = string(installstate.DBModeEmbedded)

// Keep public first: migrations create extensions without an explicit schema.
// psql also supplies this default for databases whose roles we do not manage.
const rafikiSearchPath = "public,conversations"

// newEmbeddedServer builds the private server Default describes. It is a
// variable so tests can point the resolver at a scratch data dir and a fixture
// download instead of the real per-user install.
var newEmbeddedServer = embedpg.Default

// isEmbedded reports whether a configured DSN selects the private server.
//
// RAFIKI_DB=embedded is interpreted HERE, in resolveDSN and in the two accessors
// below — nowhere else. The db verbs ask this instead of comparing the sentinel
// themselves, so "which database is embedded" and "what does embedded mean" stay
// one answer: pkg/embedpg names the server, this file decides when it is in use.
func isEmbedded(configured string) bool {
	return configured == embeddedDSN
}

// embeddedSettingHint spells the setting that selects the private server, from
// the sentinel itself, so a message telling an operator how to get back to it
// cannot drift from the value that does it.
const embeddedSettingHint = "RAFIKI_DB=" + embeddedDSN

// embeddedServer is the private server the embedded DSN names, exactly as
// configured: it is NOT made ready. Server.Ensure is what starts one (and
// downloads, and initdbs), so a caller that must not change whether the install
// is up — a physical backup — needs the struct and nothing more.
func embeddedServer() embedpg.Server {
	return newEmbeddedServer()
}

// embeddedServerMajor is the PostgreSQL major the private server's binaries
// carry, known without starting anything and without reading a data directory.
func embeddedServerMajor() (int, error) {
	major, err := strconv.Atoi(embedpg.MajorVersion)
	if err != nil {
		return 0, fmt.Errorf("embedded database: cannot read the server's major version %q: %w",
			embedpg.MajorVersion, err)
	}
	return major, nil
}

// resolveDSN turns a configured DSN into one pgx can dial. "embedded" starts
// the private server if it is not running (downloading its binaries on first
// use, progress on stderr), creates its database if it is absent, and returns
// its socket DSN; any other value is returned unchanged. It is the only place
// "embedded" is interpreted, and the only place an embedded database is
// created.
func resolveDSN(ctx context.Context, dsn string) (string, error) {
	if !isEmbedded(dsn) {
		return dsn, nil
	}
	_, resolved, err := resolveEmbedded(ctx)
	return resolved, err
}

// resolveEmbedded is the embedded half of resolveDSN: the server that answers to
// the embedded sentinel, made ready, and the DSN naming the database it serves.
// A caller that needs the Server itself (a physical backup, which must NOT make
// it ready) asks embeddedServer instead — this one starts it.
func resolveEmbedded(ctx context.Context) (embedpg.Server, string, error) {
	s := embeddedServer()
	s.Progress = os.Stderr
	// Ensure starts the server; it does not create Database, because
	// pkg/embedpg opens no connection and a CREATE DATABASE is one. A fresh
	// cluster has only the maintenance database.
	if _, err := s.Ensure(ctx); err != nil {
		return embedpg.Server{}, "", fmt.Errorf("embedded database: %w", err)
	}
	if err := ensureEmbeddedDatabase(ctx, s); err != nil {
		return embedpg.Server{}, "", err
	}
	return s, s.DSN(), nil
}

// createEmbeddedDatabase makes the database a resolved embedded DSN names. It
// is a variable so a test can stand in for the server: connecting to one takes
// a real cluster, while what resolveDSN must get right — the statement, the
// maintenance DSN it is issued on, and the SQLSTATE it tolerates — does not.
var createEmbeddedDatabase = createEmbeddedDatabaseFor

// createEmbeddedDatabaseFor ensures the database and its role defaults
// over a short-lived connection to the MAINTENANCE database: initdb creates only
// that one, so it is the only database a fresh cluster can be asked from (and
// the database being created cannot be the one the CREATE is issued on).
//
// The connection is closed on a bounded, detached context, so a caller whose own
// context was cancelled — or a wedged server — cannot hold this open on a socket
// write.
func createEmbeddedDatabaseFor(ctx context.Context, s embedpg.Server) error {
	conn, err := pgx.Connect(ctx, s.MaintenanceDSN())
	if err != nil {
		return fmt.Errorf("connecting to the maintenance database: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), databaseCloseTimeout)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	return provisionEmbeddedDatabase(ctx, conn)
}

// provisionEmbeddedDatabase repairs existing installs as well as fresh ones.
// A concurrent CREATE's duplicate is success here, BEFORE applying defaults;
// an error configuring defaults must never be mistaken for that CREATE race.
func provisionEmbeddedDatabase(ctx context.Context, c dbCatalog) error {
	if err := createDatabaseUnlessPresent(ctx, c); err != nil && !databaseAlreadyExists(err) {
		return err
	}
	// These are separate statements: CREATE DATABASE cannot run in a transaction.
	// The DO block makes the advisory lock transaction-scoped, serializing role
	// catalog updates from concurrent resolvers without leaving a session lock.
	_, err := c.Exec(ctx, embeddedDatabaseDefaultsStatement())
	if err != nil {
		return fmt.Errorf("configuring embedded database role defaults: %w", err)
	}
	return nil
}

func embeddedDatabaseDefaultsStatement() string {
	return fmt.Sprintf(`DO $rafiki$
BEGIN
    PERFORM pg_advisory_xact_lock(7261666969);
    ALTER ROLE CURRENT_USER IN DATABASE %s SET search_path TO %s;
END;
$rafiki$;`, pgx.Identifier{embedpg.Database}.Sanitize(), rafikiSearchPath)
}

// databaseCloseTimeout bounds closing the maintenance connection.
const databaseCloseTimeout = 5 * time.Second

// dbCatalog is the slice of one connection this file's provisioner uses: the
// existence query, legacy-schema probe, CREATE and role defaults. Keeping these
// decisions behind it lets tests pin them without a cluster; only the dial
// itself needs a server.
type dbCatalog interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const (
	// databaseExistsSQL asks the catalog whether the database is there, so the
	// CREATE is issued only when it is not. Asking the CREATE instead means a
	// failed statement on every daemon start, `psql`, `user create` and
	// `migrate` — each one an ERROR line in pg.log that an operator debugging a
	// start has to read past.
	databaseExistsSQL = "SELECT 1 FROM pg_database WHERE datname = $1"

	// legacySchemaSQL asks whether the maintenance database already holds
	// rafiki's schema. That schema is the one pkg/store's migrations create; the
	// name is a literal because pkg/store exports none, and this file must not
	// depend on the store to ask a catalog question.
	legacySchemaSQL  = "SELECT 1 FROM pg_namespace WHERE nspname = '" + legacySchemaName + "'"
	legacySchemaName = "conversations"
)

// createDatabaseUnlessPresent creates the database embeddedDatabaseStatement
// names, and does nothing when it is already there. c must be connected to the
// maintenance database.
func createDatabaseUnlessPresent(ctx context.Context, c dbCatalog) error {
	var one int
	err := c.QueryRow(ctx, databaseExistsSQL, embedpg.Database).Scan(&one)
	switch {
	case err == nil:
		// It is there: nothing to create, and nothing to say about it.
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("checking whether the %s database exists: %w", embedpg.Database, err)
	}

	// The create path only: this is the one moment an install that already keeps
	// rafiki's data somewhere else can be noticed before it is handed an empty
	// database to migrate into.
	warnIfMaintenanceDatabaseHoldsRafikiData(ctx, c)

	_, err = c.Exec(ctx, embeddedDatabaseStatement())
	return err
}

// warnIfMaintenanceDatabaseHoldsRafikiData names the split an operator would
// otherwise discover as "all my history is gone": a data directory from before
// the embedded database had its own name keeps everything in `postgres`, and
// the daemon would migrate a fresh, empty schema into the new one. The databases
// are still on disk, so this is a warning and not a refusal.
//
// A failed probe is silent: not knowing is no reason to refuse to create a
// database the daemon needs to start.
func warnIfMaintenanceDatabaseHoldsRafikiData(ctx context.Context, c dbCatalog) {
	var one int
	if err := c.QueryRow(ctx, legacySchemaSQL).Scan(&one); err != nil {
		return
	}
	slog.Warn("the maintenance database already holds a rafiki schema; the embedded database is being created empty",
		"creating", embedpg.Database, "existing", embedpg.MaintenanceDatabase)
}

// embeddedDatabaseStatement is the CREATE DATABASE itself. The identifier is
// quoted rather than interpolated: CREATE DATABASE takes no bind parameters (a
// parameter is not an identifier), and an unquoted name would be case-folded,
// which is how the created database would stop being the one DSN() names.
func embeddedDatabaseStatement() string {
	return "CREATE DATABASE " + pgx.Identifier{embedpg.Database}.Sanitize()
}

// Two SQLSTATEs mean "the database is there, which is what this promises":
//
//   - 42P04 (duplicate_database): it existed before this CREATE was even parsed.
//     That is what every caller gets after the first one — `rafikid migrate`,
//     then the daemon, then `rafikid user create`.
//   - 23505 (unique_violation) on pg_database_datname_index: it arrived while
//     this CREATE was in flight. PostgreSQL's duplicate check and the catalog
//     insert are not one atomic step, so a genuinely simultaneous second caller
//     loses the index rather than the check, and the conflicting row is
//     committed by the time the violation is raised. Measured, not assumed:
//     eight simultaneous CREATE DATABASE against one server return 23505 for
//     the seven losers, while 42P04 is only ever seen by a caller that starts
//     after the winner's row is committed.
//
// Anything else is a failure the operator must see: a DSN naming a database
// that is not there fails on the NEXT statement, far from the cause.
const (
	sqlstateDuplicateDatabase = "42P04"
	sqlstateUniqueViolation   = "23505"

	// pgDatabaseNameIndex is the unique index those simultaneous creates race
	// on. Checked by name because a 23505 on any other relation is somebody
	// else's conflict and not this resolver's to swallow.
	pgDatabaseNameIndex = "pg_database_datname_index"
)

// databaseAlreadyExists reports whether err says the database is there, wrapped
// or not.
func databaseAlreadyExists(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case sqlstateDuplicateDatabase:
		return true
	case sqlstateUniqueViolation:
		return pgErr.ConstraintName == pgDatabaseNameIndex
	}
	return false
}

// ensureEmbeddedDatabase creates the database s.DSN() names if needed and
// reapplies its role defaults. Nothing is read back: the next statement on the
// resolved DSN is the proof, and it belongs to the caller.
func ensureEmbeddedDatabase(ctx context.Context, s embedpg.Server) error {
	err := createEmbeddedDatabase(ctx, s)
	if err == nil {
		return nil
	}
	return fmt.Errorf("embedded database: provisioning %s: %w", embedpg.Database, err)
}

// requireDSN resolves the daemon's database DSN, preferring an explicit --db
// over RAFIKI_DB, and returns an error when neither is set.
//
// The daemon REQUIRES a database. This was an open decision until 2026-08-25
// and is now settled (Phase C design 2.1): a DB-less daemon has no history, no
// cost accounting, no task ledger, no user identity, no executor plane and no
// conversation leases — it is a proxy that forwards to OpenRouter and spawns
// children with no memory. rafiki is the friend that keeps the history; one
// that keeps none is not rafiki.
//
// The rule binds the DAEMON only. `rafikid fundi` and `rafiki executor serve`
// keep their own --db handling, and pkg/llm, pkg/fundi and pkg/agentcli stay
// usable database-free inside them.
func requireDSN(optsDB string) (string, error) {
	dsn := optsDB
	if dsn == "" {
		dsn = paths.Get(paths.DB)
	}
	if dsn == "" {
		return "", errors.New(`no database configured: set RAFIKI_DB (or pass --db). ` +
			`For a local install run "rafiki setup", or set RAFIKI_DB=embedded to let ` +
			`rafikid run its own PostgreSQL`)
	}
	return dsn, nil
}
