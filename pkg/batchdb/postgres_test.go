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
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
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
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	s := freshStore(t, pool)

	row, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", Model: "vendor/m:batch", State: batch.StateQueued})
	c.NoError(err, "Insert")
	c.NoError(s.MarkSubmitting(ctx, []int64{row.ID}), "MarkSubmitting")
	c.NoError(s.MarkSubmitted(ctx, []int64{row.ID}, "batch-9"), "MarkSubmitted")
	c.NoError(s.Complete(ctx, row.ID, json.RawMessage(`{"id":"msg_1"}`)), "Complete")
	got, ok, err := s.Live(ctx, "conv-1")
	c.False(err != nil || !ok, "Live: %v %v", got, err)

	// Every late write from the stale paths is a silent no-op.
	c.NoError(s.Fail(ctx, row.ID, "late failure"), "Fail after Complete")
	c.NoError(s.Requeue(ctx, []int64{row.ID}), "Requeue after Complete")
	c.NoError(s.MarkSubmitting(ctx, []int64{row.ID}), "MarkSubmitting after Complete")
	c.NoError(s.MarkSubmitted(ctx, []int64{row.ID}, "batch-late"), "MarkSubmitted after Complete")
	c.NoError(s.Complete(ctx, row.ID, json.RawMessage(`{"id":"late"}`)), "Complete again")

	got, ok, err = s.Live(ctx, "conv-1")
	c.False(err != nil || !ok, "Live after stale writes: %v %v", got, err)
	c.False(got.State != batch.StateCompleted || got.ProviderBatchID != "batch-9" ||
		string(got.Response) != `{"id":"msg_1"}` || got.Error != "", "stale writes clobbered the completed row: %+v", got)

	// Tombstone is idempotent: the second one is also a no-op.
	c.NoError(s.Tombstone(ctx, row.ID), "Tombstone")
	c.NoError(s.Tombstone(ctx, row.ID), "second Tombstone")
	c.NoError(s.Fail(ctx, row.ID, "after tombstone"), "Fail after Tombstone")
	if _, ok, _ := s.Live(ctx, "conv-1"); ok {
		t.Fatal("Live after Tombstone: row visible")
	}
	var n int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.batch_call WHERE custom_id = 'conv-1'`).Scan(&n), "count")
	c.Eq(1, n, "expected exactly one tombstoned row, found")
}
