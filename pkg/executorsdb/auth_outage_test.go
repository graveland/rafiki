// SPDX-License-Identifier: Apache-2.0

package executorsdb

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
)

// An auth OUTAGE must not be reported as an auth FAILURE.
//
// executors.IsTerminalAuthError classifies ErrNotFound as terminal, and an
// executor that receives a terminal error stops permanently. So collapsing a
// dead database connection into ErrNotFound told every executor reconnecting
// during a blip that its credential was revoked — and across a fleet that
// reconnects together, that is the entire fleet, needing manual restarts.
//
// This is the invariant CLAUDE.md states as "quitting on a dead credential
// costs a log line and quitting on a transient one costs the fleet". It was
// violated here for the lifetime of the file; the test exists so it cannot be
// reintroduced by a future tidy-up of the error handling.
func TestAuthenticateOnAClosedPoolIsNotTerminal(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("RAFIKI_DB")
	}
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	store := NewPostgresStore(pool)
	pool.Close() // the outage

	_, err = store.Authenticate(context.Background(), "any-credential")
	if err == nil {
		t.Fatal("authenticating against a closed pool succeeded")
	}
	if errors.Is(err, executors.ErrNotFound) {
		t.Fatal("a store outage was reported as ErrNotFound, which IsTerminalAuthError treats as terminal — every executor reconnecting during a database blip would exit permanently")
	}
	if executors.IsTerminalAuthError(err) {
		t.Fatalf("IsTerminalAuthError(%v) = true; an unanswerable check must be retryable", err)
	}
}
