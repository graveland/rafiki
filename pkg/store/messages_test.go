// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"
	"time"

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

	// created_at is what a durable event's ts reports, so every row Load
	// returns must carry a real instant -- a zero here would put the event at
	// year 1 and page it behind every other row.
	for i, m := range msgs {
		c.False(m.CreatedAt.IsZero(), "message %d has a zero CreatedAt", i)
	}
}

// pageFixture writes one conversation whose rows carry chosen created_at
// values (offsets in seconds from a base), so a test controls exactly which
// rows share a timestamp. kinds, when non-empty at an index, sets that row's
// kind.
func pageFixture(t *testing.T, offsets []int, kinds map[int]string) (*Messages, string, time.Time) {
	t.Helper()
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	var convID string
	c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID))
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i, off := range offsets {
		var kind *string
		if k, ok := kinds[i]; ok {
			kind = &k
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, kind, created_at)
			VALUES ($1::uuid, $2, 'user', '[{"type":"text","text":"x"}]'::jsonb, $3, $4)`,
			convID, i, kind, base.Add(time.Duration(off)*time.Second))
		c.NoError(err, "insert row %d", i)
	}
	return NewMessages(pool), convID, base
}

func pageOrdinals(t *testing.T, m *Messages, convID string, page HistoryPage) []int {
	t.Helper()
	msgs, err := m.LoadPage(context.Background(), convID, page)
	assert.NewAborting(t).NoError(err, "LoadPage")
	out := []int{}
	for _, msg := range msgs {
		out = append(out, msg.Ordinal)
	}
	return out
}

func TestLoadPageBounds(t *testing.T) {
	c := assert.NewAborting(t)
	// Rows 0 and 1 share a timestamp; 2 and 3 are later and distinct.
	m, convID, base := pageFixture(t, []int{0, 0, 1, 2}, nil)
	before := base.Add(2 * time.Second)

	c.EqDeep([]int{0, 1, 2, 3}, pageOrdinals(t, m, convID, HistoryPage{}), "no bounds")
	c.EqDeep([]int{3}, pageOrdinals(t, m, convID, HistoryPage{Limit: ptr(1)}), "limit keeps the newest")
	c.EqDeep([]int{2, 3}, pageOrdinals(t, m, convID, HistoryPage{AfterOrdinal: ptr(1)}), "after_ordinal is strict")
	c.EqDeep([]int{0, 1, 2}, pageOrdinals(t, m, convID, HistoryPage{Before: &before}), "before is strict")
	c.EqDeep([]int{2}, pageOrdinals(t, m, convID, HistoryPage{Before: &before, Limit: ptr(1)}), "before then limit")
	c.EqDeep([]int{0, 1, 2, 3}, pageOrdinals(t, m, convID, HistoryPage{Limit: ptr(10)}), "a limit above the row count cuts nothing")
}

// A cut that would fall inside a same-created_at run widens back to the start
// of that run, and only that run: a matching timestamp further back, past a
// row with a different one, is not part of the group.
func TestLoadPageLimitNeverSplitsATimestampGroup(t *testing.T) {
	c := assert.NewAborting(t)
	m, convID, _ := pageFixture(t, []int{0, 0, 1}, nil)
	c.EqDeep([]int{0, 1, 2}, pageOrdinals(t, m, convID, HistoryPage{Limit: ptr(2)}), "the t0 group stays whole")

	m, convID, _ = pageFixture(t, []int{0, 1, 0}, nil)
	c.EqDeep([]int{2}, pageOrdinals(t, m, convID, HistoryPage{Limit: ptr(1)}), "a non-adjacent equal ts is not widened to")
}

// compaction_tail rows render no event, so they are neither returned nor
// counted toward the limit.
func TestLoadPageSkipsCompactionTailRows(t *testing.T) {
	c := assert.NewAborting(t)
	m, convID, _ := pageFixture(t, []int{0, 1, 2}, map[int]string{1: KindCompactionTail})
	c.EqDeep([]int{0, 2}, pageOrdinals(t, m, convID, HistoryPage{}), "no bounds")
	c.EqDeep([]int{0, 2}, pageOrdinals(t, m, convID, HistoryPage{Limit: ptr(2)}), "the tail row does not count")
}

// Paging backwards with the oldest received created_at as the next Before
// visits every row exactly once, groups included.
func TestLoadPagePagingBackwardsVisitsEveryRowOnce(t *testing.T) {
	c := assert.NewAborting(t)
	m, convID, _ := pageFixture(t, []int{0, 1, 1, 1, 2, 3, 3, 4, 5, 5}, nil)
	var seen []int
	var before *time.Time
	for range 20 {
		msgs, err := m.LoadPage(context.Background(), convID, HistoryPage{Before: before, Limit: ptr(2)})
		c.NoError(err, "LoadPage")
		if len(msgs) == 0 {
			break
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			seen = append(seen, msgs[i].Ordinal)
		}
		oldest := msgs[0].CreatedAt
		before = &oldest
	}
	c.EqDeep([]int{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}, seen, "every row once, newest first")
}

func TestLoadPageRefusesNonPositiveLimit(t *testing.T) {
	c := assert.NewAborting(t)
	m, convID, _ := pageFixture(t, []int{0}, nil)
	for _, n := range []int{0, -1} {
		_, err := m.LoadPage(context.Background(), convID, HistoryPage{Limit: ptr(n)})
		c.Error(err, "limit %d", n)
	}
}

func ptr[T any](v T) *T { return &v }
