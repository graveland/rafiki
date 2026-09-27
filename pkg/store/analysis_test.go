// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

func newTestConversation(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation")
	return id
}

func TestUpsertAnalysisReplacesOnSameKey(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)

	row := AnalysisRow{
		ConversationID:  convID,
		DetectorVersion: 1,
		Model:           "claude-x",
		Analysis:        []byte(`{"a":1}`),
		InputTokens:     10,
		OutputTokens:    5,
		CostUSD:         0.01,
	}
	id1, _, err := UpsertAnalysis(ctx, pool, row)
	c.NoError(err, "first upsert")
	c.NotEq("", id1, "first upsert returned empty id")

	row.Analysis = []byte(`{"a":2}`)
	row.InputTokens = 20
	id2, _, err := UpsertAnalysis(ctx, pool, row)
	c.NoError(err, "second upsert")
	c.NotEq(id1, id2, "second upsert reused id")

	var n int
	c.NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_analysis
		WHERE conversation_id=$1::uuid`, convID).Scan(&n))
	c.Eq(1, n, "rows after second upsert")
	var old bool
	err = pool.QueryRow(ctx, `SELECT true FROM conversations.conversation_analysis WHERE id=$1::uuid`, id1).Scan(&old)
	c.Error(err, "old row %q still present after replace", id1)

	var gotInput int64
	c.NoError(pool.QueryRow(ctx, `SELECT input_tokens FROM conversations.conversation_analysis WHERE id=$1::uuid`, id2).
		Scan(&gotInput))
	c.Eq(20, gotInput, "input_tokens")
}

func TestUpsertAnalysisStripsNULFromAnalysisJSON(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)

	row := AnalysisRow{
		ConversationID:  convID,
		DetectorVersion: 1,
		Model:           "claude-x",
		Analysis:        []byte(`{"title":"has\u0000nul"}`),
	}
	id, _, err := UpsertAnalysis(ctx, pool, row)
	c.NoError(err, "upsert with NUL")
	var got string
	c.NoError(pool.QueryRow(ctx, `SELECT analysis->>'title' FROM conversations.conversation_analysis WHERE id=$1::uuid`, id).
		Scan(&got))
	c.Eq("hasnul", got, "stored title")
}

func TestAnalyzedSet(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convA := newTestConversation(t, ctx, pool)
	convB := newTestConversation(t, ctx, pool)

	if _, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convA, DetectorVersion: 1, Model: "claude-x", PromptHash: "hash1",
		Status: "ok", Analysis: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	// convB has a failed analysis at the same key: still counts as analyzed.
	if _, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convB, DetectorVersion: 1, Model: "claude-x", PromptHash: "hash1",
		Status: "failed", Error: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	set, err := AnalyzedSet(ctx, pool, []string{convA, convB}, 1, "claude-x", "hash1")
	c.NoError(err)
	c.False(!set[convA] || !set[convB], "AnalyzedSet = %+v, want both convA and convB present", set)

	// Different model at the same detector version/prompt hash: neither convo has been analyzed under it.
	set2, err := AnalyzedSet(ctx, pool, []string{convA, convB}, 1, "claude-other", "hash1")
	c.NoError(err)
	c.False(set2[convA] || set2[convB], "AnalyzedSet (different model) = %+v, want empty", set2)

	// Different prompt hash: also unanalyzed under that key.
	set3, err := AnalyzedSet(ctx, pool, []string{convA}, 1, "claude-x", "hash2")
	c.NoError(err)
	c.False(set3[convA], "AnalyzedSet (different prompt hash) = %+v, want empty", set3)

	// A conversation with no analysis row at all is absent from the set.
	convC := newTestConversation(t, ctx, pool)
	set4, err := AnalyzedSet(ctx, pool, []string{convC}, 1, "claude-x", "hash1")
	c.NoError(err)
	c.False(set4[convC], "AnalyzedSet (never analyzed) = %+v, want empty", set4)
}

func TestReplaceFindingsReplacesNotAppends(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	c.NoError(err)

	c.NoError(ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "t1", Title: "first"},
		{Axis: "prompt", TopicKey: "t2", Title: "second"},
	}, nil), "first ReplaceFindings")
	var n int
	c.NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&n))
	c.Eq(2, n, "findings after first replace")

	c.NoError(ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "t3", Title: "third"},
	}, nil), "second ReplaceFindings")
	c.NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&n))
	c.Eq(1, n, "findings after second replace")
	var title string
	c.NoError(pool.QueryRow(ctx, `SELECT title FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&title))
	c.Eq("third", title, "surviving finding title")
}

