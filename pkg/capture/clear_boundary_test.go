// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// clearTestConv creates a client-driven conversation with a per-run-unique
// external_ref (so MarkClearPending can find it, and so the shared test
// database never collides across runs) and returns its id and that ref.
func clearTestConv(t *testing.T, ctx context.Context, s *CaptureStore, label string) (string, string) {
	t.Helper()
	c := assert.NewAborting(t)
	ref := "clear-boundary-" + label + "-" + time.Now().Format(time.RFC3339Nano)
	convID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: ref,
	})
	c.NoError(err, "EnsureConversationByExternalRef")
	return convID, ref
}

// clearDecompose runs one InsertTurnIntent + DecomposeRequest pair (the proxy's
// request-side sequence) for a request that names no predecessor and returns
// the request head. It does not complete the turn: the clear root's write does
// not consult a prior turn's usage.
func clearDecompose(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte) string {
	t.Helper()
	c := assert.NewAborting(t)
	h := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: h, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, h, "")
	c.NoError(err, "DecomposeRequest")
	return head
}

func clearPendingOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) bool {
	t.Helper()
	var p bool
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT clear_pending FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&p), "read clear_pending")
	return p
}

// TestClearBoundaryWritesClearRow: a predecessor-less request with a /clear
// pending opens a clear root derived from the head it cut, consumes the flag in
// the same transaction, and leaves the old rows untouched. Fails if the
// transaction does not clear the flag, if the root is not tagged, or if it
// carries a size (a /clear replaces the context, it does not summarise it).
func TestClearBoundaryWritesClearRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "writes-clear-row")

	seed := []byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"},{"role":"user","content":"m2"},{"role":"assistant","content":"m3"}]}`)
	runTurn(t, ctx, s, convID, seed, 100, 10)
	previousHead := mustHead(t, ctx, pool, convID)
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")
	c.True(clearPendingOf(t, ctx, pool, convID), "clear_pending set by MarkClearPending")

	head := clearDecompose(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"fresh after clear"}]}`))

	kind, derived, depth := mustNodeKind(t, ctx, pool, head)
	c.Eq("clear", kind, "kind")
	c.Eq(0, depth, "root depth")
	c.Eq(previousHead, derived, "derived_from_id is the head the clear cut")
	c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending must be consumed by the root write")

	var inTok *int64
	c.NoError(pool.QueryRow(ctx,
		`SELECT input_tokens FROM conversations.conversation_message WHERE id=$1::uuid`, head).Scan(&inTok), "read input_tokens")
	c.Nil(inTok, "input_tokens = %v, want nil (a /clear carries no size)", inTok)

	var content string
	c.NoError(pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE id=$1::uuid`, head).Scan(&content), "read content")
	requireJSONEqual(t, content, `"fresh after clear"`)

	// Old rows 0..3 are untouched, each still under the one before it.
	want := []string{`"m0"`, `"m1"`, `"m2"`, `"m3"`}
	for i, w := range want {
		_, _, _, got := requireKindRow(t, ctx, pool, convID, i)
		requireJSONEqual(t, got, w)
	}
	c.Eq(mustNodeByContent(t, ctx, pool, convID, `"m0"`), mustParent(t, ctx, pool, mustNodeByContent(t, ctx, pool, convID, `"m1"`)),
		"the pre-clear history still hangs together")
}

// TestAClearWinsOverAMatchingRoot: the clear is checked before the root match,
// so a post-/clear history that happens to repeat an old root's message still
// opens a clear root rather than attaching to the pre-clear one.
func TestAClearWinsOverAMatchingRoot(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "clear-beats-root")

	runTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`), 100, 10)
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

	// "hi" is the existing root's message, so the root match would fire if it
	// were consulted first.
	head := clearDecompose(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`))

	kind, _, depth := mustNodeKind(t, ctx, pool, head)
	c.Eq("clear", kind, "kind")
	c.Eq(0, depth, "root depth")
	c.NotEq(mustNodeByContent(t, ctx, pool, convID, `"hi"`), head, "the request did not attach to the pre-clear root")
	c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending consumed")
}

