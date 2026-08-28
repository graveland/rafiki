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
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
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
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		pfx += id + "_"
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				"DELETE FROM conversations.agent_inbox WHERE child_id LIKE $1", pfx+"%")
		})
		return inboxdb.New(pool), pfx
	})
}

// TestConcurrentMarkSentIsIdempotent opens two transactions explicitly. The
// shared conformance suite cannot catch this: the memory store is atomic under
// its own mutex and Postgres is not, which is exactly how pkg/tasks' Drop bug
// passed the shared suite throughout.
func TestConcurrentMarkSentIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := inboxdb.New(pool)

	uniq, _ := inbox.NewID()
	child := "c_race_" + uniq
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM conversations.agent_inbox WHERE child_id = $1", child)
	})

	rec, err := s.Accept(ctx, inbox.Inbound{ChildID: child, Mode: inbox.ModePrompt, Text: "hi"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()

	// tx1 takes the row to 'sent' and holds the lock uncommitted. A second
	// writer must not be able to observe it as pending and deliver it again.
	if _, err := tx1.Exec(ctx,
		"UPDATE conversations.agent_inbox SET state='sent' WHERE id=$1", rec.ID); err != nil {
		t.Fatalf("tx1 update: %v", err)
	}

	rows, err := s.Pending(ctx, child)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("before commit the row is still pending to everyone else; got %d rows", len(rows))
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After the commit, MarkSent from the loser must be a no-op rather than
	// resurrecting or double-counting anything.
	if err := s.MarkSent(ctx, []string{rec.ID}); err != nil {
		t.Fatalf("MarkSent after commit: %v", err)
	}
	rows, _ = s.Pending(ctx, child)
	if len(rows) != 0 {
		t.Fatalf("row came back as pending after being sent: %+v", rows)
	}
}
