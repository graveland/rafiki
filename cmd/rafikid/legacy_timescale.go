// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// legacyTimescaleExtensionsQuery lists the extensions rafiki no longer needs,
// toolkit first — the order they must be dropped in, because timescaledb_toolkit
// depends on timescaledb.
const legacyTimescaleExtensionsQuery = `SELECT extname FROM pg_extension WHERE extname IN ('timescaledb_toolkit','timescaledb') ORDER BY CASE extname WHEN 'timescaledb_toolkit' THEN 0 ELSE 1 END`

// legacyHypertablesQuery is what an operator should look at BEFORE dropping
// anything: a hypertable listed here is not rafiki's, and dropping the
// extension destroys it.
const legacyHypertablesQuery = "SELECT * FROM timescaledb_information.hypertables"

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
func warnLegacyTimescale(ctx context.Context, pool *pgxpool.Pool) {
	names, err := legacyTimescaleExtensions(ctx, pool)
	if err != nil {
		slog.Warn("cannot check whether TimescaleDB is still installed", "error", err)
		return
	}
	if len(names) == 0 {
		return
	}
	slog.Warn("rafiki no longer uses TimescaleDB; the extension is still installed in this database and can be removed",
		"extensions", strings.Join(names, ","),
		"check_first", legacyHypertablesQuery,
		"remove", "run as a superuser: "+legacyTimescaleDrop(names)+" and remove timescaledb from shared_preload_libraries",
	)
}
