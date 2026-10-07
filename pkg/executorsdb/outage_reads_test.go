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

// The same rule auth_outage_test.go states for authentication — a database
// failure must stay a wrapped error, never an ANSWER — holds for every READ
// on the executor row, not only for the credential check.
//
// ErrNotFound is not just a signal to a caller: it is read as "the row is
// gone". execpool.refreshRow REVOKES a connected executor on ErrNotFound but
// keeps the last known row on any other error, and executor ref resolution
// falls through to a suffix search on ErrNotFound but surfaces a real error.
// Collapsing a dead connection into ErrNotFound therefore told a connected
// executor that its row had been deleted, and it exited — the same fleet-wide
// failure the auth test describes, reached through the read path instead of
// the credential path.
//
// Get, SetLabels and Annotate each collapsed EVERY error into ErrNotFound for
// the lifetime of the file, while Authenticate, Enroll and the usersdb store
// all checked pgx.ErrNoRows first. These tests exist so the rule cannot be
// reintroduced by a tidy-up of any one of them.
func TestGetOnAClosedPoolIsNotNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := testDSN(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect")
	store := NewPostgresStore(pool)
	pool.Close() // the outage

	// A WELL-FORMED id, so the malformed-id answer above does not short-circuit
	// this before the pool is ever touched: the outage has to be reached by the
	// query itself.
	_, err = store.Get(context.Background(), "00000000-0000-0000-0000-000000000000")
	c.Error(err, "Get against a closed pool succeeded")
	c.False(errors.Is(err, executors.ErrNotFound),
		"a store outage was reported as ErrNotFound, which execpool.refreshRow treats as a revoked row — every executor connected during a database blip would exit")

	// And the ANSWER still has to be reachable, in BOTH of its shapes, or the
	// fix has traded one bug for another:
	//   - a well-formed id that is simply not there;
	//   - a malformed id, which cannot name a row at all — executor ref
	//     resolution passes a user-supplied ref that may be a short SUFFIX and
	//     only falls through to its suffix search on ErrNotFound.
	live, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect (live)")
	defer live.Close()
	liveStore := NewPostgresStore(live)
	_, err = liveStore.Get(context.Background(), "00000000-0000-0000-0000-000000000000")
	c.True(errors.Is(err, executors.ErrNotFound),
		"a genuinely missing row must still be ErrNotFound; got %v", err)
	_, err = liveStore.Get(context.Background(), "7f3a1c")
	c.True(errors.Is(err, executors.ErrNotFound),
		"a malformed (suffix-shaped) ref must be ErrNotFound so the suffix search can run; got %v", err)
}

// SetLabels and Annotate read the row FOR UPDATE inside a transaction, so a
// closed pool fails at Begin before reaching the line under test. Block the
// read itself instead: another transaction holds the ROW FOR UPDATE, so the
// SELECT queues behind it and the caller's own short deadline fires first — a
// real "I could not read" rather than "no such row".
//
// A ROW lock on a scratch row, not a table lock. This table is read and written
// by every test/integration daemon, and make check runs packages in PARALLEL,
// so a table lock here would stall unrelated packages for the length of this
// test — the very class of cross-test interference these fixes exist to remove.
// (auth_outage_test.go locks the enrollment-token table, which only the enroll
// path touches; this one is not so lucky.)
func TestReadsBlockedByALockAreNotNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := testDSN(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect")
	defer pool.Close()
	store := NewPostgresStore(pool)

	// A scratch row nothing else in the suite names. The pool needs at least
	// two connections for this test to reach the SELECT: one held by the locker
	// and one for the store call (pgxpool's default is well above that, and a
	// pool of one would fail at Begin, where the pre-fix code already returned
	// a real error — the test would pass vacuously).
	scratch, _, err := store.Create(context.Background(), executors.NewToken{})
	c.NoError(err, "create scratch executor")
	t.Cleanup(func() { _ = store.Delete(context.Background(), scratch.ID) })

	lockTx, err := pool.Begin(context.Background())
	c.NoError(err, "begin locker tx")
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var locked string
	if err := lockTx.QueryRow(context.Background(),
		`SELECT id FROM conversations.executors WHERE id = $1 FOR UPDATE`, scratch.ID).Scan(&locked); err != nil {
		t.Fatalf("lock the scratch row: %v", err)
	}

	for _, tc := range []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"SetLabels", func(ctx context.Context) error {
			_, err := store.SetLabels(ctx, scratch.ID, map[string]string{"a": "b"}, nil)
			return err
		}},
		{"Annotate", func(ctx context.Context) error {
			return store.Annotate(ctx, scratch.ID, map[string]string{"a": "b"}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err := tc.call(ctx)
			c.Error(err, "%s while the executor table was locked succeeded", tc.name)
			c.False(errors.Is(err, executors.ErrNotFound),
				"%s reported a blocked read as ErrNotFound, which callers read as \"the row is gone\"; got %v", tc.name, err)
		})
	}
}

// testDSN returns the disposable test database's DSN, skipping the test when
// the developer has not supplied one — the same convention as testStores.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("RAFIKI_DB")
	}
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN is not set")
	}
	return dsn
}
