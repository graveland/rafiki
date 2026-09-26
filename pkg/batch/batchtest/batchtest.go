// SPDX-License-Identifier: Apache-2.0

// Package batchtest is the Store conformance suite: both the in-memory store
// (pkg/batch.MemStore) and the Postgres store (pkg/batchdb) run it. It pins
// the contract the Batcher relies on — unique live custom_id, tombstones
// freeing the id, every transition's source state, InState excluding
// tombstoned rows, and the refusal of empty or unknown states.
package batchtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/batch"
)

// StoreMaker returns a fresh, empty store for one subtest.
type StoreMaker func(t *testing.T) batch.Store

// Run exercises the Store contract against the store newStore returns. Every
// subtest starts from a fresh store, so implementations need not reset state
// between runs.
func Run(t *testing.T, newStore StoreMaker) {
	t.Helper()
	t.Run("insert and live round trip", func(t *testing.T) { testInsertAndLive(t, newStore(t)) })
	t.Run("insert refuses non-queued state", func(t *testing.T) { testInsertRefusesNonQueued(t, newStore(t)) })
	t.Run("live custom_id is unique", func(t *testing.T) { testUniqueLiveCustomID(t, newStore(t)) })
	t.Run("tombstone frees the custom_id", func(t *testing.T) { testTombstoneFreesID(t, newStore(t)) })
	t.Run("submit transitions", func(t *testing.T) { testSubmitTransitions(t, newStore(t)) })
	t.Run("requeue returns submitting rows to queued", func(t *testing.T) { testRequeue(t, newStore(t)) })
	t.Run("fail from submitting and submitted", func(t *testing.T) { testFail(t, newStore(t)) })
	t.Run("in state excludes tombstoned rows", func(t *testing.T) { testInStateExcludesTombstoned(t, newStore(t)) })
	t.Run("in state refuses empty and unknown state", func(t *testing.T) { testInStateRefusesUnknown(t, newStore(t)) })
	t.Run("timestamps stamped and bumped", func(t *testing.T) { testTimestamps(t, newStore(t)) })
}

func testInsertAndLive(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	row, err := s.Insert(ctx, batch.Row{
		CustomID: "conv-1",
		Model:    "vendor/m:batch",
		State:    batch.StateQueued,
		Request:  json.RawMessage(`{"max_tokens":16}`),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if row.ID == 0 {
		t.Fatal("Insert returned a zero id")
	}
	got, ok, err := s.Live(ctx, "conv-1")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if !ok {
		t.Fatal("Live: row not found")
	}
	if got.ID != row.ID || got.State != batch.StateQueued || got.Model != "vendor/m:batch" {
		t.Errorf("Live: got %+v", got)
	}
	if string(got.Request) != `{"max_tokens":16}` {
		t.Errorf("Live: Request = %s", got.Request)
	}
	if _, ok, _ := s.Live(ctx, "conv-other"); ok {
		t.Error("Live: unknown custom_id returned a row")
	}
}

func testInsertRefusesNonQueued(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	for _, st := range []batch.State{
		"",
		batch.StateSubmitting,
		batch.StateSubmitted,
		batch.StateCompleted,
		batch.StateFailed,
		batch.State("nope"),
	} {
		if _, err := s.Insert(ctx, batch.Row{CustomID: "conv-" + string(st), State: st}); err == nil {
			t.Errorf("Insert with state %q: expected an error", st)
		}
	}
}

func testUniqueLiveCustomID(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued}); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if _, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued}); err == nil {
		t.Fatal("second Insert with the same live custom_id: expected an error")
	}
}

func testTombstoneFreesID(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	first, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := s.Tombstone(ctx, first.ID); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if _, ok, _ := s.Live(ctx, "conv-1"); ok {
		t.Fatal("Live after Tombstone: row still visible")
	}
	second, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("second Insert after Tombstone: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("second Insert reused the tombstoned row's id")
	}
}

