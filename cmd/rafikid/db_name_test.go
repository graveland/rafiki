// SPDX-License-Identifier: Apache-2.0

package main

// The embedded server's database is rafiki, not the postgres maintenence
// database initdb creates. pkg/embedpg names it (it opens no connection, so it
// cannot create anything); this file pins what rafikid does with that name:
// create it from the maintenance database before handing out a DSN, tolerate
// the second caller that finds it already there, and let any other failure
// through as a startup error.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/embedpg"
)

// CREATE DATABASE takes no bind parameters, so the name is interpolated into the
// statement — quoted, or a name with a capital in it would be folded and no
// longer be the database DSN() names.
func TestEmbeddedDatabaseStatementIsAQuotedIdentifier(t *testing.T) {
	c := assert.NewAborting(t)
	stmt := embeddedDatabaseStatement()
	c.Eq(`CREATE DATABASE "rafiki"`, stmt, "the create statement")
	c.StrContains(stmt, embedpg.Database, "the statement must name the embedded database, got: %s", stmt)
}

// "already there" is success and nothing else is. Both shapes are real: the
// sequential second caller gets duplicate_database, and a caller racing the
// first one at the same instant loses the pg_database unique index instead. A
// create that failed for any other reason must not be swallowed: the resolver
// promises a DSN that connects, and the next statement would fail far from the
// cause.
func TestDatabaseAlreadyExists(t *testing.T) {
	duplicate := &pgconn.PgError{Code: "42P04", Message: `database "rafiki" already exists`}
	raceLost := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: pgDatabaseNameIndex,
		Message:        `duplicate key value violates unique constraint "pg_database_datname_index"`,
	}

	c := assert.NewAborting(t)
	c.True(databaseAlreadyExists(duplicate), "42P04 must count as the database being there already")
	c.True(databaseAlreadyExists(fmt.Errorf("exec: %w", duplicate)), "a wrapped 42P04 must still count")
	c.True(databaseAlreadyExists(raceLost), "losing pg_database_datname_index means the database arrived concurrently")

	c.False(databaseAlreadyExists(nil), "success is not a duplicate")
	c.False(databaseAlreadyExists(errors.New(`database "rafiki" already exists`)), "only the SQLSTATE may decide")
	c.False(databaseAlreadyExists(&pgconn.PgError{Code: "23505"}), "a unique violation elsewhere in the catalog is not this")
	c.False(databaseAlreadyExists(&pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"}),
		"only pg_database's own index may be swallowed")
	c.False(databaseAlreadyExists(&pgconn.PgError{Code: "42501"}), "a permission failure is a different failure")
}

// stubCreateDatabase swaps the create seam for one that answers with errs in
// order (the last one repeats) and records every server it was asked about.
// The production seam connects to a cluster; the resolver's job — that the
// create runs on every resolve, against the maintenance database, and that only
// "it is already there" is tolerated — needs none.
func stubCreateDatabase(t *testing.T, errs ...error) *[]embedpg.Server {
	t.Helper()
	seen := make([]embedpg.Server, 0, 2)
	orig := createEmbeddedDatabase
	t.Cleanup(func() { createEmbeddedDatabase = orig })
	createEmbeddedDatabase = func(_ context.Context, s embedpg.Server) error {
		seen = append(seen, s)
		if len(errs) == 0 {
			return nil
		}
		err := errs[0]
		if len(errs) > 1 {
			errs = errs[1:]
		}
		return err
	}
	return &seen
}

