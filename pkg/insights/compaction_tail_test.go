// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// insertKindMessage inserts a conversation_message row with an explicit kind
// (compaction_summary / compaction_tail), which insertMessage does not cover.
func insertKindMessage(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int, role, kind, contentJSON string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content, kind)
		 VALUES ($1, $2, $3, $4, $5)`,
		convID, ordinal, role, []byte(contentJSON), kind)
	assert.NewAborting(t).NoError(err, "insert kind message")
}

// TestExportSkipsCompactionTail pins that conversation_export drops
// compaction_tail copies: the verbatim tail row duplicates an earlier row, so
// exporting it would show the same content twice.
func TestExportSkipsCompactionTail(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "carol")
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"prompt-1"}]`)
	insertMessage(t, pool, convID, 1, "assistant", `[{"type":"text","text":"reply-1"}]`)
	insertMessage(t, pool, convID, 2, "user", `[{"type":"text","text":"prompt-2"}]`)
	insertKindMessage(t, pool, convID, 3, "user", store.KindCompactionSummary, `[{"type":"text","text":"summary"}]`)
	insertKindMessage(t, pool, convID, 4, "user", store.KindCompactionTail, `[{"type":"text","text":"prompt-2"}]`)

	tr, err := New(pool).Export(ctx, ScopeAll(), convID)
	c.NoError(err, "export")
	var prompt2 int
	for _, turn := range tr.Turns {
		if strings.Contains(string(turn.Content), "prompt-2") {
			prompt2++
		}
	}
	c.Eq(1, prompt2, "prompt-2 must appear once; the tail copy must be skipped")
}

// TestQueryToolsSkipsCompactionTail pins that the tools query does not
// double-count a tool_use the tail copied.
func TestQueryToolsSkipsCompactionTail(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "carol")
	insertMessage(t, pool, convID, 0, "assistant", `[{"type":"tool_use","id":"u-1","name":"bash","input":{}}]`)
	insertKindMessage(t, pool, convID, 1, "assistant", store.KindCompactionTail,
		`[{"type":"tool_use","id":"u-1","name":"bash","input":{}}]`)

	got, err := New(pool).Query(ctx, ScopeAll(), "tools", StatsFilter{})
	c.NoError(err, "query tools")
	c.Require().Len(got.Rows, 1, "tools rows = %d, want 1", len(got.Rows))
	calls, ok := got.Rows[0][1].(IntEntry)
	c.True(ok, "calls entry type = %T", got.Rows[0][1])
	c.Eq(int64(1), int64(calls), "calls must count the tool_use once, not once per tail copy")
}

// TestQuerySkillsSkipsCompactionTail pins the same for the skills query.
func TestQuerySkillsSkipsCompactionTail(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "carol")
	insertMessage(t, pool, convID, 0, "assistant",
		`[{"type":"tool_use","name":"Skill","input":{"skill":"brainstorming"}}]`)
	insertKindMessage(t, pool, convID, 1, "assistant", store.KindCompactionTail,
		`[{"type":"tool_use","name":"Skill","input":{"skill":"brainstorming"}}]`)

	got, err := New(pool).Query(ctx, ScopeAll(), "skills", StatsFilter{})
	c.NoError(err, "query skills")
	c.Require().Len(got.Rows, 1, "skills rows = %d, want 1", len(got.Rows))
	n, ok := got.Rows[0][1].(IntEntry)
	c.True(ok, "invocations entry type = %T", got.Rows[0][1])
	c.Eq(int64(1), int64(n), "invocations must count the skill once, not once per tail copy")
}

// TestExportKeepsSummaryRow is a belt-and-braces check that the summary row
// (which is NOT skipped, only the tail is) still exports, so the tail filter
// cannot be satisfied by dropping everything.
func TestExportKeepsSummaryRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "carol")
	insertKindMessage(t, pool, convID, 0, "user", store.KindCompactionSummary, `[{"type":"text","text":"the summary"}]`)

	tr, err := New(pool).Export(ctx, ScopeAll(), convID)
	c.NoError(err, "export")
	c.Require().Len(tr.Turns, 1, "the summary row must still export")
	var content []map[string]any
	c.NoError(json.Unmarshal(tr.Turns[0].Content, &content), "summary content")
}
