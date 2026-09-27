// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestRankEmptyInput(t *testing.T) {
	c := assert.NewCollecting(t)
	result := Rank(nil)
	c.Nil(result, "Rank(nil)")

	result = Rank([]*Analysis{})
	c.Nil(result, "Rank([]*Analysis{})")
}

func TestRankSkipsKindNone(t *testing.T) {
	analyses := []*Analysis{
		{
			ConversationID: "conv-1",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "missing skill",
					TopicKey: "missing-skill",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "none",
						SkillName: "",
						Summary:   "all good",
					},
					Confidence: 0.9,
				},
			},
		},
	}

	result := Rank(analyses)
	assert.NewCollecting(t).Nil(result, "Rank with kind=none")
}

func TestRankMergesSameSkillAcross3Analyses(t *testing.T) {
	c := assert.NewCollecting(t)
	analyses := []*Analysis{
		{
			ConversationID: "conv-1",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "missing replication lag skill",
					TopicKey: "replication-lag-skill",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "evidence from conv-1"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "sc-diagnose-replication-lag",
						Summary:   "add skill",
					},
					Confidence:  0.85,
					GrindTokens: 100,
				},
			},
		},
		{
			ConversationID: "conv-2",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "missing replication lag skill",
					TopicKey: "replication-lag-skill",
					Evidence: []TurnCite{{Ordinal: 2, Quote: "evidence from conv-2"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "sc-diagnose-replication-lag",
						Summary:   "add skill",
					},
					Confidence:  0.92,
					GrindTokens: 150,
				},
			},
		},
		{
			ConversationID: "conv-3",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "missing replication lag skill",
					TopicKey: "replication-lag-skill",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "evidence from conv-3"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "sc-diagnose-replication-lag",
						Summary:   "add skill",
					},
					Confidence:  0.78,
					GrindTokens: 120,
				},
			},
		},
	}

	result := Rank(analyses)
	c.Require().Len(result, 1, "Rank() returned %d findings, want 1", len(result))

	ranked := result[0]

	// Check Occurrences
	c.Eq(3, ranked.Occurrences, "Occurrences")

	// Check Conversations are sorted and distinct
	expectedConvs := []string{"conv-1", "conv-2", "conv-3"}
	c.EqDiff(expectedConvs, ranked.Conversations, "Conversations")

	// Check representative Finding is highest confidence (0.92 from conv-2)
	c.Eq(0.92, ranked.Confidence, "representative Finding Confidence")
	c.Eq("missing replication lag skill", ranked.Title, "Title")

	// Check Evidence is merged and sorted by contributor confidence desc, capped at 8
	c.Len(ranked.Evidence, 3, "Evidence length = %d, want 3", len(ranked.Evidence))
	// First should be from conv-2 (confidence 0.92), then conv-1 (0.85), then conv-3 (0.78)
	expectedQuotes := []string{"evidence from conv-2", "evidence from conv-1", "evidence from conv-3"}
	for i, cite := range ranked.Evidence {
		c.Eq(expectedQuotes[i], cite.Quote, "Evidence[%d].Quote = %q, want", i, cite.Quote)
	}

	// Check Score = sum(GrindTokens) + 10000*Occurrences for non-grind axis
	expectedScore := int64(100 + 150 + 120 + 10000*3)
	c.Eq(expectedScore, ranked.Score, "Score")
}

func TestRankSortsDeterministic(t *testing.T) {
	c := assert.NewCollecting(t)
	// Create findings in non-deterministic order and verify they sort consistently
	analyses := []*Analysis{
		{
			ConversationID: "conv-z",
			Findings: []Finding{
				{
					Axis:     "knowledge-to-persist",
					Title:    "Beta finding",
					TopicKey: "beta-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "memory",
						SkillName: "",
						Summary:   "persist",
					},
					Confidence:  0.8,
					GrindTokens: 50,
				},
			},
		},
		{
			ConversationID: "conv-a",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "Alpha finding",
					TopicKey: "alpha-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "skill-a",
						Summary:   "new skill",
					},
					Confidence:  0.9,
					GrindTokens: 100,
				},
			},
		},
		{
			ConversationID: "conv-m",
			Findings: []Finding{
				{
					Axis:     "grind",
					Title:    "Grind finding",
					TopicKey: "grind-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "skill-edit",
						SkillName: "skill-b",
						Summary:   "edit skill",
					},
					Confidence:  0.7,
					GrindTokens: 200,
				},
			},
		},
	}

	result := Rank(analyses)
	c.Require().Len(result, 3, "Rank() returned %d findings, want 3", len(result))

	// For non-grind axes: Score = sum(GrindTokens) + 10000*Occurrences
	// Alpha (skill-gap): 100 + 10000*1 = 10100
	// Beta (knowledge-to-persist): 50 + 10000*1 = 10050
	// Grind: 200 + 10000*(1-1) = 200
	// Expected order: Alpha (10100), Beta (10050), Grind (200)

	c.Eq("Alpha finding", result[0].Title, "result[0].Title")
	c.Eq("Beta finding", result[1].Title, "result[1].Title")
	c.Eq("Grind finding", result[2].Title, "result[2].Title")
}

