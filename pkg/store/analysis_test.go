// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newTestConversation(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

func TestUpsertAnalysisReplacesOnSameKey(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if id1 == "" {
		t.Fatal("first upsert returned empty id")
	}

	row.Analysis = []byte(`{"a":2}`)
	row.InputTokens = 20
	id2, _, err := UpsertAnalysis(ctx, pool, row)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id2 == id1 {
		t.Fatalf("second upsert reused id %q, want a fresh id", id2)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_analysis
		WHERE conversation_id=$1::uuid`, convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows after second upsert = %d, want 1 (old replaced)", n)
	}
	var old bool
	err = pool.QueryRow(ctx, `SELECT true FROM conversations.conversation_analysis WHERE id=$1::uuid`, id1).Scan(&old)
	if err == nil {
		t.Fatalf("old row %q still present after replace", id1)
	}

	var gotInput int64
	if err := pool.QueryRow(ctx, `SELECT input_tokens FROM conversations.conversation_analysis WHERE id=$1::uuid`, id2).
		Scan(&gotInput); err != nil {
		t.Fatal(err)
	}
	if gotInput != 20 {
		t.Fatalf("input_tokens = %d, want 20 (new row's data)", gotInput)
	}
}

func TestUpsertAnalysisStripsNULFromAnalysisJSON(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	convID := newTestConversation(t, ctx, pool)

	row := AnalysisRow{
		ConversationID:  convID,
		DetectorVersion: 1,
		Model:           "claude-x",
		Analysis:        []byte(`{"title":"has\u0000nul"}`),
	}
	id, _, err := UpsertAnalysis(ctx, pool, row)
	if err != nil {
		t.Fatalf("upsert with NUL: %v", err)
	}
	var got string
	if err := pool.QueryRow(ctx, `SELECT analysis->>'title' FROM conversations.conversation_analysis WHERE id=$1::uuid`, id).
		Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "hasnul" {
		t.Fatalf("stored title = %q, want NUL stripped", got)
	}
}

func TestAnalyzedSet(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if !set[convA] || !set[convB] {
		t.Fatalf("AnalyzedSet = %+v, want both convA and convB present", set)
	}

	// Different model at the same detector version/prompt hash: neither convo has been analyzed under it.
	set2, err := AnalyzedSet(ctx, pool, []string{convA, convB}, 1, "claude-other", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if set2[convA] || set2[convB] {
		t.Fatalf("AnalyzedSet (different model) = %+v, want empty", set2)
	}

	// Different prompt hash: also unanalyzed under that key.
	set3, err := AnalyzedSet(ctx, pool, []string{convA}, 1, "claude-x", "hash2")
	if err != nil {
		t.Fatal(err)
	}
	if set3[convA] {
		t.Fatalf("AnalyzedSet (different prompt hash) = %+v, want empty", set3)
	}

	// A conversation with no analysis row at all is absent from the set.
	convC := newTestConversation(t, ctx, pool)
	set4, err := AnalyzedSet(ctx, pool, []string{convC}, 1, "claude-x", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if set4[convC] {
		t.Fatalf("AnalyzedSet (never analyzed) = %+v, want empty", set4)
	}
}

func TestReplaceFindingsReplacesNotAppends(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "t1", Title: "first"},
		{Axis: "prompt", TopicKey: "t2", Title: "second"},
	}, nil); err != nil {
		t.Fatalf("first ReplaceFindings: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("findings after first replace = %d, want 2", n)
	}

	if err := ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "t3", Title: "third"},
	}, nil); err != nil {
		t.Fatalf("second ReplaceFindings: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("findings after second replace = %d, want 1 (replaced not appended)", n)
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "third" {
		t.Fatalf("surviving finding title = %q, want %q", title, "third")
	}
}

func TestReplaceFindingsEmptyClearsAll(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFindings(ctx, pool, analysisID, []FindingRow{{Axis: "prompt", TopicKey: "t1", Title: "first"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFindings(ctx, pool, analysisID, nil, nil); err != nil {
		t.Fatalf("ReplaceFindings(nil): %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("findings after clearing to empty = %d, want 0", n)
	}
}

func TestListFindingsDefaultOpenAndOrdering(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "low", SkillName: "skillA", Title: "low savings", ExpectedSavingsTokens: 10},
		{Axis: "prompt", TopicKey: "high", SkillName: "skillB", Title: "high savings", ExpectedSavingsTokens: 100},
		{Axis: "tool", TopicKey: "mid", SkillName: "skillA", Title: "mid savings", ExpectedSavingsTokens: 50},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// Mark one dismissed: default filter should exclude it.
	var dismissedID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM conversations.analysis_finding WHERE topic_key='mid'`).
		Scan(&dismissedID); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFindingStatus(ctx, pool, dismissedID, "dismissed"); err != nil {
		t.Fatal(err)
	}

	rows, err := ListFindings(ctx, pool, FindingFilter{})
	if err != nil {
		t.Fatalf("ListFindings default: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListFindings default returned %d rows, want 2 (open only)", len(rows))
	}
	if rows[0].TopicKey != "high" || rows[1].TopicKey != "low" {
		t.Fatalf("ListFindings order = [%s, %s], want [high, low] (expected_savings_tokens DESC)", rows[0].TopicKey, rows[1].TopicKey)
	}

	// Axis filter (within the open-only default: "mid" is dismissed and tool-axis, so this
	// exercises axis narrowing on the surviving prompt-axis findings).
	rows, err = ListFindings(ctx, pool, FindingFilter{Axis: "prompt"})
	if err != nil {
		t.Fatalf("ListFindings axis filter: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListFindings axis=prompt = %+v, want [high, low]", rows)
	}

	// Skill filter.
	rows, err = ListFindings(ctx, pool, FindingFilter{Skill: "skillB"})
	if err != nil {
		t.Fatalf("ListFindings skill filter: %v", err)
	}
	if len(rows) != 1 || rows[0].TopicKey != "high" {
		t.Fatalf("ListFindings skill=skillB = %+v, want [high]", rows)
	}

	// Explicit status filter overrides the open default.
	rows, err = ListFindings(ctx, pool, FindingFilter{Status: "dismissed"})
	if err != nil {
		t.Fatalf("ListFindings status filter: %v", err)
	}
	if len(rows) != 1 || rows[0].TopicKey != "mid" {
		t.Fatalf("ListFindings status=dismissed = %+v, want [mid]", rows)
	}
}

