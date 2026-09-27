// SPDX-License-Identifier: Apache-2.0

package inboxdb_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/inbox/inboxtest"
	"go.graveland.dev/rafiki/pkg/inboxdb"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pgxpool.New")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
	return pool
}

func TestPostgresConformance(t *testing.T) {
	pool := testPool(t)
	inboxtest.RunConformance(t, func(t *testing.T) (inbox.Store, string) {
		// A prefix unique per subtest keeps a shared database from letting one
		// run see another's rows -- the residue lesson from the integration
		// suite applies to every DB-backed test in this repo.
		pfx := "c_" + t.Name() + "_"
		id, err := inbox.NewID()
		assert.NewAborting(t).NoError(err, "NewID")
		pfx += id + "_"
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				"DELETE FROM conversations.agent_inbox WHERE child_id LIKE $1", pfx+"%")
		})
		return inboxdb.New(pool), pfx
	})
}

// The Postgres-only concurrency test (a real contention barrier around
// markSentSQL) lives in postgres_internal_test.go, in package inboxdb rather
// than inboxdb_test: it needs to execute the real markSentSQL constant
// directly and read pgconn.CommandTag.RowsAffected, neither of which is
// visible through the exported inbox.Store interface.
