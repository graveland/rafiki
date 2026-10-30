// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestLoadSinceReturnsOnlyRowsAtOrAfterSince(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID))

	insert := func(ordinal int, role, at string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, created_at)
			VALUES ($1::uuid, $2, $3, '[{"type":"text","text":"x"}]'::jsonb, `+at+`)`, convID, ordinal, role)
		c.NoError(err, "insert ordinal %d", ordinal)
	}
	insert(0, "user", "now() - interval '1 hour'")
	insert(1, "assistant", "now() - interval '1 hour'")
	var since time.Time
	c.NoError(pool.QueryRow(ctx, `SELECT now()`).Scan(&since))
	insert(3, "assistant", "now()")
	insert(2, "user", "now()")

	got, err := NewMessages(pool).LoadSince(ctx, convID, since)
	c.NoError(err)
	c.Require().Len(got, 2, "rows at or after since")
	c.Eq(2, got[0].Ordinal, "ordinal order")
	c.Eq(3, got[1].Ordinal, "ordinal order")
	c.False(got[0].CreatedAt.IsZero(), "CreatedAt is set")
}

func TestLoadSinceOmitsCompactionTail(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID))
	var since time.Time
	c.NoError(pool.QueryRow(ctx, `SELECT now()`).Scan(&since))
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, kind)
		VALUES ($1::uuid, 0, 'user', '[{"type":"text","text":"a"}]'::jsonb, NULL),
		       ($1::uuid, 1, 'user', '[{"type":"text","text":"b"}]'::jsonb, 'compaction_tail'),
		       ($1::uuid, 2, 'user', '[{"type":"text","text":"c"}]'::jsonb, 'compaction_summary')`, convID)
	c.NoError(err)

	got, err := NewMessages(pool).LoadSince(ctx, convID, since)
	c.NoError(err)
	c.Require().Len(got, 2, "tail row omitted")
	c.Eq(0, got[0].Ordinal)
	c.Eq(2, got[1].Ordinal)
}

// TestLoadSinceCarriesKindAndInputTokens pins that the surviving row reader
// returns the node fields a compaction boundary needs: an ordinary row carries
// no kind and no size, a compaction_summary row carries both, and every row
// carries its created_at.
func TestLoadSinceCarriesKindAndInputTokens(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID))
	var since time.Time
	c.NoError(pool.QueryRow(ctx, `SELECT now() - interval '1 hour'`).Scan(&since))
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
		VALUES ($1::uuid, 0, 'user', '[{"type":"text","text":"hello"}]'::jsonb)`, convID)
	c.NoError(err)
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, kind, input_tokens)
		VALUES ($1::uuid, 1, 'user', '[{"type":"text","text":"summary"}]'::jsonb, 'compaction_summary', 182000)`, convID)
	c.NoError(err)

	got, err := NewMessages(pool).LoadSince(ctx, convID, since)
	c.Require().NoError(err)
	c.Require().Len(got, 2, "rows")

	ordinary := got[0]
	c.Eq("", ordinary.Kind, "an ordinary row has no kind")
	c.Nil(ordinary.InputTokens, "an ordinary row has no size")

	comp := got[1]
	c.Eq("compaction_summary", comp.Kind, "compaction row kind")
	c.Require().NotNil(comp.InputTokens, "compaction row input_tokens")
	c.Eq(int64(182000), *comp.InputTokens, "compaction row input_tokens")
	c.Eq("user", comp.Msg.Role.String(), "the summary row stays a user message")
	for i, n := range got {
		c.False(n.CreatedAt.IsZero(), "row %d has a zero CreatedAt", i)
	}
}
