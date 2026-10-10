// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// The compaction tests share one disposable database and never assert on global
// counts: each builds its own conversation.

func assistantMessage(text string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)},
	}
}

func assistantToolUse(id string) anthropic.MessageParam {
	return anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(id, map[string]any{}, "some_tool"))
}

// appendRows appends n user messages at ordinals 0..n-1.
func appendRows(t *testing.T, pool *pgxpool.Pool, convID string, n int) {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	for i := range n {
		c.NoError(NewMessages(pool).Append(ctx, convID, i, userMessage("row"), nil), "append row %d", i)
	}
}

// rowOrdinals returns the conversation's ordinals in order.
func rowOrdinals(t *testing.T, pool *pgxpool.Pool, convID string) []int {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(context.Background(), `
		SELECT ordinal FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid ORDER BY ordinal`, convID)
	c.NoError(err, "query ordinals")
	defer rows.Close()
	var out []int
	for rows.Next() {
		var o int
		c.NoError(rows.Scan(&o), "scan ordinal")
		out = append(out, o)
	}
	c.NoError(rows.Err(), "rows err")
	return out
}

// rowKind returns the kind column of the row at ordinal.
func rowKind(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int) *string {
	t.Helper()
	var kind *string
	err := pool.QueryRow(context.Background(), `
		SELECT kind FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND ordinal = $2`, convID, ordinal).Scan(&kind)
	assert.NewAborting(t).NoError(err, "query kind at %d", ordinal)
	return kind
}

// horizon reads resume_from_ordinal; the bool is false when it is NULL.
func horizon(t *testing.T, pool *pgxpool.Pool, convID string) (int, bool) {
	t.Helper()
	var h *int
	err := pool.QueryRow(context.Background(), `
		SELECT resume_from_ordinal FROM conversations.conversation WHERE id = $1::uuid`, convID).Scan(&h)
	assert.NewAborting(t).NoError(err, "query horizon")
	if h == nil {
		return 0, false
	}
	return *h, true
}

