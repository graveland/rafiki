// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

const (
	legacyTimescaleExtension = "timescaledb"
	legacyToolkitExtension   = "timescaledb_toolkit"
)

// legacyWarnMessage is the notice's text, asserted verbatim: it is what an
// operator greps the daemon log for.
const legacyWarnMessage = "rafiki no longer uses TimescaleDB; the extension is still installed in this database and can be removed"

// TestLegacyTimescaleDropOrdersToolkitFirst pins the statement text and the
// drop ORDER. timescaledb_toolkit depends on timescaledb, so dropping the other
// way round fails with a dependency error; the names arrive already in that
// order (legacyTimescaleExtensions sorts them) and this function must not
// reorder them.
func TestLegacyTimescaleDropOrdersToolkitFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	cases := []struct {
		name  string
		names []string
		want  string
	}{
		{
			"both extensions",
			[]string{legacyToolkitExtension, legacyTimescaleExtension},
			"DROP EXTENSION timescaledb_toolkit; DROP EXTENSION timescaledb;",
		},
		{"timescaledb only", []string{legacyTimescaleExtension}, "DROP EXTENSION timescaledb;"},
		{"toolkit only", []string{legacyToolkitExtension}, "DROP EXTENSION timescaledb_toolkit;"},
		{"none installed", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.Eq(tc.want, legacyTimescaleDrop(tc.names), "legacyTimescaleDrop(%v)", tc.names)
		})
	}
}

// TestLegacyTimescaleExtensionsReportsInstalled pins the probe against a real
// server on both sides of its branch: a database with the extension installed
// must be reported (toolkit first, because it depends on timescaledb and must
// be dropped first), and a database without it must answer nothing. A probe
// that always returns a name fails the second half; one that never does fails
// the first.
func TestLegacyTimescaleExtensionsReportsInstalled(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()

	t.Run("installed", func(t *testing.T) {
		pool, haveToolkit := legacyPoolWithTimescale(t)
		names, err := legacyTimescaleExtensions(ctx, pool)
		c.Require().NoError(err, "probe the scratch database")
		want := []string{legacyTimescaleExtension}
		if haveToolkit {
			want = []string{legacyToolkitExtension, legacyTimescaleExtension}
		}
		c.EqDeep(want, names, "the installed extensions, toolkit first: it depends on timescaledb")
	})

	t.Run("absent", func(t *testing.T) {
		names, err := legacyTimescaleExtensions(ctx, legacyScratchPool(t))
		c.Require().NoError(err, "probe a database with no extensions installed")
		c.Empty(names, "nothing is installed, so the probe must report nothing; got %v", names)
	})
}

// TestLegacyTimescaleWarnsOnce pins the notice itself: one WARN, with the exact
// message and the three operator-facing values, when an extension is installed —
// and NO record at all when neither is. The single-record half is what keeps
// the notice a startup line rather than log spam.
func TestLegacyTimescaleWarnsOnce(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()

	t.Run("installed", func(t *testing.T) {
		pool, _ := legacyPoolWithTimescale(t)
		// The probe's own answer is the source of truth for the values
		// asserted below, so they pin the NOTICE's rendering (comma join,
		// statement order, the superuser prefix) rather than restating which
		// extensions this server provides.
		names, err := legacyTimescaleExtensions(ctx, pool)
		c.Require().NoError(err, "probe the scratch database")
		c.Require().NotEmpty(names, "the scratch database must have an extension installed")
		rec := captureLegacyLogs(t)

		warnLegacyTimescale(ctx, pool)

		records := rec.snapshot()
		c.Require().Len(records, 1, "exactly one record for an installed extension; got %+v", records)
		c.Eq(slog.LevelWarn, records[0].Level, "the notice must be a WARN")
		c.Eq(legacyWarnMessage, records[0].Msg, "the notice's text")
		c.Eq(strings.Join(names, ","), records[0].Attrs["extensions"],
			"the extensions the probe found, comma-joined in drop order")
		c.Eq("SELECT * FROM timescaledb_information.hypertables", records[0].Attrs["check_first"],
			"the check that must precede a drop: a hypertable listed there is not rafiki's")
		c.Eq("run as a superuser: "+legacyTimescaleDrop(names)+" and remove timescaledb from shared_preload_libraries",
			records[0].Attrs["remove"], "the removal instructions")
	})

	t.Run("absent", func(t *testing.T) {
		pool := legacyScratchPool(t)
		rec := captureLegacyLogs(t)

		warnLegacyTimescale(ctx, pool)

		c.Empty(rec.snapshot(), "nothing to say when neither extension is installed")
	})
}

