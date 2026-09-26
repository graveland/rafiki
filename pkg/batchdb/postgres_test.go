// SPDX-License-Identifier: Apache-2.0

package batchdb_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/batch"
	"go.graveland.dev/rafiki/pkg/batch/batchtest"
	"go.graveland.dev/rafiki/pkg/batchdb"
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
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// freshStore wipes conversations.batch_call before and after the subtest: the
// conformance suite starts every subtest from a fresh store, and subtests run
// sequentially, so a truncate around each is the per-subtest reset.
func freshStore(t *testing.T, pool *pgxpool.Pool) batch.Store {
	t.Helper()
	ctx := context.Background()
	clear := func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.batch_call`) }
	clear()
	t.Cleanup(clear)
	return batchdb.New(pool)
}

func TestPostgresConformance(t *testing.T) {
	pool := testPool(t)
	batchtest.Run(t, func(t *testing.T) batch.Store {
		return freshStore(t, pool)
	})
}

// TestStaleClobberNoOps pins the adopt-boundary no-op contract from task 1.3
// review minor 4: every transition is conditional on the row being live in its
// source state, so a stale sweep writing after an outcome landed (or a Fail
// after a Complete) must not clobber it. MemStore already behaves this way;
// these pin the same semantics on the Postgres store.
func TestStaleClobberNoOps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := freshStore(t, pool)

	row, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", Model: "vendor/m:batch", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.MarkSubmitting(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("MarkSubmitting: %v", err)
	}
	if err := s.MarkSubmitted(ctx, []int64{row.ID}, "batch-9"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := s.Complete(ctx, row.ID, json.RawMessage(`{"id":"msg_1"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, ok, err := s.Live(ctx, "conv-1")
	if err != nil || !ok {
		t.Fatalf("Live: %v %v", got, err)
	}

	// Every late write from the stale paths is a silent no-op.
	if err := s.Fail(ctx, row.ID, "late failure"); err != nil {
		t.Fatalf("Fail after Complete: %v", err)
	}
	if err := s.Requeue(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("Requeue after Complete: %v", err)
	}
	if err := s.MarkSubmitting(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("MarkSubmitting after Complete: %v", err)
	}
	if err := s.MarkSubmitted(ctx, []int64{row.ID}, "batch-late"); err != nil {
		t.Fatalf("MarkSubmitted after Complete: %v", err)
	}
	if err := s.Complete(ctx, row.ID, json.RawMessage(`{"id":"late"}`)); err != nil {
		t.Fatalf("Complete again: %v", err)
	}

	got, ok, err = s.Live(ctx, "conv-1")
	if err != nil || !ok {
		t.Fatalf("Live after stale writes: %v %v", got, err)
	}
	if got.State != batch.StateCompleted || got.ProviderBatchID != "batch-9" ||
		string(got.Response) != `{"id":"msg_1"}` || got.Error != "" {
		t.Fatalf("stale writes clobbered the completed row: %+v", got)
	}

	// Tombstone is idempotent: the second one is also a no-op.
	if err := s.Tombstone(ctx, row.ID); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if err := s.Tombstone(ctx, row.ID); err != nil {
		t.Fatalf("second Tombstone: %v", err)
	}
	if err := s.Fail(ctx, row.ID, "after tombstone"); err != nil {
		t.Fatalf("Fail after Tombstone: %v", err)
	}
	if _, ok, _ := s.Live(ctx, "conv-1"); ok {
		t.Fatal("Live after Tombstone: row visible")
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.batch_call WHERE custom_id = 'conv-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one tombstoned row, found %d", n)
	}
}