// TestClearPendingSurvivesAChainedRequest: a /clear is consumed by the first
// request that names no predecessor. A request that chains to a stored reply is
// an ordinary continuation, so the flag survives it and no clear root appears.
func TestClearPendingSurvivesAChainedRequest(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "stays-pending")

	runTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"}]}`), 100, 10)
	mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID),
		[]byte(`{"id":"msg_pending","content":[{"type":"text","text":"reply"}]}`), "end_turn")
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

	runTurnFrom(t, ctx, s, convID,
		[]byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"},{"role":"user","content":"next"}]}`),
		"msg_pending", 110, 11)

	c.True(clearPendingOf(t, ctx, pool, convID), "clear_pending must survive a chained request")
	var clears int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='clear'`,
		convID).Scan(&clears), "count clear roots")
	c.Eq(0, clears, "clear roots")
}

// TestAPredecessorlessRequestWithoutAClearAttachesToAMatchingRoot: a retried
// first request — including the first request after a /clear whose flag the
// original already consumed — lands on the root it created instead of opening a
// second one.
func TestAPredecessorlessRequestWithoutAClearAttachesToAMatchingRoot(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, _ := clearTestConv(t, ctx, s, "matching-root")

	fresh := []byte(`{"model":"claude","messages":[{"role":"user","content":"fresh after clear"},{"role":"user","content":"and more"}]}`)
	first := clearDecompose(t, ctx, s, convID, fresh)
	nodes := mustNodeCount(t, ctx, pool, convID)

	retry := clearDecompose(t, ctx, s, convID, fresh)

	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "the retry inserts nothing")
	c.Eq(first, retry, "the retry lands on the head the original created")
	var roots int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND parent_id IS NULL`,
		convID).Scan(&roots), "count roots")
	c.Eq(1, roots, "one root")
}

// TestAClearWinsOverACompactionSummary pins the rule order: the pending clear
// is checked before the summary shape, so a summary arriving with a clear
// pending opens a clear root and leaves the summary as ordinary content.
func TestAClearWinsOverACompactionSummary(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "pending-compaction")

	runTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`), 100, 10)
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

	head := clearDecompose(t, ctx, s, convID,
		[]byte(`{"model":"claude","messages":[{"role":"user","content":`+summaryContent("SUMMARY-A details")+`},{"role":"user","content":"q1"}]}`))

	// The head is the request's last message; the root is its first.
	root := mustParent(t, ctx, pool, head)
	kind, _, depth := mustNodeKind(t, ctx, pool, root)
	c.Eq("clear", kind, "kind")
	c.Eq(0, depth, "root depth")
	c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending consumed")
	var summaries int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='compaction_summary'`,
		convID).Scan(&summaries), "count summary roots")
	c.Eq(0, summaries, "the clear won, so no compaction root was written")
}

// TestMarkClearPendingTargetsOnlyTheMainConversation: MarkClearPending matches
// an exact external_ref, so a child's thread branch ("X:thread1") is not
// flagged, and an unknown ref is not an error.
func TestMarkClearPendingTargetsOnlyTheMainConversation(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	label := "targets-main-" + time.Now().Format(time.RFC3339Nano)
	ref := "clear-boundary-" + label

	mainID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: ref})
	c.NoError(err, "EnsureConversationByExternalRef (main)")
	branchID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: ref + ":thread1"})
	c.NoError(err, "EnsureConversationByExternalRef (branch)")

	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")
	c.True(clearPendingOf(t, ctx, pool, mainID), "main conversation flagged")
	c.False(clearPendingOf(t, ctx, pool, branchID), "branch conversation must not be flagged by the exact ref")

	// Unknown ref: not an error (a child that captured nothing has nothing to clear).
	c.NoError(s.MarkClearPending(ctx, ref+"-unknown"), "MarkClearPending on an unknown ref")
}