// legacyScratchPool creates a private database on the test server, with no
// extensions and WITHOUT migrating: what these tests read is the extension set,
// and a private database is what makes both states reachable on one server.
//
// Created from template0 rather than template1 deliberately. A template1 copy
// fails with SQLSTATE 55006 ("source database \"template1\" is being accessed by
// other users") whenever TimescaleDB's background worker scheduler happens to
// be attached to template1 — measured on the pg-test server, where a scheduler
// worker cycles through every database that has the extension installed.
// Creating the extensions here instead also makes the premise the test's own
// rather than inherited from however the server was set up.
func legacyScratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_legacy_ts_%d", time.Now().UnixNano())
	// CREATE DATABASE takes no bind parameters; the name is test-controlled,
	// never input.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" WITH TEMPLATE template0"); err != nil {
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
	return pool
}

// legacyPoolWithTimescale returns a scratch database with timescaledb
// installed and, when the server provides it, timescaledb_toolkit too. The bool
// reports whether the toolkit is there, so a caller can assert the exact list
// without restating the server's extension set.
func legacyPoolWithTimescale(t *testing.T) (*pgxpool.Pool, bool) {
	t.Helper()
	pool := legacyScratchPool(t)
	legacyCreateExtension(t, pool, legacyTimescaleExtension)
	if !legacyExtensionAvailable(t, pool, legacyToolkitExtension) {
		return pool, false
	}
	legacyCreateExtension(t, pool, legacyToolkitExtension)
	return pool, true
}

// legacyCreateExtension installs one extension in pool's database.
func legacyCreateExtension(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	c := assert.NewAborting(t)
	// CREATE EXTENSION takes no bind parameters; name is a constant here.
	_, err := pool.Exec(context.Background(), "CREATE EXTENSION "+name)
	c.NoError(err, "create extension %s in the scratch database", name)
}

// legacyExtensionAvailable reports whether the server can create name in pool's
// database.
func legacyExtensionAvailable(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	c := assert.NewAborting(t)
	var available bool
	c.NoError(pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = $1)`, name).Scan(&available),
		"probe pg_available_extensions for %s", name)
	return available
}

// legacyLogRecord is one captured slog record, flattened to what the notice is
// asserted on.
type legacyLogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

// legacyLogRecorder collects slog records in memory. Every method is safe for
// concurrent use so a -race run cannot trip on it even though warnLegacyTimescale
// logs from the calling goroutine only.
type legacyLogRecorder struct {
	mu      sync.Mutex
	records []legacyLogRecord
}

func (r *legacyLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *legacyLogRecorder) Handle(_ context.Context, rec slog.Record) error {
	entry := legacyLogRecord{Level: rec.Level, Msg: rec.Message, Attrs: map[string]string{}}
	rec.Attrs(func(a slog.Attr) bool {
		entry.Attrs[a.Key] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, entry)
	return nil
}

func (r *legacyLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *legacyLogRecorder) WithGroup(string) slog.Handler      { return r }

func (r *legacyLogRecorder) snapshot() []legacyLogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]legacyLogRecord, len(r.records))
	copy(out, r.records)
	return out
}

// captureLegacyLogs swaps in a recording default logger for the rest of the
// test. Package tests run sequentially, so the global swap is safe; restored
// via cleanup.
func captureLegacyLogs(t *testing.T) *legacyLogRecorder {
	t.Helper()
	rec := &legacyLogRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}
