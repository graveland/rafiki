// SPDX-License-Identifier: Apache-2.0

package inboxdb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/store"
)

// This file is a white-box test (package inboxdb, not inboxdb_test)
// deliberately: it needs to execute the real markSentSQL constant directly
// and inspect pgconn.CommandTag.RowsAffected, neither of which the exported
// inbox.Store interface exposes -- MarkSent returns only an error, which
// cannot distinguish "0 rows matched the guard" from "1 row matched and was
// set to the value it already had".

func testPoolInternal(t *testing.T) *pgxpool.Pool {
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

// TestConcurrentMarkSentIsIdempotent is a real contention barrier, not a
// sequenced no-op check: tx1 takes the row to 'sent' and holds the row lock
// UNCOMMITTED, while a second, independent connection fires the production
// markSentSQL against the very same row concurrently. Postgres blocks that
// second writer on the row lock rather than letting it proceed. Only once
// tx1 commits does the blocked statement resume -- and when it does, it
// re-evaluates its WHERE clause against the now-current row (Postgres's
// EvalPlanQual mechanism under READ COMMITTED), which is 'sent', not
// 'pending'. A correctly guarded query therefore matches zero rows.
//
// This is the case markSentSQL's "AND state = 'pending'" guard exists for: if
// that clause were removed, the very same blocked statement would resume,
// match the row by id alone, and re-affect it -- silently re-sending a
// message a model has already acted on. Deleting the guard from postgres.go
// and re-running this test demonstrates exactly that (see the fix report for
// the captured failing output).
func TestConcurrentMarkSentIsIdempotent(t *testing.T) {
	pool := testPoolInternal(t)
	ctx := context.Background()

	uniq, err := inbox.NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	child := "c_race_" + uniq
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM conversations.agent_inbox WHERE child_id = $1", child)
	})

	s := New(pool)
	rec, err := s.Accept(ctx, inbox.Inbound{ChildID: child, Mode: inbox.ModePrompt, Text: "hi"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()

	// tx1 takes the row to 'sent' and holds the row lock uncommitted.
	if _, err := tx1.Exec(ctx,
		"UPDATE conversations.agent_inbox SET state='sent' WHERE id=$1", rec.ID); err != nil {
		t.Fatalf("tx1 update: %v", err)
	}

	// A separate reader must not see tx1's uncommitted change: the row is
	// still 'pending' to everyone else until tx1 commits.
	rows, err := s.Pending(ctx, child)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("before commit the row is still pending to everyone else; got %d rows", len(rows))
	}

	// Fire the production markSentSQL from a second, independent connection
	// while tx1 still holds the row locked uncommitted. This call blocks
	// until tx1 resolves.
	tagCh := make(chan pgconn.CommandTag, 1)
	errCh := make(chan error, 1)
	go func() {
		tag, err := pool.Exec(ctx, markSentSQL, []string{rec.ID})
		if err != nil {
			errCh <- err
			return
		}
		tagCh <- tag
	}()

	// Give the goroutine's UPDATE time to reach and block on the row lock
	// before releasing it -- otherwise the two statements aren't guaranteed
	// to overlap and the barrier proves nothing.
	time.Sleep(200 * time.Millisecond)

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("concurrent markSentSQL: %v", err)
	case tag := <-tagCh:
		if tag.RowsAffected() != 0 {
			t.Fatalf("concurrent markSentSQL affected %d rows, want 0 (row was already sent by tx1)", tag.RowsAffected())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent markSentSQL never returned; it should have unblocked once tx1 committed")
	}

	if rows, _ := s.Pending(ctx, child); len(rows) != 0 {
		t.Fatalf("row came back as pending after the race: %+v", rows)
	}
}
