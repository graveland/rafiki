// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NoError(err, "insert analysis")
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
	_, err := pool.Exec(context.Background(), `
		INSERT INTO conversations.analysis_finding
			(analysis_id, axis, topic_key, skill_name, title, expected_savings_tokens, status)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
		analysisID, sf.axis, sf.topicKey, skill, sf.title, sf.expectedSavings, status)
	assert.NewAborting(t).NoError(err, "insert finding")
}

func TestReviewRecentAnalysesScopesByOwner(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-x", inTok: 100, outTok: 20, costUSD: 0.01})
	convB := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-x"})

	rows, err := New(pool).RecentAnalyses(ctx, ScopeOwner(alice), nil, 0)
	c.NoError(err, "RecentAnalyses")
	c.False(len(rows) != 1 || rows[0].ConversationID != convA, "owner-scoped rows = %+v, want exactly convA (%s)", rows, convA)
	if rows[0].Model != "detector-x" || rows[0].InputTokens != 100 || rows[0].OutputTokens != 20 || rows[0].CostUSD != 0.01 {
		t.Errorf("row = %+v, want detector-x 100/20/0.01", rows[0])
	}
	if rows[0].Status != "ok" || rows[0].Profile != "" || rows[0].Error != "" {
		t.Errorf("row defaults = %+v, want status ok, empty profile and error", rows[0])
	}
}

func TestReviewRecentAnalysesScopeAllSeesEverything(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	base := time.Now().Add(-time.Hour)
	convOld := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, convOld, seedAnalysis{model: "detector-old", createdAt: base})
	convNew := insertConversation(t, pool, "client", "review-bob")
	insertAnalysis(t, pool, convNew, seedAnalysis{model: "detector-new", createdAt: base.Add(time.Minute)})

	rows, err := New(pool).RecentAnalyses(ctx, ScopeAll(), nil, 0)
	c.NoError(err, "RecentAnalyses")
	c.Len(rows, 2, "ScopeAll rows")
	if rows[0].ConversationID != convNew || rows[1].ConversationID != convOld {
		t.Errorf("order = [%s, %s], want [%s, %s] (most recent first)",
			rows[0].ConversationID, rows[1].ConversationID, convNew, convOld)
	}
}

// TestReviewRecentAnalysesZeroScopeDeniesEverything is the load-bearing test:
// a zero-value Scope DENIES. It must return zero rows without error, never
// behave as "no filter" and leak every analysis.
func TestReviewRecentAnalysesZeroScopeDeniesEverything(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, conv, seedAnalysis{model: "detector-x"})

	rows, err := New(pool).RecentAnalyses(ctx, Scope{}, nil, 0)
	c.NoError(err, "zero scope must deny, not error")
	c.Empty(rows, "zero-scope rows")
}

func TestReviewRecentAnalysesConversationIDsNarrowWithinScope(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(err, "RecentAnalyses narrowed")
	c.False(len(rows) != 1 || rows[0].ConversationID != convB, "narrowed rows = %+v, want exactly convB (%s)", rows, convB)
}

func TestReviewFindingsScopesByOwner(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Findings")
	c.Require().False(len(rows) != 1 || rows[0].TopicKey != "alice-topic", "owner-scoped findings = %+v, want exactly alice-topic", rows)
	c.Eq(convA, rows[0].ConversationID, "finding conversation_id")
	c.Eq("open", rows[0].Status, "finding status")
}

func TestReviewFindingsDefaultStatusIsOpen(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")
	insertAnalysis(t, pool, conv, seedAnalysis{model: "detector-x", findings: []seedFinding{
		{axis: "prompt", topicKey: "high", title: "High savings", expectedSavings: 900},
		{axis: "prompt", topicKey: "low", title: "Low savings", expectedSavings: 100},
		{axis: "tools", topicKey: "gone", title: "Dismissed", expectedSavings: 5000, status: "dismissed"},
	}})

	rows, err := New(pool).Findings(ctx, ScopeAll(), FindingsFilter{})
	c.NoError(err, "Findings default")
	c.Len(rows, 2, "default findings")
	if rows[0].TopicKey != "high" || rows[1].TopicKey != "low" {
		t.Errorf("order = [%s, %s], want [high, low] (expected_savings_tokens DESC)", rows[0].TopicKey, rows[1].TopicKey)
	}

	dismissed, err := New(pool).Findings(ctx, ScopeAll(), FindingsFilter{Status: "dismissed"})
	c.NoError(err, "Findings status=dismissed")
	c.False(len(dismissed) != 1 || dismissed[0].TopicKey != "gone", "dismissed findings = %+v, want exactly gone", dismissed)
}

func TestReviewFindingsConversationIDsNarrowsWithinScope(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(err, "Findings narrowed")
	c.False(len(rows) != 1 || rows[0].TopicKey != "b-topic" || rows[0].ConversationID != convB, "narrowed findings = %+v, want exactly b-topic on %s", rows, convB)
}

func TestReviewFilterByScopeDropsIDsOutsideScope(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	convB := insertConversation(t, pool, "client", "review-bob")
	missing := "00000000-0000-0000-0000-00000000abcd"

	// A UUID input is its own spelling and its own canonical.
	canonical, spellings, err := New(pool).FilterByScope(ctx, ScopeOwner(alice), []string{convA, convB, missing})
	c.NoError(err, "FilterByScope owner")
	c.False(len(canonical) != 1 || canonical[0] != convA || len(spellings) != 1 || spellings[0] != convA, "owner-scoped ids = %v/%v, want exactly [%s] (out-of-scope and missing dropped)", canonical, spellings, convA)

	// ScopeAll admits both owners' conversations; the nonexistent id still
	// drops, and the caller's order is preserved.
	canonical, spellings, err = New(pool).FilterByScope(ctx, ScopeAll(), []string{convB, missing, convA})
	c.NoError(err, "FilterByScope all")
	c.False(len(canonical) != 2 || canonical[0] != convB || canonical[1] != convA ||
		len(spellings) != 2 || spellings[0] != convB || spellings[1] != convA, "all-scope ids = %v/%v, want [%s, %s] in input order", canonical, spellings, convB, convA)
}

func TestReviewFilterByScopeZeroScopeReturnsEmpty(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")

	canonical, spellings, err := New(pool).FilterByScope(ctx, Scope{}, []string{conv})
	c.NoError(err, "zero scope must deny, not error")
	c.False(len(canonical) != 0 || len(spellings) != 0, "zero-scope ids = %v/%v, want empty (the zero value denies)", canonical, spellings)
}

// insertChildRow seeds a conversations.child row pointing at convID -- the
// authoritative mapping the review reads match child ids through. Nothing in
// pkg/insights needed one before the two-arm id match landed.
func insertChildRow(t *testing.T, pool *pgxpool.Pool, childID, convID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO conversations.child (child_id, kind, status, spawned_at, conversation_id)
		VALUES ($1, 'fundi', 'exited', now(), $2::uuid)`,
		childID, convID)
	assert.NewAborting(t).NoError(err, "insert child row %s", childID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM conversations.child WHERE child_id = $1`, childID)
	})
}

// The child-id arm in FilterByScope: the client's Resolve produces child ids,
// so a conversation must be admitted through conversations.child and
// canonicalized to its uuid, while another owner's child id and an id that
// matches neither arm (garbage, a nonexistent uuid) drop silently. A real
// UUID is unchanged: its own spelling, its own canonical.
func TestReviewFilterByScopeAdmitsChildIDs(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	convB := insertConversation(t, pool, "client", "review-bob")
	childA, childB := "c_reviewchildA", "c_reviewchildB"
	insertChildRow(t, pool, childA, convA)
	insertChildRow(t, pool, childB, convB)
	missing := "00000000-0000-0000-0000-00000000abcd"
	garbage := "not-an-id-at-all"

	canonical, spellings, err := New(pool).FilterByScope(ctx, ScopeOwner(alice),
		[]string{childA, childB, garbage, missing, convA})
	c.NoError(err, "FilterByScope")
	c.False(len(canonical) != 2 || len(spellings) != 2, "canonical=%v spellings=%v, want exactly two survivors", canonical, spellings)
	if spellings[0] != childA || canonical[0] != convA {
		t.Fatalf("first survivor = (%q, %q), want (%q, %q) -- the child id canonicalized to its conversation",
			spellings[0], canonical[0], childA, convA)
	}
	if spellings[1] != convA || canonical[1] != convA {
		t.Fatalf("second survivor = (%q, %q), want the uuid admitted as its own canonical", spellings[1], canonical[1])
	}

	// ScopeAll admits the other owner's child id too, still canonicalized,
	// and the garbage id stays a silent drop.
	canonical, spellings, err = New(pool).FilterByScope(ctx, ScopeAll(), []string{childB, garbage})
	c.NoError(err, "FilterByScope all")
	c.False(len(canonical) != 1 || canonical[0] != convB || len(spellings) != 1 || spellings[0] != childB, "all-scope = canonical %v spellings %v, want %q canonicalized to %q", canonical, spellings, childB, convB)
}

// The zero-value scope denies through the child arm too, without error --
// and a garbage id is a silent drop there, not the loud failure the
// pre-two-arm read gave for any non-UUID.
func TestReviewFilterByScopeZeroScopeDeniesChildIDs(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	conv := insertConversation(t, pool, "client", "review-alice")
	child := "c_reviewzerochild"
	insertChildRow(t, pool, child, conv)

	canonical, spellings, err := New(pool).FilterByScope(ctx, Scope{}, []string{child, "junk"})
	c.NoError(err, "zero scope must deny, not error")
	c.False(len(canonical) != 0 || len(spellings) != 0, "zero-scope = %v/%v, want empty (the zero value denies)", canonical, spellings)
}

// The child-id arm through Findings: the filter matches through the child
// mapping (the conversation join runs through conversation_analysis here),
// another owner's child id contributes nothing, and a real uuid still works
// in the same request.
func TestReviewFindingsChildIDArm(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertChildRow(t, pool, "c_findchildA", convA)
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-a", findings: []seedFinding{
		{axis: "prompt", topicKey: "a-topic", title: "A", expectedSavings: 10},
	}})
	convB := insertConversation(t, pool, "client", "review-bob")
	insertChildRow(t, pool, "c_findchildB", convB)
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-b", findings: []seedFinding{
		{axis: "prompt", topicKey: "b-topic", title: "B", expectedSavings: 20},
	}})

	rows, err := New(pool).Findings(ctx, ScopeOwner(alice), FindingsFilter{ConversationIDs: []string{"c_findchildA"}})
	c.NoError(err, "Findings child id")
	c.False(len(rows) != 1 || rows[0].TopicKey != "a-topic", "child-id findings = %+v, want exactly a-topic", rows)

	// Mixed arms in one request: bob's child id and the junk id contribute
	// nothing, alice's uuid still matches.
	rows, err = New(pool).Findings(ctx, ScopeAll(), FindingsFilter{ConversationIDs: []string{"c_findchildB", convA, "junk"}})
	c.NoError(err, "Findings mixed")
	c.Len(rows, 2, "mixed findings")
}

// The child-id arm through RecentAnalyses, where the row IS the analysis.
func TestReviewRecentAnalysesChildIDArm(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	alice := ensureUser(t, pool, "review-alice")
	convA := insertConversation(t, pool, "client", "review-alice")
	insertChildRow(t, pool, "c_rachildA", convA)
	insertAnalysis(t, pool, convA, seedAnalysis{model: "detector-a"})
	convB := insertConversation(t, pool, "client", "review-bob")
	insertChildRow(t, pool, "c_rachildB", convB)
	insertAnalysis(t, pool, convB, seedAnalysis{model: "detector-b"})

	rows, err := New(pool).RecentAnalyses(ctx, ScopeOwner(alice), []string{"c_rachildA"}, 0)
	c.NoError(err, "RecentAnalyses child id")
	c.False(len(rows) != 1 || rows[0].ConversationID != convA, "child-id rows = %+v, want convA (%s) canonicalized", rows, convA)

	// Zero scope + child id: denied, no error.
	rows, err = New(pool).RecentAnalyses(ctx, Scope{}, []string{"c_rachildA"}, 0)
	c.NoError(err, "zero scope must deny, not error")
	c.Empty(rows, "zero-scope rows")
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
		got := clampReviewLimit(tc.in)
		assert.NewCollecting(t).Eq(tc.want, got, "clampReviewLimit(%d) = %d, want", tc.in, got)
	}
}
