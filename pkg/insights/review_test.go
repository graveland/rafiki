// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAnalysis is a fully-specified conversations.conversation_analysis row
// for tests, with optional findings to insert under it.
type seedAnalysis struct {
	model     string
	profile   string
	status    string // defaults to "ok"
	inTok     int64
	outTok    int64
	costUSD   float64
	createdAt time.Time // defaults to now() when zero
	findings  []seedFinding
}

// seedFinding is a fully-specified conversations.analysis_finding row.
type seedFinding struct {
	axis            string
	topicKey        string
	skillName       string
	title           string
	expectedSavings int64
	status          string // defaults to "open"
}

func insertAnalysis(t *testing.T, pool *pgxpool.Pool, convID string, sa seedAnalysis) string {
	t.Helper()
	status := sa.status
	if status == "" {
		status = "ok"
	}
	createdAt := sa.createdAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	var profile any
	if sa.profile != "" {
		profile = sa.profile
	}
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO conversations.conversation_analysis
			(conversation_id, detector_version, model, profile, status, error, prompt_hash,
			 analysis, input_tokens, output_tokens, cost_usd, created_at)
		VALUES ($1::uuid, 1, $2, $3, $4, NULL, '', NULL, $5, $6, $7, $8)
		RETURNING id::text`,
		convID, sa.model, profile, status, sa.inTok, sa.outTok, sa.costUSD, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert analysis: %v", err)
	}
	for _, sf := range sa.findings {
		insertFinding(t, pool, id, sf)
	}
	return id
}

func insertFinding(t *testing.T, pool *pgxpool.Pool, analysisID string, sf seedFinding) {
	t.Helper()
	status := sf.status
	if status == "" {
		status = "open"
	}
	var skill any
	if sf.skillName != "" {
		skill = sf.skillName
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO conversations.analysis_finding
			(analysis_id, axis, topic_key, skill_name, title, expected_savings_tokens, status)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
		analysisID, sf.axis, sf.topicKey, skill, sf.title, sf.expectedSavings, status); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
}

func TestReviewRecentAnalysesScopesByOwner(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-x", inTok: 100, outTok: 20, costUSD: 0.01})
	convB := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-x"})

	rows, err := New(pool).RecentAnalyses(ctx, ScopeOwner(alice), nil, 0)
	if err != nil {
		t.Fatalf("RecentAnalyses: %v", err)
	}
	if len(rows) != 1 || rows[0].ConversationID != convA {
		t.Fatalf("owner-scoped rows = %+v, want exactly convA (%s)", rows, convA)
	}
	if rows[0].Model != "detector-x" || rows[0].InputTokens != 100 || rows[0].OutputTokens != 20 || rows[0].CostUSD != 0.01 {
		t.Errorf("row = %+v, want detector-x 100/20/0.01", rows[0])
	}
	if rows[0].Status != "ok" || rows[0].Profile != "" || rows[0].Error != "" {
		t.Errorf("row defaults = %+v, want status ok, empty profile and error", rows[0])
	}
}

func TestReviewRecentAnalysesScopeAllSeesEverything(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	base := time.Now().Add(-time.Hour)
	convOld := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convOld, seedAnalysis{model: "detector-old", createdAt: base})
	convNew := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convNew, seedAnalysis{model: "detector-new", createdAt: base.Add(time.Minute)})

	rows, err := New(pool).RecentAnalyses(ctx, ScopeAll(), nil, 0)
	if err != nil {
		t.Fatalf("RecentAnalyses: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ScopeAll rows = %+v, want both conversations", rows)
	}
	if rows[0].ConversationID != convNew || rows[1].ConversationID != convOld {
		t.Errorf("order = [%s, %s], want [%s, %s] (most recent first)",
			rows[0].ConversationID, rows[1].ConversationID, convNew, convOld)
	}
}

// TestReviewRecentAnalysesZeroScopeDeniesEverything is the load-bearing test:
// a zero-value Scope DENIES. It must return zero rows without error, never
// behave as "no filter" and leak every analysis.
func TestReviewRecentAnalysesZeroScopeDeniesEverything(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, conv, seedAnalysis{model: "detector-x"})

	rows, err := New(pool).RecentAnalyses(ctx, Scope{}, nil, 0)
	if err != nil {
		t.Fatalf("zero scope must deny, not error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("zero-scope rows = %+v, want none (the zero value denies)", rows)
	}
}

func TestReviewRecentAnalysesConversationIDsNarrowWithinScope(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-a"})
	convB := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-b"})
	convOther := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convOther, seedAnalysis{model: "detector-c"})

	// convB is in scope and named; convOther is named but outside scope, so it
	// contributes no row and no error.
	rows, err := New(pool).RecentAnalyses(ctx, ScopeOwner(alice), []string{convB, convOther}, 0)
	if err != nil {
		t.Fatalf("RecentAnalyses narrowed: %v", err)
	}
	if len(rows) != 1 || rows[0].ConversationID != convB {
		t.Fatalf("narrowed rows = %+v, want exactly convB (%s)", rows, convB)
	}
}

func TestReviewFindingsScopesByOwner(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-x", findings: []seedFinding{
		{axis: "prompt", topicKey: "alice-topic", title: "Alice's finding", expectedSavings: 500},
	}})
	convB := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-x", findings: []seedFinding{
		{axis: "prompt", topicKey: "bob-topic", title: "Bob's finding", expectedSavings: 999},
	}})

	rows, err := New(pool).Findings(ctx, ScopeOwner(alice), FindingsFilter{})
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(rows) != 1 || rows[0].TopicKey != "alice-topic" {
		t.Fatalf("owner-scoped findings = %+v, want exactly alice-topic", rows)
	}
	if rows[0].ConversationID != convA {
		t.Errorf("finding conversation_id = %q, want %q", rows[0].ConversationID, convA)
	}
	if rows[0].Status != "open" {
		t.Errorf("finding status = %q, want open", rows[0].Status)
	}
}

func TestReviewFindingsDefaultStatusIsOpen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, conv, seedAnalysis{model: "detector-x", findings: []seedFinding{
		{axis: "prompt", topicKey: "high", title: "High savings", expectedSavings: 900},
		{axis: "prompt", topicKey: "low", title: "Low savings", expectedSavings: 100},
		{axis: "tools", topicKey: "gone", title: "Dismissed", expectedSavings: 5000, status: "dismissed"},
	}})

	rows, err := New(pool).Findings(ctx, ScopeAll(), FindingsFilter{})
	if err != nil {
		t.Fatalf("Findings default: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("default findings = %+v, want 2 (open only)", rows)
	}
	if rows[0].TopicKey != "high" || rows[1].TopicKey != "low" {
		t.Errorf("order = [%s, %s], want [high, low] (expected_savings_tokens DESC)", rows[0].TopicKey, rows[1].TopicKey)
	}

	dismissed, err := New(pool).Findings(ctx, ScopeAll(), FindingsFilter{Status: "dismissed"})
	if err != nil {
		t.Fatalf("Findings status=dismissed: %v", err)
	}
	if len(dismissed) != 1 || dismissed[0].TopicKey != "gone" {
		t.Fatalf("dismissed findings = %+v, want exactly gone", dismissed)
	}
}

func TestReviewFindingsConversationIDsNarrowsWithinScope(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-a", findings: []seedFinding{
		{axis: "prompt", topicKey: "a-topic", title: "A", expectedSavings: 10},
	}})
	convB := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-b", findings: []seedFinding{
		{axis: "prompt", topicKey: "b-topic", title: "B", expectedSavings: 20},
	}})
	convOther := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convOther, seedAnalysis{model: "detector-c", findings: []seedFinding{
		{axis: "prompt", topicKey: "other-topic", title: "Other", expectedSavings: 30},
	}})

	// convOther is named but outside scope: silently absent, not an error.
	rows, err := New(pool).Findings(ctx, ScopeOwner(alice), FindingsFilter{ConversationIDs: []string{convB, convOther}})
	if err != nil {
		t.Fatalf("Findings narrowed: %v", err)
	}
	if len(rows) != 1 || rows[0].TopicKey != "b-topic" || rows[0].ConversationID != convB {
		t.Fatalf("narrowed findings = %+v, want exactly b-topic on %s", rows, convB)
	}
}

func TestReviewFilterByScopeDropsIDsOutsideScope(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	convB := insertConversation(t, pool, "client", "review-bob")
	missing := "00000000-0000-0000-0000-00000000abcd"

	got, err := New(pool).FilterByScope(ctx, ScopeOwner(alice), []string{convA, convB, missing})
	if err != nil {
		t.Fatalf("FilterByScope owner: %v", err)
	}
	if len(got) != 1 || got[0] != convA {
		t.Fatalf("owner-scoped ids = %v, want exactly [%s] (out-of-scope and missing dropped)", got, convA)
	}

	// ScopeAll admits both owners' conversations; the nonexistent id still
	// drops, and the caller's order is preserved.
	got, err = New(pool).FilterByScope(ctx, ScopeAll(), []string{convB, missing, convA})
	if err != nil {
		t.Fatalf("FilterByScope all: %v", err)
	}
	if len(got) != 2 || got[0] != convB || got[1] != convA {
		t.Fatalf("all-scope ids = %v, want [%s, %s] in input order", got, convB, convA)
	}
}

func TestReviewFilterByScopeZeroScopeReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")

	got, err := New(pool).FilterByScope(ctx, Scope{}, []string{conv})
	if err != nil {
		t.Fatalf("zero scope must deny, not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("zero-scope ids = %v, want empty (the zero value denies)", got)
	}
}

// TestReviewClampLimit pins the server-side limit convention both review reads
// share: 0/negative means the default (50), anything above the cap (500)
// clamps to the cap. Pure, so it cannot silently skip without RAFIKI_TEST_DSN.
func TestReviewClampLimit(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{
		{0, defaultReviewLimit},
		{-1, defaultReviewLimit},
		{-500, defaultReviewLimit},
		{1, 1},
		{defaultReviewLimit, defaultReviewLimit},
		{maxReviewLimit, maxReviewLimit},
		{maxReviewLimit + 1, maxReviewLimit},
		{100000, maxReviewLimit},
	} {
		if got := clampReviewLimit(tc.in); got != tc.want {
			t.Errorf("clampReviewLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