// fakeEmbeddedServer is a Server whose Ensure succeeds with no cluster behind
// it: bin/postgres is the "binaries are installed" marker (nothing is
// downloaded) and bin/pg_ctl status exits 0, so the server is believed to be
// running. The data dir comes from /tmp, not t.TempDir(), because the server's
// only socket lives inside it and sun_path is 104 bytes on macOS.
func fakeEmbeddedServer(t *testing.T) embedpg.Server {
	t.Helper()
	c := assert.NewAborting(t)
	dir, err := os.MkdirTemp("/tmp", "epg")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s := embedpg.Server{
		BinDir:   filepath.Join(dir, "bin"),
		DataDir:  filepath.Join(dir, "data"),
		LogPath:  filepath.Join(dir, "pg.log"),
		LockPath: filepath.Join(dir, "pg.lock"),
	}
	c.NoError(os.MkdirAll(filepath.Join(s.BinDir, "bin"), 0o755), "MkdirAll bin")
	c.NoError(os.WriteFile(filepath.Join(s.BinDir, "bin", "postgres"), nil, 0o755), "write postgres")
	c.NoError(os.WriteFile(filepath.Join(s.BinDir, "bin", "pg_ctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755), "write pg_ctl")
	c.NoError(os.MkdirAll(s.DataDir, 0o700), "MkdirAll data dir")
	c.NoError(os.WriteFile(filepath.Join(s.DataDir, "PG_VERSION"), []byte("18\n"), 0o600), "PG_VERSION")
	c.NoError(os.WriteFile(filepath.Join(s.DataDir, "postgresql.conf"), []byte("# as initdb left it\n"), 0o600), "postgresql.conf")
	return s
}

// pointResolverAt makes newEmbeddedServer hand back s for the rest of the test.
func pointResolverAt(t *testing.T, s embedpg.Server) {
	t.Helper()
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server { return s }
}

// Every embedded resolve creates the database, from the maintenance database,
// and every answer that means "it is already there" is a successful resolve:
// duplicate_database for a caller that comes after the first one, and the lost
// pg_database_datname_index for one that arrives at the same instant. Resolving
// more than once is ordinary — `rafikid migrate`, then `rafikid user create`,
// then the daemon — and so is two of those starting together.
func TestResolveDSNEmbeddedCreatesTheDatabase(t *testing.T) {
	c := assert.NewAborting(t)
	s := fakeEmbeddedServer(t)
	pointResolverAt(t, s)
	answers := []error{
		nil,
		&pgconn.PgError{Code: sqlstateDuplicateDatabase},
		&pgconn.PgError{Code: sqlstateUniqueViolation, ConstraintName: pgDatabaseNameIndex},
	}
	seen := stubCreateDatabase(t, answers...)

	for run := 1; run <= len(answers); run++ {
		got, err := resolveDSN(context.Background(), embeddedDSN)
		c.NoError(err, "resolveDSN (run %d)", run)
		c.Eq(s.DSN(), got, "resolveDSN must hand back the server's own DSN (run %d)", run)
	}

	c.Eq(len(answers), len(*seen), "the create must be attempted on every resolve, got %d attempt(s)", len(*seen))
	for i, srv := range *seen {
		c.Eq(s.DataDir, srv.DataDir, "attempt %d must be against the resolved server", i+1)
		cfg, err := pgconn.ParseConfig(srv.MaintenanceDSN())
		c.NoError(err, "parse the maintenance DSN (attempt %d)", i+1)
		c.Eq("postgres", cfg.Database, "attempt %d must be issued on the maintenance database", i+1)
	}
}

// A create that failed for any other reason is the resolver's error, wrapped as
// one: the caller gets no DSN at all rather than one naming a database that is
// not there.
func TestResolveDSNEmbeddedRefusesAfterACreateFailure(t *testing.T) {
	c := assert.NewAborting(t)
	pointResolverAt(t, fakeEmbeddedServer(t))
	stubCreateDatabase(t, errors.New("disk full"))

	got, err := resolveDSN(context.Background(), embeddedDSN)
	c.Error(err, "want an error when the database cannot be created, got nil")
	c.Eq("", got, "no DSN may be handed back after a failed create")
	c.ErrorContains(err, "embedded database", "the error must be wrapped as an embedded-database failure, got: %v", err)
	c.ErrorContains(err, embedpg.Database, "the error must name the database, got: %v", err)
	c.ErrorContains(err, "disk full", "the cause must survive, got: %v", err)
}

// The real claim, on a real cluster: the first resolveDSN starts a private
// server and creates its database, the DSN it returns really connects to that
// database, and the second resolveDSN finds it and is a no-op.
//
// Gated on RAFIKI_TEST_EMBEDPG=1 because the first run downloads the pinned
// binaries (~33 MB on macOS, ~15 MB on Linux) from Maven Central; the cache is
// shared with test/integration's embedded test, so a suite that ran that one
// pays nothing here. -count=1 always: go test's cache cannot see through to a
// server subprocess.
func TestEmbeddedDatabaseIsCreatedOnARealServer(t *testing.T) {
	if os.Getenv("RAFIKI_TEST_EMBEDPG") != "1" {
		t.Skip("RAFIKI_TEST_EMBEDPG=1 runs this against a real embedded server (downloads the pinned binaries once)")
	}
	c := assert.NewAborting(t)

	// os.UserCacheDir() must be read BEFORE XDG_CACHE_HOME is pointed at the
	// scratch tree, or the binaries would be looked for in the tree it is about
	// to name.
	cache, err := os.UserCacheDir()
	c.NoError(err, "UserCacheDir")
	root, err := os.MkdirTemp("/tmp", "epg")
	c.NoError(err, "MkdirTemp")

	// A SHORT root under /tmp, never t.TempDir(): the server's only socket lives
	// inside its data directory, so a long path is refused before anything runs.
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cache, "rafiki-test-embedpg"))

	server := embedpg.Default()
	// rafikid never stops the private server, so this test does — and only then
	// throws its data dir away; a server that would not stop leaves the tree for
	// a human, because deleting the data dir under a running postmaster is worse
	// than a stray directory in /tmp.
	t.Cleanup(func() {
		if !stopEmbeddedServer(t, server) {
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Logf("remove %s: %v", root, err)
		}
	})

	ctx := context.Background()
	first, err := resolveDSN(ctx, embeddedDSN)
	c.NoError(err, "resolveDSN on a fresh cluster")
	c.Eq(server.DSN(), first, "resolveDSN must return the server's own DSN")

	conn, err := pgx.Connect(ctx, first)
	c.NoError(err, "the resolved DSN must connect: %s", first)
	defer conn.Close(context.WithoutCancel(ctx))

	var current string
	c.NoError(conn.QueryRow(ctx, "SELECT current_database()").Scan(&current), "current_database")
	c.Eq(embedpg.Database, current, "the resolved DSN must land in the embedded database")

	var databases int64
	c.NoError(conn.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", embedpg.Database).Scan(&databases),
		"count the embedded database")
	c.Eq(int64(1), databases, "exactly one %s database", embedpg.Database)

	// The second resolve: the server is already running and the database is
	// already there, so PostgreSQL answers duplicate_database (42P04) and the
	// resolver must treat it as done.
	second, err := resolveDSN(ctx, embeddedDSN)
	c.NoError(err, "the second resolveDSN must be a no-op, not an error")
	c.Eq(first, second, "the second resolveDSN must return the same DSN")
}

// stopEmbeddedServer stops a private server this test started and reports
// whether it is gone. A failure is logged rather than fatal: the data dir is
// what lets a human finish the job.
func stopEmbeddedServer(t *testing.T, s embedpg.Server) bool {
	t.Helper()
	pgctl := filepath.Join(s.BinDir, "bin", "pg_ctl")
	out, err := exec.Command(pgctl, "stop", "-D", s.DataDir, "-m", "fast", "-w").CombinedOutput()
	if err != nil {
		t.Logf("pg_ctl stop: %v: %s (data dir left at %s)", err, strings.TrimSpace(string(out)), s.DataDir)
		return false
	}
	return true
}