func testSubmitTransitions(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	row, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.MarkSubmitting(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("MarkSubmitting: %v", err)
	}
	got, _, _ := s.Live(ctx, "conv-1")
	if got.State != batch.StateSubmitting {
		t.Fatalf("after MarkSubmitting: state = %q", got.State)
	}
	if err := s.MarkSubmitted(ctx, []int64{row.ID}, "batch-9"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	got, _, _ = s.Live(ctx, "conv-1")
	if got.State != batch.StateSubmitted || got.ProviderBatchID != "batch-9" {
		t.Fatalf("after MarkSubmitted: state = %q, batch = %q", got.State, got.ProviderBatchID)
	}
	if err := s.Complete(ctx, row.ID, json.RawMessage(`{"id":"msg_1"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, _, _ = s.Live(ctx, "conv-1")
	if got.State != batch.StateCompleted || string(got.Response) != `{"id":"msg_1"}` {
		t.Fatalf("after Complete: state = %q, response = %s", got.State, got.Response)
	}
}

func testRequeue(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	row, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued, Request: json.RawMessage(`{"max_tokens":16}`)})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.MarkSubmitting(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("MarkSubmitting: %v", err)
	}
	if err := s.Requeue(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	got, _, _ := s.Live(ctx, "conv-1")
	if got.State != batch.StateQueued {
		t.Fatalf("after Requeue: state = %q", got.State)
	}
	if got.ProviderBatchID != "" {
		t.Errorf("after Requeue: ProviderBatchID = %q", got.ProviderBatchID)
	}
	if string(got.Request) != `{"max_tokens":16}` {
		t.Errorf("after Requeue: Request = %s", got.Request)
	}
	// The row is queued again: a fresh submit window picks it up and the
	// transitions replay.
	if err := s.MarkSubmitting(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("second MarkSubmitting: %v", err)
	}
	got, _, _ = s.Live(ctx, "conv-1")
	if got.State != batch.StateSubmitting {
		t.Fatalf("after second MarkSubmitting: state = %q", got.State)
	}
}

func testFail(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	fromSubmitting, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	fromSubmitted, err := s.Insert(ctx, batch.Row{CustomID: "conv-2", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("second Insert: %v", err)
	}
	if err := s.MarkSubmitting(ctx, []int64{fromSubmitting.ID, fromSubmitted.ID}); err != nil {
		t.Fatalf("MarkSubmitting: %v", err)
	}
	if err := s.MarkSubmitted(ctx, []int64{fromSubmitted.ID}, "batch-9"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}
	if err := s.Fail(ctx, fromSubmitting.ID, "rate limited"); err != nil {
		t.Fatalf("Fail from submitting: %v", err)
	}
	if err := s.Fail(ctx, fromSubmitted.ID, "batch failed: expired"); err != nil {
		t.Fatalf("Fail from submitted: %v", err)
	}
	got, _, _ := s.Live(ctx, "conv-1")
	if got.State != batch.StateFailed || got.Error != "rate limited" {
		t.Errorf("row failed from submitting: state = %q, error = %q", got.State, got.Error)
	}
	got, _, _ = s.Live(ctx, "conv-2")
	if got.State != batch.StateFailed || got.Error != "batch failed: expired" {
		t.Errorf("row failed from submitted: state = %q, error = %q", got.State, got.Error)
	}
}

func testInStateExcludesTombstoned(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	kept, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	gone, err := s.Insert(ctx, batch.Row{CustomID: "conv-2", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("second Insert: %v", err)
	}
	if err := s.Tombstone(ctx, gone.ID); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	rows, err := s.InState(ctx, batch.StateQueued)
	if err != nil {
		t.Fatalf("InState: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != kept.ID {
		t.Fatalf("InState(queued) = %+v, want only row %d", rows, kept.ID)
	}
}

func testInStateRefusesUnknown(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	for _, st := range []batch.State{"", batch.State("nope")} {
		if _, err := s.InState(ctx, st); err == nil {
			t.Errorf("InState(%q): expected an error", st)
		}
	}
}

func testTimestamps(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	row, err := s.Insert(ctx, batch.Row{CustomID: "conv-1", State: batch.StateQueued})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, _, _ := s.Live(ctx, "conv-1")
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("Insert did not stamp CreatedAt/UpdatedAt")
	}
	before := got.UpdatedAt
	if err := s.MarkSubmitting(ctx, []int64{row.ID}); err != nil {
		t.Fatalf("MarkSubmitting: %v", err)
	}
	got, _, _ = s.Live(ctx, "conv-1")
	if got.UpdatedAt.Before(before) {
		t.Errorf("MarkSubmitting moved UpdatedAt backwards: %v < %v", got.UpdatedAt, before)
	}
	if got.UpdatedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("UpdatedAt is in the future: %v", got.UpdatedAt)
	}
}