func TestSetFindingStatus(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	convID := newTestConversation(t, ctx, pool)
	analysisID, _, err := UpsertAnalysis(ctx, pool, AnalysisRow{
		ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFindings(ctx, pool, analysisID, []FindingRow{
		{Axis: "prompt", TopicKey: "t1", Title: "first", ExpectedSavingsTokens: 42, SkillName: "skillA"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM conversations.analysis_finding WHERE analysis_id=$1::uuid`, analysisID).
		Scan(&id); err != nil {
		t.Fatal(err)
	}

	row, err := SetFindingStatus(ctx, pool, id, "actioned")
	if err != nil {
		t.Fatalf("valid status: %v", err)
	}
	// SetFindingStatus must return the updated row directly (RETURNING),
	// not force the caller to list every finding and scan for the one it
	// just touched.
	if row.ID != id {
		t.Errorf("row.ID = %q, want %q", row.ID, id)
	}
	if row.AnalysisID != analysisID {
		t.Errorf("row.AnalysisID = %q, want %q", row.AnalysisID, analysisID)
	}
	if row.Axis != "prompt" || row.TopicKey != "t1" || row.Title != "first" {
		t.Errorf("row = %+v, want axis=prompt topic_key=t1 title=first", row)
	}
	if row.SkillName != "skillA" || row.ExpectedSavingsTokens != 42 {
		t.Errorf("row = %+v, want skill_name=skillA expected_savings_tokens=42", row)
	}
	if row.Status != "actioned" {
		t.Errorf("row.Status = %q, want actioned", row.Status)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM conversations.analysis_finding WHERE id=$1::uuid`, id).
		Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "actioned" {
		t.Fatalf("status = %q, want actioned", status)
	}

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
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	convID := newTestConversation(t, ctx, pool)
	row := AnalysisRow{ConversationID: convID, DetectorVersion: 1, Model: "claude-x", Analysis: []byte(`{}`)}

	analysisID1, prior, err := UpsertAnalysis(ctx, pool, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 0 {
		t.Fatalf("prior on first analysis = %+v, want empty (nothing to carry yet)", prior)
	}
	if err := ReplaceFindings(ctx, pool, analysisID1, []FindingRow{
		{Axis: "prompt", TopicKey: "dismiss-me", Title: "will be dismissed"},
		{Axis: "prompt", TopicKey: "keep-open", Title: "stays open"},
	}, nil); err != nil {
		t.Fatal(err)
	}

	var dismissID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM conversations.analysis_finding
		WHERE analysis_id=$1::uuid AND topic_key='dismiss-me'`, analysisID1).Scan(&dismissID); err != nil {
		t.Fatal(err)
	}
	if _, err := SetFindingStatus(ctx, pool, dismissID, "dismissed"); err != nil {
		t.Fatal(err)
	}

	// Force re-analysis: same key, so UpsertAnalysis deletes+cascades the
	// old analysis/findings, but must hand back the dismissed status keyed
	// by (axis, topic_key) before it does.
	analysisID2, prior2, err := UpsertAnalysis(ctx, pool, row)
	if err != nil {
		t.Fatal(err)
	}
	if analysisID2 == analysisID1 {
		t.Fatal("re-analysis reused the analysis id, want a fresh one")
	}
	wantKey := FindingKey{Axis: "prompt", TopicKey: "dismiss-me"}
	if prior2[wantKey] != "dismissed" {
		t.Fatalf("prior2[%+v] = %q, want dismissed (prior2=%+v)", wantKey, prior2[wantKey], prior2)
	}
	if _, ok := prior2[FindingKey{Axis: "prompt", TopicKey: "keep-open"}]; ok {
		t.Fatalf("prior2 carries keep-open, want only non-open findings tracked: %+v", prior2)
	}

	// The detector re-detects both topics, plus a brand-new one.
	if err := ReplaceFindings(ctx, pool, analysisID2, []FindingRow{
		{Axis: "prompt", TopicKey: "dismiss-me", Title: "will be dismissed"},
		{Axis: "prompt", TopicKey: "keep-open", Title: "stays open"},
		{Axis: "prompt", TopicKey: "brand-new", Title: "never seen before"},
	}, prior2); err != nil {
		t.Fatal(err)
	}

	rows, err := ListFindings(ctx, pool, FindingFilter{Status: "dismissed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TopicKey != "dismiss-me" {
		t.Fatalf("dismissed findings after re-analysis = %+v, want exactly [dismiss-me] (carried over, not resurrected as open)", rows)
	}

	rows, err = ListFindings(ctx, pool, FindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	gotOpen := map[string]bool{}
	for _, r := range rows {
		gotOpen[r.TopicKey] = true
	}
	if !gotOpen["keep-open"] || !gotOpen["brand-new"] {
		t.Fatalf("open findings after re-analysis = %+v, want keep-open and brand-new both open", rows)
	}
	if gotOpen["dismiss-me"] {
		t.Fatalf("dismiss-me resurrected as open: %+v", rows)
	}
}
