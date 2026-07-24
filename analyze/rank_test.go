package analyze

import (
	"slices"
	"testing"
)

func TestRankEmptyInput(t *testing.T) {
	result := Rank(nil)
	if result != nil {
		t.Errorf("Rank(nil) = %#v, want nil", result)
	}

	result = Rank([]*Analysis{})
	if result != nil {
		t.Errorf("Rank([]*Analysis{}) = %#v, want nil", result)
	}
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
	if result != nil {
		t.Errorf("Rank with kind=none = %#v, want nil", result)
	}
}

func TestRankMergesSameSkillAcross3Analyses(t *testing.T) {
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
	if len(result) != 1 {
		t.Fatalf("Rank() returned %d findings, want 1", len(result))
	}

	ranked := result[0]

	// Check Occurrences
	if ranked.Occurrences != 3 {
		t.Errorf("Occurrences = %d, want 3", ranked.Occurrences)
	}

	// Check Conversations are sorted and distinct
	expectedConvs := []string{"conv-1", "conv-2", "conv-3"}
	if !slices.Equal(ranked.Conversations, expectedConvs) {
		t.Errorf("Conversations = %v, want %v", ranked.Conversations, expectedConvs)
	}

	// Check representative Finding is highest confidence (0.92 from conv-2)
	if ranked.Confidence != 0.92 {
		t.Errorf("representative Finding Confidence = %v, want 0.92", ranked.Confidence)
	}
	if ranked.Title != "missing replication lag skill" {
		t.Errorf("Title = %q, want 'missing replication lag skill'", ranked.Title)
	}

	// Check Evidence is merged and sorted by contributor confidence desc, capped at 8
	if len(ranked.Evidence) != 3 {
		t.Errorf("Evidence length = %d, want 3", len(ranked.Evidence))
	}
	// First should be from conv-2 (confidence 0.92), then conv-1 (0.85), then conv-3 (0.78)
	expectedQuotes := []string{"evidence from conv-2", "evidence from conv-1", "evidence from conv-3"}
	for i, cite := range ranked.Evidence {
		if cite.Quote != expectedQuotes[i] {
			t.Errorf("Evidence[%d].Quote = %q, want %q", i, cite.Quote, expectedQuotes[i])
		}
	}

	// Check Score = sum(GrindTokens) + 10000*Occurrences for non-grind axis
	expectedScore := int64(100 + 150 + 120 + 10000*3)
	if ranked.Score != expectedScore {
		t.Errorf("Score = %d, want %d", ranked.Score, expectedScore)
	}
}

func TestRankSortsDeterministic(t *testing.T) {
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
	if len(result) != 3 {
		t.Fatalf("Rank() returned %d findings, want 3", len(result))
	}

	// For non-grind axes: Score = sum(GrindTokens) + 10000*Occurrences
	// Alpha (skill-gap): 100 + 10000*1 = 10100
	// Beta (knowledge-to-persist): 50 + 10000*1 = 10050
	// Grind: 200 + 10000*(1-1) = 200
	// Expected order: Alpha (10100), Beta (10050), Grind (200)

	if result[0].Title != "Alpha finding" {
		t.Errorf("result[0].Title = %q, want 'Alpha finding'", result[0].Title)
	}
	if result[1].Title != "Beta finding" {
		t.Errorf("result[1].Title = %q, want 'Beta finding'", result[1].Title)
	}
	if result[2].Title != "Grind finding" {
		t.Errorf("result[2].Title = %q, want 'Grind finding'", result[2].Title)
	}
}

func TestRankGrindScoringFormula(t *testing.T) {
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
	if len(result) != 1 {
		t.Fatalf("Rank() returned %d findings, want 1", len(result))
	}

	ranked := result[0]
	// For grind axis: sum(GrindTokens) + 10000*(Occurrences-1)
	// = 500 + 300 + 10000*(2-1) = 800 + 10000 = 10800
	expectedScore := int64(500 + 300 + 10000*1)
	if ranked.Score != expectedScore {
		t.Errorf("Grind Score = %d, want %d", ranked.Score, expectedScore)
	}
}

func TestRankEvidenceCapped(t *testing.T) {
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
	if len(result) != 1 {
		t.Fatalf("Rank() returned %d findings, want 1", len(result))
	}

	if len(result[0].Evidence) != 8 {
		t.Errorf("Evidence length = %d, want 8 (capped)", len(result[0].Evidence))
	}
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
	if len(result) != 2 {
		t.Fatalf("Rank() returned %d findings, want 2", len(result))
	}

	// Same score (10000), so tie-break by Title asc
	if result[0].Title != "Alpha" || result[1].Title != "Beta" {
		t.Errorf("Tie-break Title order: got [%s, %s], want [Alpha, Beta]", result[0].Title, result[1].Title)
	}
}

func TestRankDeterministicTiebreaker(t *testing.T) {
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
		if len(results[i]) != len(results[0]) {
			t.Fatalf("Run %d: different length %d vs %d", i, len(results[i]), len(results[0]))
		}
		for j, rf := range results[i] {
			if rf.Recommendation.SkillName != results[0][j].Recommendation.SkillName {
				t.Errorf("Run %d, result[%d]: SkillName = %q, want %q",
					i, j, rf.Recommendation.SkillName, results[0][j].Recommendation.SkillName)
			}
		}
	}

	// Verify the order is deterministic: aaa-skill before zzz-skill (group key asc tiebreaker).
	if len(results[0]) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(results[0]))
	}
	if results[0][0].Recommendation.SkillName != "aaa-skill" {
		t.Errorf("first result: SkillName = %q, want aaa-skill", results[0][0].Recommendation.SkillName)
	}
	if results[0][1].Recommendation.SkillName != "zzz-skill" {
		t.Errorf("second result: SkillName = %q, want zzz-skill", results[0][1].Recommendation.SkillName)
	}

	// Run Rank on a permuted copy of the input slice.
	permuted := make([]*Analysis, len(baseAnalyses))
	copy(permuted, baseAnalyses)
	// Shuffle the findings within the first analysis to force map iteration order variation.
	permuted[0].Findings[0], permuted[0].Findings[1] = permuted[0].Findings[1], permuted[0].Findings[0]

	permutedResult := Rank(permuted)
	if len(permutedResult) != len(results[0]) {
		t.Fatalf("permuted run: different length %d vs %d", len(permutedResult), len(results[0]))
	}
	for j, rf := range permutedResult {
		if rf.Recommendation.SkillName != results[0][j].Recommendation.SkillName {
			t.Errorf("permuted run, result[%d]: SkillName = %q, want %q",
				j, rf.Recommendation.SkillName, results[0][j].Recommendation.SkillName)
		}
	}
}