func TestReplaceFindingsEmptyClearsAll(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	c.NoError(err)
	c.NoError(ReplaceFindings(ctx, pool, analysisID, []FindingRow{{Axis: "prompt", TopicKey: "t1", Title: "first"}}, nil))
	c.NoError(ReplaceFindings(ctx, pool, analysisID, nil, nil), "ReplaceFindings(nil)")
	var n int
	c.NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&n))
	c.Eq(0, n, "findings after clearing to empty")
}

func TestListFindingsDefaultOpenAndOrdering(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	c.NoError(err)
	c.NoError(ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "low", SkillName: "skillA", Title: "low savings", ExpectedSavingsTokens: 10},
		{Axis: "prompt", TopicKey: "high", SkillName: "skillB", Title: "high savings", ExpectedSavingsTokens: 100},
		{Axis: "tool", TopicKey: "mid", SkillName: "skillA", Title: "mid savings", ExpectedSavingsTokens: 50},
	}, nil))
	// Mark one dismissed: default filter should exclude it.
	var dismissedID string
	c.NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.analysis_finding WHERE topic_key='mid'`).
		Scan(&dismissedID))
	if _, err := SetFindingStatus(ctx, pool, dismissedID, "dismissed"); err != nil {
		t.Fatal(err)
	}

	rows, err := ListFindings(ctx, pool, FindingFilter{})
	c.NoError(err, "ListFindings default")
	c.Len(rows, 2, "ListFindings default returned %d rows, want 2 (open only)", len(rows))
	if rows[0].TopicKey != "high" || rows[1].TopicKey != "low" {
		t.Fatalf("ListFindings order = [%s, %s], want [high, low] (expected_savings_tokens DESC)", rows[0].TopicKey, rows[1].TopicKey)
	}

	// Axis filter (within the open-only default: "mid" is dismissed and tool-axis, so this
	// exercises axis narrowing on the surviving prompt-axis findings).
	rows, err = ListFindings(ctx, pool, FindingFilter{Axis: "prompt"})
	c.NoError(err, "ListFindings axis filter")
	c.Len(rows, 2, "ListFindings axis=prompt")

	// Skill filter.
	rows, err = ListFindings(ctx, pool, FindingFilter{Skill: "skillB"})
	c.NoError(err, "ListFindings skill filter")
	c.False(len(rows) != 1 || rows[0].TopicKey != "high", "ListFindings skill=skillB = %+v, want [high]", rows)

	// Explicit status filter overrides the open default.
	rows, err = ListFindings(ctx, pool, FindingFilter{Status: "dismissed"})
	c.NoError(err, "ListFindings status filter")
	c.False(len(rows) != 1 || rows[0].TopicKey != "mid", "ListFindings status=dismissed = %+v, want [mid]", rows)
}

func TestSetFindingStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	c.Require().NoError(err)
	c.Require().NoError(ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "t1", Title: "first", ExpectedSavingsTokens: 42, SkillName: "skillA"},
	}, nil))
	var id string
	c.Require().NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&id))

	row, err := SetFindingStatus(ctx, pool, id, "actioned")
	c.Require().NoError(err, "valid status")
	// SetFindingStatus must return the updated row directly (RETURNING),
	// not force the caller to list every finding and scan for the one it
	// just touched.
	c.Eq(id, row.ID, "row.ID")
	c.Eq(analysisID, row.AnalysisID, "row.AnalysisID")
	c.False(row.Axis != "prompt" || row.TopicKey != "t1" || row.Title != "first", "row = %+v, want axis=prompt topic_key=t1 title=first", row)
	c.False(row.SkillName != "skillA" || row.ExpectedSavingsTokens != 42, "row = %+v, want skill_name=skillA expected_savings_tokens=42", row)
	c.Eq("actioned", row.Status, "row.Status")

	var status string
	c.Require().NoError(pool.QueryRow(ctx, `SELECT status FROM conversations.analysis_finding WHERE id=$1::uuid`, id).
		Scan(&status))
	c.Require().Eq("actioned", status, "status")

	if _, err := SetFindingStatus(ctx, pool, id, "bogus"); err == nil {
		t.Fatal("SetFindingStatus with invalid enum: want error, got nil")
	}

	if _, err := SetFindingStatus(ctx, pool, "00000000-0000-0000-0000-000000000000", "open"); err == nil {
		t.Fatal("SetFindingStatus on unknown id: want error, got nil")
	}
}

// TestUpsertAnalysisCarriesFindingStatusAcrossReAnalysis covers the
// resurrection bug UpsertAnalysis's prior-status capture and
// ReplaceFindings' carry-over exist to fix: a --force re-analysis deletes
// the old conversation_analysis row (cascading away its analysis_finding
// rows, status included) and then re-inserts the re-detected findings —
// without carry-over, a finding a human had already dismissed would come
// back as 'open' merely because the detector found it again.
func TestUpsertAnalysisCarriesFindingStatusAcrossReAnalysis(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))
	convID := newTestConversation(t, ctx, pool)
	row := AnalysisRow{ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`)}

	analysisID1, prior, err := UpsertAnalysis(ctx, pool, row)
	c.NoError(err)
	c.Empty(prior, "prior on first analysis")
	c.NoError(ReplaceFindings(ctx, pool, analysisID1, []FindingRow{
		{Axis: "prompt", TopicKey: "dismiss-me", Title: "will be dismissed"},
		{Axis: "prompt", TopicKey: "keep-open", Title: "stays open"},
	}, nil))

	var dismissID string
	c.NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.analysis_finding
		WHERE analysis_id=$1::uuid AND topic_key='dismiss-me'`, analysisID1).Scan(&dismissID))
	if _, err := SetFindingStatus(ctx, pool, dismissID, "dismissed"); err != nil {
		t.Fatal(err)
	}

	// Force re-analysis: same key, so UpsertAnalysis deletes+cascades the
	// old analysis/findings, but must hand back the dismissed status keyed
	// by (axis, topic_key) before it does.
	analysisID2, prior2, err := UpsertAnalysis(ctx, pool, row)
	c.NoError(err)
	c.NotEq(analysisID1, analysisID2, "re-analysis reused the analysis id, want a fresh one")
	wantKey := FindingKey{Axis: "prompt", TopicKey: "dismiss-me"}
	c.Eq("dismissed", prior2[wantKey], "prior2[%+v] = %q, want dismissed (prior2=%+v)", wantKey, prior2[wantKey], prior2)
	_, ok := prior2[FindingKey{Axis: "prompt", TopicKey: "keep-open"}]
	c.False(ok, "prior2 carries keep-open, want only non-open findings tracked: %+v", prior2)

	// The detector re-detects both topics, plus a brand-new one.
	c.NoError(ReplaceFindings(ctx, pool, analysisID2, []FindingRow{
		{Axis: "prompt", TopicKey: "dismiss-me", Title: "will be dismissed"},
		{Axis: "prompt", TopicKey: "keep-open", Title: "stays open"},
		{Axis: "prompt", TopicKey: "brand-new", Title: "never seen before"},
	}, prior2))

	rows, err := ListFindings(ctx, pool, FindingFilter{Status: "dismissed"})
	c.NoError(err)
	c.False(len(rows) != 1 || rows[0].TopicKey != "dismiss-me", "dismissed findings after re-analysis = %+v, want exactly [dismiss-me] (carried over, not resurrected as open)", rows)

	rows, err = ListFindings(ctx, pool, FindingFilter{})
	c.NoError(err)
	gotOpen := map[string]bool{}
	for _, r := range rows {
		gotOpen[r.TopicKey] = true
	}
	c.False(!gotOpen["keep-open"] || !gotOpen["brand-new"], "open findings after re-analysis = %+v, want keep-open and brand-new both open", rows)
	c.False(gotOpen["dismiss-me"], "dismiss-me resurrected as open: %+v", rows)
}
