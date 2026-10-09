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

// clearDecompose runs one InsertTurnIntent + DecomposeRequest pair (the
// proxy's request-side sequence) and returns DecomposeRequest's horizon-aware
// next ordinal. It does not complete the turn: the clear path's boundary write
// does not consult a prior turn's usage.
func clearDecompose(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte) int {
	t.Helper()
	c := assert.NewAborting(t)
	h := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: h, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, h)
	c.NoError(err, "DecomposeRequest")
	return next
}

func storedHorizon(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) int {
	t.Helper()
	var h int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&h), "read resume_from_ordinal")
	return h
}

func clearPendingOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) bool {
	t.Helper()
	var p bool
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT clear_pending FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&p), "read clear_pending")
	return p
}

// TestClearBoundaryWritesClearRow: a request whose head diverges from the
// stored head, with a /clear pending, records a kind='clear' boundary at
// H'=max(ordinal)+1, consumes clear_pending in the same transaction, and
// leaves the old rows untouched. Fails if the transaction doesn't clear the
// flag, if the tag is missing, or if H' is wrong.
func TestClearBoundaryWritesClearRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "writes-clear-row")

	seed := []byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"},{"role":"user","content":"m2"},{"role":"assistant","content":"m3"}]}`)
	c.Eq(4, runTurn(t, ctx, s, convID, seed, 100, 10), "seed next ordinal")
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")
	c.True(clearPendingOf(t, ctx, pool, convID), "clear_pending set by MarkClearPending")

	// A different, non-summary head: divergence with a /clear pending.
	clearReq := []byte(`{"model":"claude","messages":[{"role":"user","content":"fresh after clear"}]}`)
	next := clearDecompose(t, ctx, s, convID, clearReq)

	c.Eq(5, next, "next ordinal")
	c.Eq(4, storedHorizon(t, ctx, pool, convID), "resume_from_ordinal")
	c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending must be consumed by the boundary write")

	kind, role, inTok, content := requireKindRow(t, ctx, pool, convID, 4)
	c.False(kind == nil || *kind != "clear", "ordinal 4 kind = %v, want clear", kind)
	c.False(role == nil || *role != "user", "ordinal 4 role = %v, want user", role)
	// A /clear boundary replaces the context, it does not summarise it, so the
	// boundary row carries no size: the prior-turn input_tokens lookup is gated
	// on kind='compaction_summary' and must stay nil here.
	c.Nil(inTok, "ordinal 4 input_tokens = %v, want nil (a /clear carries no size)", inTok)
	requireJSONEqual(t, content, `"fresh after clear"`)

	// Old rows 0..3 are untouched.
	want := []string{`"m0"`, `"m1"`, `"m2"`, `"m3"`}
	for i, w := range want {
		_, _, _, got := requireKindRow(t, ctx, pool, convID, i)
		requireJSONEqual(t, got, w)
	}
}

// TestClearBoundarySkipsReanchor: the re-anchor guard is skipped while a
// /clear is pending. The request's two messages positionally match stored rows
// 0 and 1 (the shape TestDecomposeRequest_ReAnchorRewind relies on), so without
// the skip the horizon would re-anchor to 0; instead a clear boundary is
// written at H'=max(ordinal)+1. Fails if the skip is deleted.
func TestClearBoundarySkipsReanchor(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "skips-reanchor")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 100, 10), "turn 1 next ordinal")
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 110, 11), "turn 2 next ordinal")
	// Compact: horizon moves to 3, rows 3=summary, 4=q2.
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-A details") + `},{"role":"user","content":"q2"}]}`)
	c.Eq(5, runTurn(t, ctx, s, convID, req3, 120, 12), "turn 3 next ordinal")
	c.Eq(3, storedHorizon(t, ctx, pool, convID), "horizon after compaction")

	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

	// "hi","yo" match stored ordinals 0 and 1 — a re-anchor candidate — but the
	// pending clear must win.
	req4 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	next := clearDecompose(t, ctx, s, convID, req4)

	c.Eq(7, next, "next ordinal")
	c.Eq(5, storedHorizon(t, ctx, pool, convID), "resume_from_ordinal must be H', not the re-anchor target 0")
	c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending consumed")

	kind, _, _, content := requireKindRow(t, ctx, pool, convID, 5)
	c.False(kind == nil || *kind != "clear", "ordinal 5 kind = %v, want clear (a boundary, not a re-anchor)", kind)
	requireJSONEqual(t, content, `"hi"`)
}