func TestRankGrindScoringFormula(t *testing.T) {
	c := assert.NewCollecting(t)
	// Test that grind axis uses different scoring formula
	analyses := []*Analysis{
		{
			ConversationID: "conv-1",
			Findings: []Finding{
				{
					Axis:     "grind",
					Title:    "Grind 1",
					TopicKey: "grind-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "skill-edit",
						SkillName: "grind-skill",
						Summary:   "edit",
					},
					Confidence:  0.9,
					GrindTokens: 500,
				},
			},
		},
		{
			ConversationID: "conv-2",
			Findings: []Finding{
				{
					Axis:     "grind",
					Title:    "Grind 1",
					TopicKey: "grind-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "skill-edit",
						SkillName: "grind-skill",
						Summary:   "edit",
					},
					Confidence:  0.85,
					GrindTokens: 300,
				},
			},
		},
	}

	result := Rank(analyses)
	c.Require().Len(result, 1, "Rank() returned %d findings, want 1", len(result))

	ranked := result[0]
	// For grind axis: sum(GrindTokens) + 10000*(Occurrences-1)
	// = 500 + 300 + 10000*(2-1) = 800 + 10000 = 10800
	expectedScore := int64(500 + 300 + 10000*1)
	c.Eq(expectedScore, ranked.Score, "Grind Score")
}

func TestRankEvidenceCapped(t *testing.T) {
	c := assert.NewCollecting(t)
	// Create a finding with many contributors to test evidence capping at 8
	analyses := make([]*Analysis, 10)
	for i := 0; i < 10; i++ {
		analyses[i] = &Analysis{
			ConversationID: "conv-" + string(rune('a'+i)),
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "capped evidence",
					TopicKey: "capped-topic",
					Evidence: []TurnCite{{Ordinal: i, Quote: "evidence-" + string(rune('a'+i))}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "test-skill",
						Summary:   "test",
					},
					Confidence:  0.95 - float64(i)*0.01, // confidence decreases with i
					GrindTokens: int64(100),
				},
			},
		}
	}

	result := Rank(analyses)
	c.Require().Len(result, 1, "Rank() returned %d findings, want 1", len(result))

	c.Len(result[0].Evidence, 8, "Evidence length = %d, want 8 (capped)", len(result[0].Evidence))
}

func TestRankTieBreak(t *testing.T) {
	// Test tie-breaking: Score desc, Title asc, Axis asc
	analyses := []*Analysis{
		{
			ConversationID: "conv-1",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "Beta",
					TopicKey: "beta-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "skill-1",
						Summary:   "test",
					},
					Confidence:  0.9,
					GrindTokens: 0, // Score = 0 + 10000*1 = 10000
				},
				{
					Axis:     "skill-gap",
					Title:    "Alpha",
					TopicKey: "alpha-topic",
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "skill-2",
						Summary:   "test",
					},
					Confidence:  0.9,
					GrindTokens: 0, // Score = 0 + 10000*1 = 10000
				},
			},
		},
	}

	result := Rank(analyses)
	assert.NewAborting(t).Len(result, 2, "Rank() returned %d findings, want 2", len(result))

	// Same score (10000), so tie-break by Title asc
	if result[0].Title != "Alpha" || result[1].Title != "Beta" {
		t.Errorf("Tie-break Title order: got [%s, %s], want [Alpha, Beta]", result[0].Title, result[1].Title)
	}
}

func TestRankDeterministicTiebreaker(t *testing.T) {
	c := assert.NewCollecting(t)
	// Create two DIFFERENT groups that share Score, Title, and Axis.
	// They differ only in group key (skill-name-or-topic-key).
	// Verify output is deterministic across multiple runs and permuted input.
	baseAnalyses := []*Analysis{
		{
			ConversationID: "conv-1",
			Findings: []Finding{
				{
					Axis:     "skill-gap",
					Title:    "Same title",
					TopicKey: "aaa-topic", // different topic key
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "aaa-skill",
						Summary:   "test",
					},
					Confidence:  0.9,
					GrindTokens: 0, // Score = 0 + 10000*1 = 10000
				},
				{
					Axis:     "skill-gap",
					Title:    "Same title",
					TopicKey: "zzz-topic", // different topic key
					Evidence: []TurnCite{{Ordinal: 1, Quote: "test"}},
					Recommendation: Recommendation{
						Kind:      "new-skill",
						SkillName: "zzz-skill",
						Summary:   "test",
					},
					Confidence:  0.9,
					GrindTokens: 0, // Score = 0 + 10000*1 = 10000
				},
			},
		},
	}

	// Run Rank multiple times on the same input.
	var results [][]RankedFinding
	for i := 0; i < 3; i++ {
		results = append(results, Rank(baseAnalyses))
	}

	// Verify all runs produce identical output.
	for i := 1; i < len(results); i++ {
		c.Require().Len(results[i], len(results[0]), "Run %d: different length %d vs", i, len(results[i]))
		for j, rf := range results[i] {
			c.Eq(results[0][j].Recommendation.SkillName, rf.Recommendation.SkillName, "Run %d, result[%d]: SkillName = %q, want", i, j, rf.Recommendation.SkillName)
		}
	}

	// Verify the order is deterministic: aaa-skill before zzz-skill (group key asc tiebreaker).
	c.Require().Len(results[0], 2, "expected 2 findings, got %d", len(results[0]))
	c.Eq("aaa-skill", results[0][0].Recommendation.SkillName, "first result: SkillName")
	c.Eq("zzz-skill", results[0][1].Recommendation.SkillName, "second result: SkillName")

	// Run Rank on a permuted copy of the input slice.
	permuted := make([]*Analysis, len(baseAnalyses))
	copy(permuted, baseAnalyses)
	// Shuffle the findings within the first analysis to force map iteration order variation.
	permuted[0].Findings[0], permuted[0].Findings[1] = permuted[0].Findings[1], permuted[0].Findings[0]

	permutedResult := Rank(permuted)
	c.Require().Len(permutedResult, len(results[0]), "permuted run: different length %d vs", len(permutedResult))
	for j, rf := range permutedResult {
		c.Eq(results[0][j].Recommendation.SkillName, rf.Recommendation.SkillName, "permuted run, result[%d]: SkillName = %q, want", j, rf.Recommendation.SkillName)
	}
}
