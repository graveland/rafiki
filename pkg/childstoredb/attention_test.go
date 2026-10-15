// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// attentionRow is the durable projection of the two operator attention columns,
// compared with EqDeep so a mismatch dumps both fields.
type attentionRow struct {
	PendingQuestion string
	LastTurnError   string
}

func attentionOf(rec childstore.ChildRecord) attentionRow {
	return attentionRow{PendingQuestion: rec.PendingQuestion, LastTurnError: rec.LastTurnError}
}

// attentionRec builds a minimal live record for a per-run-unique child id. The
// caller registers the t.Cleanup that tombstones it; the shared pg-test
// database is not reset between runs, so ids must be unique per run.
func attentionRec(id string) childstore.ChildRecord {
	return childstore.ChildRecord{
		ChildID:   id,
		Kind:      protocol.KindFundi,
		Status:    string(protocol.StatusIdle),
		SpawnedAt: time.Now(),
	}
}

func attentionID(prefix string) string {
	return prefix + "_" + time.Now().Format("150405.000000000")
}

// TestAttentionFieldsRoundTrip writes a record with both fields set and reads
// it back through Upsert + List; the SELECT list and the scan are in the
// statement if this passes.
func TestAttentionFieldsRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := attentionID("c_attention_rt")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	want := attentionRow{
		PendingQuestion: "Which migration should I run first?",
		LastTurnError:   "context deadline exceeded",
	}
	rec := attentionRec(id)
	rec.PendingQuestion = want.PendingQuestion
	rec.LastTurnError = want.LastTurnError

	c.Require().NoError(s.Upsert(ctx, rec), "Upsert")
	c.EqDeep(want, attentionOf(findRecord(t, s, id)), "round trip")
}

// TestAttentionFieldsEmptyStoresNull writes empty strings and asserts the
// columns are SQL NULL (not ”), and that they read back as the empty string —
// the NULL ⇄ "" contract shared with result.
func TestAttentionFieldsEmptyStoresNull(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := attentionID("c_attention_null")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := attentionRec(id)
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert empty")

	var pqNull, lteNull bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT pending_question IS NULL, last_turn_error IS NULL
		   FROM conversations.child WHERE child_id = $1`, id).Scan(&pqNull, &lteNull),
		"read NULLness")
	c.True(pqNull, "pending_question is not SQL NULL for an empty value")
	c.True(lteNull, "last_turn_error is not SQL NULL for an empty value")

	c.EqDeep(attentionRow{}, attentionOf(findRecord(t, s, id)), "empty reads back as empty")
}

// TestAttentionFieldsClearedByUpsert writes set values, then writes empty, and
// asserts the columns are cleared. This is the write side of "cleared when the
// next turn starts": a plain assignment, no COALESCE.
func TestAttentionFieldsClearedByUpsert(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := attentionID("c_attention_clear")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := attentionRec(id)
	rec.PendingQuestion = "still waiting?"
	rec.LastTurnError = "boom"
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert set")
	c.EqDeep(attentionRow{PendingQuestion: "still waiting?", LastTurnError: "boom"},
		attentionOf(findRecord(t, s, id)), "after set")

	// Same record, cleared. An unrelated field also changes, so this is an
	// ordinary later write rather than a targeted clear.
	rec.PendingQuestion = ""
	rec.LastTurnError = ""
	rec.Status = string(protocol.StatusStreaming)
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert cleared")
	c.EqDeep(attentionRow{}, attentionOf(findRecord(t, s, id)), "after clear")

	var pqNull, lteNull bool
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT pending_question IS NULL, last_turn_error IS NULL
		   FROM conversations.child WHERE child_id = $1`, id).Scan(&pqNull, &lteNull),
		"read NULLness after clear")
	c.True(pqNull, "pending_question not NULL after clear")
	c.True(lteNull, "last_turn_error not NULL after clear")
}
