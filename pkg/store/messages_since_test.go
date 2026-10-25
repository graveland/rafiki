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