func rowCount(t *testing.T, pool *pgxpool.Pool, convID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM conversations.conversation_message WHERE conversation_id = $1::uuid`, convID).Scan(&n)
	assert.NewAborting(t).NoError(err, "count rows")
	return n
}

// TestCompactionLoadWorkingFiltersAtHorizon: LoadWorking returns only the
// summary and its tail copies, while Load still returns the full history.
func TestCompactionLoadWorkingFiltersAtHorizon(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 6)

	tail := []Message{
		{Param: userMessage("tail user")},
		{Param: assistantMessage("tail assistant"), StopReason: "end_turn"},
	}
	summaryOrdinal, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("the summary"), 111, tail)
	c.NoError(err, "AppendCompaction")
	c.Eq(6, summaryOrdinal, "summary ordinal")

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	c.Len(working, 3, "working set")
	c.EqDeep([]int{6, 7, 8}, []int{working[0].Ordinal, working[1].Ordinal, working[2].Ordinal}, "working ordinals")

	full, err := NewMessages(pool).Load(ctx, conv)
	c.NoError(err, "Load")
	c.Len(full, 9, "full history")
}

// TestCompactionLoadWorkingWithoutHorizonEqualsLoad: with no compaction the
// horizon is NULL (0) and the two reads are identical.
func TestCompactionLoadWorkingWithoutHorizonEqualsLoad(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 4)

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	full, err := NewMessages(pool).Load(ctx, conv)
	c.NoError(err, "Load")
	c.EqDeep(full, working, "working set with no horizon")
	c.Len(working, 4, "rows")
}

// TestCompactionAppendWritesSummaryTailAndHorizon: contiguous ordinals from
// max+1, kinds tagged, horizon at the summary.
func TestCompactionAppendWritesSummaryTailAndHorizon(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 3)

	tail := []Message{
		{Param: userMessage("a")},
		{Param: assistantMessage("b"), StopReason: "end_turn"},
	}
	summaryOrdinal, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("summary"), 500, tail)
	c.NoError(err, "AppendCompaction")
	c.Eq(3, summaryOrdinal, "summary ordinal")

	c.EqDeep([]int{0, 1, 2, 3, 4, 5}, rowOrdinals(t, pool, conv), "ordinals contiguous from max+1")
	c.Eq(KindCompactionSummary, *rowKind(t, pool, conv, 3), "summary kind")
	c.Eq(KindCompactionTail, *rowKind(t, pool, conv, 4), "tail 0 kind")
	c.Eq(KindCompactionTail, *rowKind(t, pool, conv, 5), "tail 1 kind")

	h, ok := horizon(t, pool, conv)
	c.False(!ok || h != summaryOrdinal, "horizon = (%d,%v), want %d", h, ok, summaryOrdinal)
}

// TestCompactionTailCopiesKeepStopReasonAndNullTokens: a tail copy carries the
// source row's stop reason and leaves its tokens NULL — a zero cache_write must
// stay distinguishable from "not reported".
func TestCompactionTailCopiesKeepStopReasonAndNullTokens(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 1)

	tail := []Message{{Param: assistantMessage("assistant"), StopReason: "end_turn"}}
	_, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("summary"), 42, tail)
	c.NoError(err, "AppendCompaction")

	var (
		stopReason    *string
		inTok, outTok *int
	)
	err = pool.QueryRow(ctx, `
		SELECT stop_reason, input_tokens, output_tokens FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND ordinal = 2`, conv).Scan(&stopReason, &inTok, &outTok)
	c.NoError(err, "query tail row")
	c.False(stopReason == nil || *stopReason != "end_turn", "tail stop_reason = %v, want end_turn", stopReason)
	c.False(inTok != nil, "tail input_tokens = %v, want NULL", inTok)
	c.False(outTok != nil, "tail output_tokens = %v, want NULL", outTok)
}

// TestCompactionSummaryCarriesReplacedTokens: the summary row's input_tokens is
// the replacedTokens argument.
func TestCompactionSummaryCarriesReplacedTokens(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)

	summaryOrdinal, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("summary"), 182000, nil)
	c.NoError(err, "AppendCompaction")

	var inTok *int
	c.NoError(pool.QueryRow(ctx, `
		SELECT input_tokens FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND ordinal = $2`, conv, summaryOrdinal).Scan(&inTok), "query summary")
	c.False(inTok == nil || *inTok != 182000, "summary input_tokens = %v, want 182000", inTok)
}

// TestCompactionEmptyTail: a nil tail writes exactly one row and moves the
// horizon to it.
func TestCompactionEmptyTail(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)

	summaryOrdinal, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("summary"), 10, nil)
	c.NoError(err, "AppendCompaction")
	c.Eq(2, summaryOrdinal, "summary ordinal")
	c.Eq(3, rowCount(t, pool, conv), "row count")
	h, ok := horizon(t, pool, conv)
	c.False(!ok || h != summaryOrdinal, "horizon = (%d,%v), want %d", h, ok, summaryOrdinal)
}

// TestCompactionSecondCompactionMovesHorizon: the second summary lands after
// everything including the first tail copies, and LoadWorking returns only the
// newest boundary.
func TestCompactionSecondCompactionMovesHorizon(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 3)

	first, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("first"), 100, []Message{
		{Param: userMessage("f1")}, {Param: userMessage("f2")},
	})
	c.NoError(err, "first AppendCompaction")
	c.Eq(3, first, "first summary ordinal")

	second, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("second"), 200, []Message{
		{Param: userMessage("s1")},
	})
	c.NoError(err, "second AppendCompaction")
	c.Eq(6, second, "second summary ordinal")

	h, ok := horizon(t, pool, conv)
	c.False(!ok || h != second, "horizon = (%d,%v), want %d", h, ok, second)

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	c.Len(working, 2, "working set")
	c.EqDeep([]int{6, 7}, []int{working[0].Ordinal, working[1].Ordinal}, "working ordinals")
	c.Eq(KindCompactionSummary, *working[0].Kind, "working[0] kind")
}

// TestCompactionFencedLeaseLostWritesNothing: a stale holder writes nothing and
// the horizon is unchanged. Delete the EXISTS clause from the fenced insert and
// this test must fail.
func TestCompactionFencedLeaseLostWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	c.False(err != nil || !ok, "acquire stale: ok=%v err=%v", ok, err)
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}

	_, err = NewMessages(pool).WithLease(stale).AppendCompaction(ctx, conv, userMessage("summary"), 50, []Message{
		{Param: userMessage("tail")},
	})
	c.ErrorIs(err, ErrLeaseLost, "AppendCompaction error")
	c.Eq(2, rowCount(t, pool, conv), "row count after refused compaction")
	_, ok = horizon(t, pool, conv)
	c.False(ok, "horizon was set by a refused compaction")
}

// TestCompactionFencedLeaseHeldWrites: with a live lease the guard is invisible.
func TestCompactionFencedLeaseHeldWrites(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 1)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.False(err != nil || !ok, "acquire: ok=%v err=%v", ok, err)

	summaryOrdinal, err := NewMessages(pool).WithLease(lease).AppendCompaction(ctx, conv, userMessage("summary"), 5, []Message{
		{Param: userMessage("tail")},
	})
	c.NoError(err, "AppendCompaction with a live lease")
	c.Eq(1, summaryOrdinal, "summary ordinal")
	c.Eq(3, rowCount(t, pool, conv), "row count")
}

// TestCompactionUnfinishedOrphansUnchanged: compaction appends a copy of an
// unresolved assistant tool_use row but never removes the original, so
// orphan recovery still sees the conversation with exactly that one id
// (DISTINCT collapses the copy).
func TestCompactionUnfinishedOrphansUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	orphan := "orphan-" + conv
	c.NoError(NewMessages(pool).Append(ctx, conv, 0, assistantToolUse(orphan), nil), "append assistant tool_use")

	_, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("summary"), 7, []Message{
		{Param: assistantToolUse(orphan), StopReason: "tool_use"},
	})
	c.NoError(err, "AppendCompaction")

	unfinished, err := UnfinishedConversations(ctx, pool, DrivenByServer)
	c.NoError(err, "UnfinishedConversations")
	var found *Unfinished
	for i := range unfinished {
		if unfinished[i].ConversationID == conv {
			found = &unfinished[i]
			break
		}
	}
	c.False(found == nil, "conversation %s not reported as unfinished", conv)
	c.EqDeep([]string{orphan}, found.OrphanToolUses, "orphan ids")
}

// TestCompactionAppendUnknownConversation: an unknown conversation is refused
// and nothing is written.
func TestCompactionAppendUnknownConversation(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()

	var unknown string
	c.NoError(pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&unknown), "random uuid")

	_, err := NewMessages(pool).AppendCompaction(ctx, unknown, userMessage("summary"), 1, nil)
	c.Error(err, "AppendCompaction on unknown conversation")
	c.StrContains(err.Error(), "not found", "error")
	c.Eq(0, rowCount(t, pool, unknown), "rows written for an unknown conversation")
}

// TestCompactionLoadIsUnfiltered: Load stays FULL history after compaction,
// including the pre-compaction rows, in ordinal order.
func TestCompactionLoadIsUnfiltered(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 3)

	_, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("summary"), 9, []Message{
		{Param: userMessage("t1")}, {Param: assistantMessage("t2"), StopReason: "end_turn"},
	})
	c.NoError(err, "AppendCompaction")

	full, err := NewMessages(pool).Load(ctx, conv)
	c.NoError(err, "Load")
	c.EqDeep([]int{0, 1, 2, 3, 4, 5}, rowOrdinals(t, pool, conv), "all ordinals in order")
	c.Len(full, 6, "Load returns full history")
	c.Eq(0, full[0].Ordinal, "first row is the pre-compaction row 0")
	// The pre-compaction rows are still ordinary (untagged) rows.
	c.False(full[0].Kind != nil, "pre-compaction row kind = %v, want nil", full[0].Kind)
}

// TestAppendClearWritesBoundaryAndMovesHorizon: a clear appends one kind='clear'
// row, moves the horizon onto it, touches nothing else, and the working set is
// exactly that row.
func TestAppendClearWritesBoundaryAndMovesHorizon(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 3)

	ord, err := NewMessages(pool).AppendClear(ctx, conv, userMessage(ClearBoundaryText))
	c.NoError(err, "AppendClear")
	c.Eq(3, ord, "boundary ordinal")
	c.Eq(4, rowCount(t, pool, conv), "no row is deleted or duplicated")
	k := rowKind(t, pool, conv, ord)
	c.Require().NotNil(k, "boundary kind")
	c.Eq(KindClear, *k, "boundary kind")
	h, ok := horizon(t, pool, conv)
	c.False(!ok || h != ord, "horizon = (%d,%v), want %d", h, ok, ord)

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	c.Require().Len(working, 1, "working set")
	c.True(IsClearBoundary(working[0]), "the working set must be the synthetic boundary row")
}

// TestAppendClearFencedLeaseLostWritesNothing mirrors the compaction fence.
func TestAppendClearFencedLeaseLostWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	c.False(err != nil || !ok, "acquire stale: ok=%v err=%v", ok, err)
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}

	_, err = NewMessages(pool).WithLease(stale).AppendClear(ctx, conv, userMessage(ClearBoundaryText))
	c.ErrorIs(err, ErrLeaseLost, "AppendClear error")
	c.Eq(2, rowCount(t, pool, conv), "a refused clear writes nothing")
	_, ok = horizon(t, pool, conv)
	c.False(ok, "a refused clear must not move the horizon")
}
