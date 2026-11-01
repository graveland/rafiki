// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.graveland.dev/rafiki/pkg/embedpg"
	"go.graveland.dev/rafiki/pkg/paths"
)

// embeddedDSN is the RAFIKI_DB value that means "the rafiki-managed PostgreSQL
// under this user's data dir" (pkg/embedpg).
const embeddedDSN = "embedded"

// newEmbeddedServer builds the private server Default describes. It is a
// variable so tests can point the resolver at a scratch data dir and a fixture
// download instead of the real per-user install.
var newEmbeddedServer = embedpg.Default

// resolveDSN turns a configured DSN into one pgx can dial. "embedded" starts
// the private server if it is not running (downloading its binaries on first
// use, progress on stderr) and returns its socket DSN; any other value is
// returned unchanged. It is the only place "embedded" is interpreted.
func resolveDSN(ctx context.Context, dsn string) (string, error) {
	if dsn != embeddedDSN {
		return dsn, nil
	}
	s := newEmbeddedServer()
	s.Progress = os.Stderr
	out, err := s.Ensure(ctx)
	if err != nil {
		return "", fmt.Errorf("embedded database: %w", err)
	}
	return out, nil
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
