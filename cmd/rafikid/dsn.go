// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

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

// newEmbeddedServer builds the private server Default describes. It is a
// variable so tests can point the resolver at a scratch data dir and a fixture
// download instead of the real per-user install.
var newEmbeddedServer = embedpg.Default

// resolveDSN turns a configured DSN into one pgx can dial. "embedded" starts
// the private server if it is not running (downloading its binaries on first
// use, progress on stderr), creates its database if it is absent, and returns
// its socket DSN; any other value is returned unchanged. It is the only place
// "embedded" is interpreted, and the only place an embedded database is
// created.
func resolveDSN(ctx context.Context, dsn string) (string, error) {
	if dsn != embeddedDSN {
		return dsn, nil
	}
	s := newEmbeddedServer()
	s.Progress = os.Stderr
	// Ensure starts the server; it does not create Database, because
	// pkg/embedpg opens no connection and a CREATE DATABASE is one. A fresh
	// cluster has only the maintenance database.
	if _, err := s.Ensure(ctx); err != nil {
		return "", fmt.Errorf("embedded database: %w", err)
	}
	if err := ensureEmbeddedDatabase(ctx, s); err != nil {
		return "", err
	}
	return s.DSN(), nil
}

// createEmbeddedDatabase makes the database a resolved embedded DSN names. It
// is a variable so a test can stand in for the server: connecting to one takes
// a real cluster, while what resolveDSN must get right — the statement, the
// maintenance DSN it is issued on, and the SQLSTATE it tolerates — does not.
var createEmbeddedDatabase = createEmbeddedDatabaseFor

// createEmbeddedDatabaseFor runs the CREATE DATABASE over a short-lived
// connection to the maintenance database: initdb creates only that one, so it
// is the only database a fresh cluster can be asked from. The connection is
// closed before returning whether or not the statement worked, and on a context
// the caller has already cancelled too, so a failed create does not leave a
// session parked on a server nothing here ever stops.
func createEmbeddedDatabaseFor(ctx context.Context, s embedpg.Server) error {
	conn, err := pgx.Connect(ctx, s.MaintenanceDSN())
	if err != nil {
		return fmt.Errorf("connecting to the maintenance database: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	_, err = conn.Exec(ctx, embeddedDatabaseStatement())
	return err
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

// ensureEmbeddedDatabase creates the database s.DSN() names, and is a no-op
// when it is already there. Nothing is read back: the next statement on the
// resolved DSN is the proof, and it belongs to the caller.
func ensureEmbeddedDatabase(ctx context.Context, s embedpg.Server) error {
	err := createEmbeddedDatabase(ctx, s)
	if err == nil || databaseAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("embedded database: creating %s: %w", embedpg.Database, err)
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
