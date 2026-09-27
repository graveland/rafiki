// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/multigres/testkit/assert"
)

// Append's conflict semantics: an identical replay is a no-op, diverging
// content at an existing ordinal is a loud error (never a silent fork).
func TestAppendReplayAndDivergence(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID))
	m := NewMessages(pool)
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))

	c.NoError(m.Append(ctx, convID, 0, msg, nil), "first append")
	// Identical replay: no-op, no error, still one row.
	c.NoError(m.Append(ctx, convID, 0, msg, nil), "replay append")
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid`, convID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d err=%v, want 1", n, err)
	}
	// Diverging content at the same ordinal: loud error.
	err := m.Append(ctx, convID, 0, anthropic.NewUserMessage(anthropic.NewTextBlock("DIFFERENT")), nil)
	c.False(err == nil || !strings.Contains(err.Error(), "diverged"), "diverged append err = %v, want history-diverged error", err)
}

// Kind/InputTokens round-trip through Load. A compaction_summary row is
// written by raw SQL (pkg/capture's boundary-write path), not by Append, and
// Load must return it alongside ordinary rows with both new fields populated;
// ordinary rows round-trip with both nil. Load stays FULL, unfiltered history
// — the constraint guards the WHERE clause, this test guards what comes back.
func TestLoadRoundTripsKindAndInputTokens(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID))

	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
		VALUES ($1::uuid, 0, 'user', '[{"type":"text","text":"hello"}]'::jsonb)`, convID)
	c.NoError(err)
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, kind, input_tokens)
		VALUES ($1::uuid, 1, 'user', '[{"type":"text","text":"summary"}]'::jsonb, 'compaction_summary', 182000)`, convID)
	c.NoError(err)

	msgs, err := NewMessages(pool).Load(ctx, convID)
	c.NoError(err)
	c.Len(msgs, 2, "got %d messages, want 2 (Load returns FULL history)", len(msgs))

	ord := msgs[0]
	c.False(ord.Kind != nil || ord.InputTokens != nil, "ordinary row: kind=%v input_tokens=%v, want both nil", ord.Kind, ord.InputTokens)

	comp := msgs[1]
	c.False(comp.Kind == nil || *comp.Kind != "compaction_summary", "compaction row kind = %v, want compaction_summary", comp.Kind)
	c.False(comp.InputTokens == nil || *comp.InputTokens != 182000, "compaction row input_tokens = %v, want 182000", comp.InputTokens)
	// The summary row is kept API-valid as Claude Code's own user message.
	c.Eq("user", string(comp.Param.Role), "compaction row role = %q, want user", comp.Param.Role)
}