// TestClearBoundaryStaysPendingOnMatchingHead: when the request's head equals
// the stored head at the horizon, the early return leaves clear_pending
// untouched — the clear has not happened yet. Fails if the flag is consumed on
// a matching head.
func TestClearBoundaryStaysPendingOnMatchingHead(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "stays-pending")

	seed := []byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"},{"role":"user","content":"m2"},{"role":"assistant","content":"m3"}]}`)
	c.Eq(4, runTurn(t, ctx, s, convID, seed, 100, 10), "seed next ordinal")
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

	// Same head at the horizon: a stable prefix, no divergence. Messages insert
	// at h=0 (DO NOTHING against the stored rows) and the return is h+len.
	sameReq := []byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"}]}`)
	next := clearDecompose(t, ctx, s, convID, sameReq)

	c.Eq(2, next, "next ordinal")
	c.Eq(0, storedHorizon(t, ctx, pool, convID), "horizon unchanged")
	c.True(clearPendingOf(t, ctx, pool, convID), "clear_pending must survive a matching head")

	var kindCount int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind IS NOT NULL`, convID).Scan(&kindCount), "count kind rows")
	c.Eq(0, kindCount, "kind-tagged rows")
}

// TestClearBoundaryNotPendingDropsDivergentHead: the same divergent request as
// TestClearBoundaryWritesClearRow but with no /clear pending keeps today's
// behaviour — horizon unchanged, no clear row, the head dropped. Fails if the
// branch ignores the flag.
func TestClearBoundaryNotPendingDropsDivergentHead(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, _ := clearTestConv(t, ctx, s, "not-pending-drops")

	seed := []byte(`{"model":"claude","messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"},{"role":"user","content":"m2"},{"role":"assistant","content":"m3"}]}`)
	c.Eq(4, runTurn(t, ctx, s, convID, seed, 100, 10), "seed next ordinal")

	// No MarkClearPending. The head inserts at h=0 (DO NOTHING), return h+len.
	clearReq := []byte(`{"model":"claude","messages":[{"role":"user","content":"fresh after clear"}]}`)
	next := clearDecompose(t, ctx, s, convID, clearReq)

	c.Eq(1, next, "next ordinal")
	c.Eq(0, storedHorizon(t, ctx, pool, convID), "horizon unchanged")

	var clearRows int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind='clear'`, convID).Scan(&clearRows), "count clear rows")
	c.Eq(0, clearRows, "clear rows")
}

// TestClearBoundaryPendingWithCompactionSummary: a divergent head that reads as
// a compaction summary takes the compaction path even with a /clear pending,
// and leaves clear_pending as it is.
func TestClearBoundaryPendingWithCompactionSummary(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "pending-compaction")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 100, 10), "turn 1 next ordinal")
	c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-A details") + `},{"role":"user","content":"q1"}]}`)
	next := clearDecompose(t, ctx, s, convID, req2)

	c.Eq(4, next, "next ordinal")
	c.Eq(2, storedHorizon(t, ctx, pool, convID), "horizon advanced to the compaction boundary")
	c.True(clearPendingOf(t, ctx, pool, convID), "clear_pending left as it is on the compaction path")

	kind, _, _, content := requireKindRow(t, ctx, pool, convID, 2)
	c.False(kind == nil || *kind != "compaction_summary", "ordinal 2 kind = %v, want compaction_summary", kind)
	requireJSONEqual(t, content, summaryContent("SUMMARY-A details"))
}

// TestMarkClearPendingTargetsOnlyTheMainConversation: MarkClearPending matches
// an exact external_ref, so a child's thread branch ("X:thread1") is not
// flagged, and an unknown ref is not an error.
func TestMarkClearPendingTargetsOnlyTheMainConversation(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
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
