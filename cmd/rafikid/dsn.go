// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"go.graveland.dev/rafiki/pkg/paths"
)

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
		return "", errors.New("no database configured: set RAFIKI_DB (or pass --db). " +
			"rafikid requires TimescaleDB; the documented local setup is: " +
			"docker run -d -p 5433:5432 -e POSTGRES_PASSWORD=postgres timescale/timescaledb:2.28.2-pg18")
	}
	return dsn, nil
}