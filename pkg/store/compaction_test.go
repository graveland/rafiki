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

// appendRows appends n user messages under the conversation's head.
func appendRows(t *testing.T, pool *pgxpool.Pool, convID string, n int) {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	for i := range n {
		_, aerr := NewMessages(pool).AppendNode(ctx, convID, userMessage("row"), nil, "")
		c.NoError(aerr, "append row %d", i)
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

// headID returns the conversation's head_id, "" when it has none.
func headID(t *testing.T, pool *pgxpool.Pool, convID string) string {
	t.Helper()
	var head string
	err := pool.QueryRow(context.Background(), `
		SELECT coalesce(head_id::text, '') FROM conversations.conversation
		 WHERE id = $1::uuid`, convID).Scan(&head)
	assert.NewAborting(t).NoError(err, "query head")
	return head
}

// textOf returns the text of the row at ordinal's first block.
func textOf(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int) string {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := NewMessages(pool).Load(context.Background(), convID)
	c.NoError(err, "Load")
	for _, m := range rows {
		if m.Ordinal == ordinal && len(m.Param.Content) > 0 && m.Param.Content[0].OfText != nil {
			return m.Param.Content[0].OfText.Text
		}
	}
	t.Fatalf("no row at ordinal %d", ordinal)
	return ""
}

func rowCount(t *testing.T, pool *pgxpool.Pool, convID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM conversations.conversation_message WHERE conversation_id = $1::uuid`, convID).Scan(&n)
	assert.NewAborting(t).NoError(err, "count rows")
	return n
}

// pathTexts renders the working path's text blocks, the shape the assertions
// compare.
func pathTexts(t *testing.T, pool *pgxpool.Pool, convID string) []string {
	t.Helper()
	c := assert.NewAborting(t)
	path, err := NewMessages(pool).LoadWorking(context.Background(), convID)
	c.NoError(err, "LoadWorking")
	out := make([]string, len(path))
	for i, m := range path {
		if len(m.Param.Content) > 0 && m.Param.Content[0].OfText != nil {
			out[i] = m.Param.Content[0].OfText.Text
		}
	}
	return out
}

// TestCompactionRootWithTailUnderIt pins the tree shape a compaction writes: the
// summary is a compaction_summary ROOT derived from the old head, the tail
// copies hang under it tagged compaction_tail, the working path is
// [summary, tail…], and the pre-compaction rows are untouched.
func TestCompactionRootWithTailUnderIt(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	for _, text := range []string{"a", "b", "c"} {
		_, err := NewMessages(pool).AppendNode(ctx, conv, userMessage(text), nil, "")
		c.NoError(err, "append %q", text)
	}
	a, b, before := readTreeRow(t, pool, conv, 0), readTreeRow(t, pool, conv, 1), readTreeRow(t, pool, conv, 2)

	tail := []Message{
		{Param: userMessage("b")},
		{Param: assistantMessage("c"), StopReason: "end_turn"},
	}
	summaryOrdinal, err := NewMessages(pool).AppendCompaction(ctx, conv, userMessage("the summary"), 100, tail)
	c.NoError(err, "AppendCompaction")
	c.Eq(3, summaryOrdinal, "summary ordinal")

	summary := readTreeRow(t, pool, conv, 3)
	c.Require().NotNil(summary.kind, "summary kind")
	c.Eq(KindCompactionSummary, *summary.kind, "summary kind")
	c.Eq("", summary.parent, "the summary is a root")
	c.Eq(0, summary.depth, "root depth")
	c.Eq(before.id, summary.derived, "the summary is derived from the old head")

	for i, want := range []string{"b", "c"} {
		row := readTreeRow(t, pool, conv, 4+i)
		c.Require().NotNil(row.kind, "tail %d kind", i)
		c.Eq(KindCompactionTail, *row.kind, "tail %d kind", i)
		c.Eq(1+i, row.depth, "tail %d depth", i)
		if i == 0 {
			c.Eq(summary.id, row.parent, "tail 0 hangs under the summary")
		} else {
			c.Eq(readTreeRow(t, pool, conv, 4).id, row.parent, "tail 1 hangs under tail 0")
		}
		c.Eq(want, textOf(t, pool, conv, 4+i), "tail %d content", i)
	}
	c.Eq(readTreeRow(t, pool, conv, 5).id, headID(t, pool, conv), "head is the last tail copy")

	c.EqDeep([]string{"the summary", "b", "c"}, pathTexts(t, pool, conv), "working path")

	// The pre-compaction rows are exactly as they were.
	c.EqDeep([]int{0, 1, 2, 3, 4, 5}, rowOrdinals(t, pool, conv), "ordinals")
	c.False(a.kind != nil || b.kind != nil || before.kind != nil, "pre-compaction rows keep their nil kind")
	c.Eq(a.id, b.parent, "row b still hangs under a")
	c.Eq(b.id, before.parent, "row c still hangs under b")
	c.Eq(6, rowCount(t, pool, conv), "no row was deleted or rewritten")
}

// TestCompactionWorkingPathEqualsLoadWhenLinear: a conversation with no boundary
// has one path, so the working read and the full read agree.
func TestCompactionWorkingPathEqualsLoadWhenLinear(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 4)

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	full, err := NewMessages(pool).Load(ctx, conv)
	c.NoError(err, "Load")
	c.EqDeep(full, working, "working set with no boundary")
	c.Len(working, 4, "rows")
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

// TestCompactionEmptyTail: a nil tail writes exactly one row and the working
// path is that summary alone.
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
	c.EqDeep([]string{"summary"}, pathTexts(t, pool, conv), "working path")
}

// TestCompactionSecondCompactionIsANewRoot: the second summary lands after
// everything including the first tail copies, is derived from the first
// compaction's last node, and the working path is only the newest boundary.
func TestCompactionSecondCompactionIsANewRoot(t *testing.T) {
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

	firstTail := readTreeRow(t, pool, conv, 5)
	secondSummary := readTreeRow(t, pool, conv, 6)
	c.Eq("", secondSummary.parent, "the second summary is a root too")
	c.Eq(firstTail.id, secondSummary.derived, "derived from the first compaction's last node")

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	c.Require().Len(working, 2, "working set")
	c.EqDeep([]int{6, 7}, []int{working[0].Ordinal, working[1].Ordinal}, "working ordinals")
	c.Eq(KindCompactionSummary, *working[0].Kind, "working[0] kind")
}

// TestCompactionFencedLeaseLostWritesNothing: a stale holder writes nothing and
// the head is unchanged.
func TestCompactionFencedLeaseLostWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)
	before := headID(t, pool, conv)

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
	c.Eq(before, headID(t, pool, conv), "head moved by a refused compaction")
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
	_, aerr := NewMessages(pool).AppendNode(ctx, conv, assistantToolUse(orphan), nil, "")
	c.NoError(aerr, "append assistant tool_use")

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

// TestAppendClearWritesRootAndMovesHead: a clear appends one kind='clear' root,
// moves the head onto it, touches nothing else, and the working path is exactly
// that row.
func TestAppendClearWritesRootAndMovesHead(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 3)
	before := headID(t, pool, conv)

	ord, err := NewMessages(pool).AppendClear(ctx, conv, userMessage(ClearBoundaryText))
	c.NoError(err, "AppendClear")
	c.Eq(3, ord, "boundary ordinal")
	c.Eq(4, rowCount(t, pool, conv), "no row is deleted or duplicated")
	boundary := readTreeRow(t, pool, conv, ord)
	c.Require().NotNil(boundary.kind, "boundary kind")
	c.Eq(KindClear, *boundary.kind, "boundary kind")
	c.Eq("", boundary.parent, "the boundary is a root")
	c.Eq(0, boundary.depth, "root depth")
	c.Eq(before, boundary.derived, "derived from the pre-clear head")
	c.Eq(boundary.id, headID(t, pool, conv), "head is the boundary")

	working, err := NewMessages(pool).LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	c.Require().Len(working, 1, "working path")
	c.True(IsClearBoundary(working[0]), "the working path must be the synthetic boundary row")
}

// TestTwoClearsAreDistinctRoots: a clear root's seed mixes in the node it is
// derived from, so the SAME boundary text twice is two distinct roots rather
// than one node reached twice — otherwise the second clear would write nothing
// and the working path would still hold the first.
func TestTwoClearsAreDistinctRoots(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)
	msgs := NewMessages(pool)

	first, err := msgs.AppendClear(ctx, conv, userMessage(ClearBoundaryText))
	c.NoError(err, "first AppendClear")
	second, err := msgs.AppendClear(ctx, conv, userMessage(ClearBoundaryText))
	c.NoError(err, "second AppendClear")
	c.Eq(first+1, second, "the second clear is a new row")

	c.Eq(4, rowCount(t, pool, conv), "two clears are two rows")
	firstRow, secondRow := readTreeRow(t, pool, conv, first), readTreeRow(t, pool, conv, second)
	c.NotEq(firstRow.id, secondRow.id, "two distinct rows")
	c.Eq("", secondRow.parent, "the second clear is a root too")
	c.Eq(firstRow.id, secondRow.derived, "derived from the first boundary")
	c.False(string(firstRow.hash) == string(secondRow.hash), "two clears must hash differently")

	working, err := msgs.LoadWorking(ctx, conv)
	c.NoError(err, "LoadWorking")
	c.Require().Len(working, 1, "working path")
	c.Eq(secondRow.id, working[0].ID, "working path is the SECOND boundary")
}

// TestAppendClearFencedLeaseLostWritesNothing mirrors the compaction fence.
func TestAppendClearFencedLeaseLostWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)
	appendRows(t, pool, conv, 2)
	before := headID(t, pool, conv)

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	c.False(err != nil || !ok, "acquire stale: ok=%v err=%v", ok, err)
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}

	_, err = NewMessages(pool).WithLease(stale).AppendClear(ctx, conv, userMessage(ClearBoundaryText))
	c.ErrorIs(err, ErrLeaseLost, "AppendClear error")
	c.Eq(2, rowCount(t, pool, conv), "a refused clear writes nothing")
	c.Eq(before, headID(t, pool, conv), "a refused clear must not move the head")
}

// TestSeedChainIdempotentAndCompletesPartial: seeding a prefix then the whole
// history inserts only the missing suffix and never duplicates the prefix.
func TestSeedChainIdempotentAndCompletesPartial(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	msgs := NewMessages(pool)

	r0 := userMessage("r0")
	r1 := assistantMessage("r1")
	r2 := userMessage("r2")

	c.NoError(msgs.SeedChain(ctx, conv, []anthropic.MessageParam{r0}), "seed r0")
	c.Eq(1, rowCount(t, pool, conv), "rows after seeding r0")
	r0Row := readTreeRow(t, pool, conv, 0)

	c.NoError(msgs.SeedChain(ctx, conv, []anthropic.MessageParam{r0, r1, r2}), "seed r0,r1,r2")
	c.Eq(3, rowCount(t, pool, conv), "rows after completing the chain")
	c.Eq(0, readTreeRow(t, pool, conv, 0).ordinal, "r0 ordinal")
	c.Eq(r0Row.id, readTreeRow(t, pool, conv, 0).id, "r0 is not duplicated")
	c.EqDeep([]string{"r0", "r1", "r2"}, pathTexts(t, pool, conv), "working path")
	c.Eq(readTreeRow(t, pool, conv, 2).id, headID(t, pool, conv), "head is the last seeded node")

	// Seeding the same history again writes nothing.
	c.NoError(msgs.SeedChain(ctx, conv, []anthropic.MessageParam{r0, r1, r2}), "re-seed")
	c.Eq(3, rowCount(t, pool, conv), "rows after re-seeding")
}
