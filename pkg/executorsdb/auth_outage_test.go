// SPDX-License-Identifier: Apache-2.0

package executorsdb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("RAFIKI_DB")
	}
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect")
	store := NewPostgresStore(pool)
	pool.Close() // the outage

	_, err = store.Authenticate(context.Background(), "any-credential")
	c.Error(err, "authenticating against a closed pool succeeded")
	c.False(errors.Is(err, executors.ErrNotFound), "a store outage was reported as ErrNotFound, which IsTerminalAuthError treats as terminal — every executor reconnecting during a database blip would exit permanently")
	c.False(executors.IsTerminalAuthError(err), "IsTerminalAuthError(%v) = true; an unanswerable check must be retryable", err)
}

// The same invariant, on Enroll's token lookup specifically.
//
// Enroll wraps its work in a transaction, so a closed pool fails at Begin
// before ever reaching the token-lookup query — that doesn't exercise the
// line this test is for. Instead this blocks the lookup itself: a second
// connection holds an ACCESS EXCLUSIVE lock on the token table, so Enroll's
// SELECT queues behind it and its own short context deadline fires first,
// producing a real "I could not check" error rather than "no such row".
// Before the fix, this path returned ErrTokenUnknown unconditionally, which
// IsTerminalAuthError treats as terminal — every executor enrolling during a
// database blip would have been told its token was permanently invalid.
func TestEnrollBlockedByALockIsNotTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("RAFIKI_DB")
	}
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN is not set")
	}

	lockerPool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect (locker)")
	defer lockerPool.Close()

	lockTx, err := lockerPool.Begin(context.Background())
	c.NoError(err, "begin locker tx")
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if _, err := lockTx.Exec(context.Background(),
		"LOCK TABLE conversations.executor_enrollment_token IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("acquire table lock: %v", err)
	}

	storePool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect (store)")
	defer storePool.Close()
	store := NewPostgresStore(storePool)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, _, err = store.Enroll(ctx, "any-token", nil)
	c.Error(err, "enrolling while the token table was locked succeeded")
	c.False(errors.Is(err, executors.ErrTokenUnknown), "a blocked lookup was reported as ErrTokenUnknown, which IsTerminalAuthError treats as terminal — every executor enrolling during a database blip would be told its token was permanently invalid")
	c.False(executors.IsTerminalAuthError(err), "IsTerminalAuthError(%v) = true; an unanswerable check must be retryable", err)
}
