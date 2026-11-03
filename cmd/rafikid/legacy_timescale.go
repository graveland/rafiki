// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// legacyTimescaleExtension and legacyToolkitExtension are the two extensions
// rafiki stopped needing, named once so the probe, the notice and the tests
// cannot drift apart.
const (
	legacyTimescaleExtension = "timescaledb"
	legacyToolkitExtension   = "timescaledb_toolkit"
)

// legacyTimescaleExtensionsQuery lists the extensions rafiki no longer needs,
// toolkit first — the safe order to drop them in, since the toolkit is installed
// alongside timescaledb.
const legacyTimescaleExtensionsQuery = `SELECT extname FROM pg_extension WHERE extname IN ('timescaledb_toolkit','timescaledb') ORDER BY CASE extname WHEN 'timescaledb_toolkit' THEN 0 ELSE 1 END`

// legacyHypertablesQuery is what an operator should look at BEFORE dropping
// anything: a hypertable listed here that is not one of rafiki's three is not
// rafiki's, and it must be dealt with before the extension goes — a DROP
// EXTENSION without CASCADE refuses while such a dependent remains.
const legacyHypertablesQuery = "SELECT * FROM timescaledb_information.hypertables"

// legacyRafikiHypertablesQuery names rafiki's three former hypertables. It is
// what separates "the extension can go" from "migrate first": before 0068 runs,
// these are still hypertables and dropping the extension would destroy them.
// The view only exists while timescaledb is installed, so it is never run on a
// database without the extension.
const legacyRafikiHypertablesQuery = `SELECT hypertable_schema||'.'||hypertable_name FROM timescaledb_information.hypertables WHERE (hypertable_schema, hypertable_name) IN (('conversations','conversation_turn'), ('openrouter','broadcast'), ('conversations','raw_http_request')) ORDER BY 1`

const (
	// legacyRemoveMessage is the notice for a migrated database: 0068 has run,
	// rafiki's tables are ordinary tables, and the extension is just left over.
	legacyRemoveMessage = "rafiki no longer uses TimescaleDB; the extension is still installed in this database and can be removed"
	// legacyMigrateFirstMessage is the notice while rafiki's own tables are
	// still hypertables — an unmigrated database, or a --dev start that warns
	// before it migrates. Removal is exactly the wrong advice there.
	legacyMigrateFirstMessage = "rafiki's tables are still TimescaleDB hypertables; run rafikid migrate before removing the extension"
	// legacyUnknownHypertablesMessage is the notice when the probe itself
	// failed: the state is unknown, so the confident advice must not be given.
	legacyUnknownHypertablesMessage = "cannot tell whether rafiki's tables are still TimescaleDB hypertables; run rafikid migrate before removing the extension"
)

// legacyTimescaleExtensions returns which of timescaledb_toolkit and timescaledb are installed in
// pool's database, toolkit first (the order they must be dropped in).
func legacyTimescaleExtensions(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, legacyTimescaleExtensionsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// legacyRafikiHypertables returns which of rafiki's three former hypertables
// are STILL hypertables in pool's database, schema-qualified and sorted. An
// empty result is what makes "the extension can be removed" true.
//
// It reads a view timescaledb creates, so the caller must only run it when
// timescaledb is installed; on any other database the query errors rather than
// answering "none".
func legacyRafikiHypertables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, legacyRafikiHypertablesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// legacyTimescaleDrop returns the statements that remove names, which must be in the order
// legacyTimescaleExtensions returns.
func legacyTimescaleDrop(names []string) string {
	if len(names) == 0 {
		return ""
	}
	stmts := make([]string, 0, len(names))
	for _, name := range names {
		stmts = append(stmts, "DROP EXTENSION "+name+";")
	}
	return strings.Join(stmts, " ")
}

// warnLegacyTimescale logs one WARN when either extension is installed; a failed probe is
// logged at WARN too, never returned.
//
// rafiki never drops the extension itself: it may have been installed for
// something else on this server, and dropping it takes every hypertable with
// it. The operator is told what to run instead.
//
// "Can be removed" is only said when it is true: the extension is installed AND
// none of rafiki's three former hypertables is still a hypertable. Before 0068
// runs — a daemon started ahead of `rafikid migrate`, or --dev, which warns
// before it migrates — they still are, and the notice says to migrate first. A
// probe that fails says the same, because an unknown state is not a safe one.
func warnLegacyTimescale(ctx context.Context, pool *pgxpool.Pool) {
	names, err := legacyTimescaleExtensions(ctx, pool)
	if err != nil {
		slog.Warn("cannot check whether TimescaleDB is still installed", "error", err)
		return
	}
	if len(names) == 0 {
		return
	}
	extensions := strings.Join(names, ",")

	// The hypertables view belongs to the extension, so there is nothing to ask
	// when timescaledb itself is absent (only the toolkit can be).
	var hypertables []string
	if slices.Contains(names, legacyTimescaleExtension) {
		hypertables, err = legacyRafikiHypertables(ctx, pool)
		if err != nil {
			slog.Warn(legacyUnknownHypertablesMessage,
				"extensions", extensions,
				"error", err,
				"next", "rafikid migrate",
			)
			return
		}
	}
	if len(hypertables) > 0 {
		slog.Warn(legacyMigrateFirstMessage,
			"extensions", extensions,
			"hypertables", strings.Join(hypertables, ","),
			"next", "rafikid migrate",
			"check_first", legacyHypertablesQuery,
		)
		return
	}

	slog.Warn(legacyRemoveMessage,
		"extensions", extensions,
		"check_first", legacyHypertablesQuery,
		"remove", "run as a superuser: "+legacyTimescaleDrop(names)+"; removing timescaledb from shared_preload_libraries is cluster-wide, so follow the documented procedure and check every database on the server first",
	)
}
