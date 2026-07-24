package analyze

import (
	"cmp"
	"slices"
)

// contributor represents a single finding and its metadata within a group merge.
type contributor struct {
	analysis  *Analysis
	finding   *Finding
	convIndex int // position in sorted conversations list (computed later)
}

// Rank performs deterministic cross-conversation grouping and scoring of findings.
// It groups findings by (Axis, key) where key = SkillName if non-empty else TopicKey,
// merges evidence from all contributors, and assigns a score based on token savings
// and recurrence.
//
// Findings with Recommendation.Kind == "none" are skipped (they represent verdicts
// of "fine as-is", not actionable recommendations).
//
// The returned slice is sorted by Score descending, with ties broken by Title
// ascending, then Axis ascending, then by group key (skill-name-or-topic-key).
// Empty input returns nil (Go convention).
func Rank(analyses []*Analysis) []RankedFinding {
	if len(analyses) == 0 {
		return nil
	}

	// groupKey uniquely identifies a group of findings to be merged.
	type groupKey struct {
		axis string
		key  string
	}

	// Accumulate findings by group key.
	groups := make(map[groupKey][]*contributor)

	for _, analysis := range analyses {
		for i := range analysis.Findings {
			finding := &analysis.Findings[i]

			if finding.Recommendation.Kind == "none" {
				continue
			}

			key := finding.Recommendation.SkillName
			if key == "" {
				key = finding.TopicKey
			}

			gk := groupKey{
				axis: finding.Axis,
				key:  key,
			}

			groups[gk] = append(groups[gk], &contributor{
				analysis:  analysis,
				finding:   finding,
				convIndex: -1, // will be set after we sort conversations
			})
		}
	}

	// Convert groups to ranked findings, tracking group keys for final tiebreaker.
	type rankedWithKey struct {
		ranked  RankedFinding
		gkeyStr string // string representation of group key for tiebreaker
	}
	var rankedWithKeys []rankedWithKey

	for gk, contributors := range groups {
		// Collect distinct conversation IDs and sort them.
		convMap := make(map[string]bool)
		for _, c := range contributors {
			convMap[c.analysis.ConversationID] = true
		}
		conversations := make([]string, 0, len(convMap))
		for conv := range convMap {
			conversations = append(conversations, conv)
		}
		slices.Sort(conversations)

		// Update convIndex for each contributor.
		convToIndex := make(map[string]int)
		for i, conv := range conversations {
			convToIndex[conv] = i
		}
		for _, c := range contributors {
			c.convIndex = convToIndex[c.analysis.ConversationID]
		}

		// Find the representative finding (highest confidence).
		// Ties: first by conversation id, then by title (deterministic).
		best := contributors[0]
		for i := 1; i < len(contributors); i++ {
			curr := contributors[i]
			if curr.finding.Confidence > best.finding.Confidence {
				best = curr
			} else if curr.finding.Confidence == best.finding.Confidence {
				// Tie-break by conversation id, then title.
				if cmp.Compare(curr.analysis.ConversationID, best.analysis.ConversationID) < 0 {
					best = curr
				} else if curr.analysis.ConversationID == best.analysis.ConversationID {
					if cmp.Compare(curr.finding.Title, best.finding.Title) < 0 {
						best = curr
					}
				}
			}
		}

		// Merge evidence from all contributors, sorted by contributor confidence desc.
		// Sort contributors by confidence desc (ties: convIndex asc, title asc for determinism).
		slices.SortFunc(contributors, func(a, b *contributor) int {
			// Confidence desc (negate for reverse).
			if cmp.Compare(a.finding.Confidence, b.finding.Confidence) != 0 {
				return -cmp.Compare(a.finding.Confidence, b.finding.Confidence)
			}
			// Tie-break: convIndex asc.
			if a.convIndex != b.convIndex {
				return cmp.Compare(a.convIndex, b.convIndex)
			}
			// Tie-break: title asc.
			return cmp.Compare(a.finding.Title, b.finding.Title)
		})

		var mergedEvidence []TurnCite
		for _, c := range contributors {
			mergedEvidence = append(mergedEvidence, c.finding.Evidence...)
		}
		if len(mergedEvidence) > 8 {
			mergedEvidence = mergedEvidence[:8]
		}

		// Compute score.
		// For non-grind axes: sum(GrindTokens) + 10_000*Occurrences
		// For grind axis: sum(GrindTokens) + 10_000*(Occurrences-1)
		var grindSum int64
		for _, c := range contributors {
			grindSum += c.finding.GrindTokens
		}

		occurrences := len(conversations)
		var score int64
		if best.finding.Axis == "grind" {
			score = grindSum + int64(10000*(occurrences-1))
		} else {
			score = grindSum + int64(10000*occurrences)
		}

		// GrindTokens in the ranked finding is representative-only; Score carries the sum.
		rf := RankedFinding{
			Finding: Finding{
				Axis:           best.finding.Axis,
				Title:          best.finding.Title,
				TopicKey:       best.finding.TopicKey,
				Evidence:       mergedEvidence,
				Recommendation: best.finding.Recommendation,
				Confidence:     best.finding.Confidence,
				GrindTokens:    best.finding.GrindTokens,
			},
			Conversations: conversations,
			Occurrences:   occurrences,
			Score:         score,
		}

		rankedWithKeys = append(rankedWithKeys, rankedWithKey{
			ranked:  rf,
			gkeyStr: gk.axis + "|" + gk.key,
		})
	}

	// Sort by Score desc, Title asc, Axis asc, then group key asc.
	slices.SortFunc(rankedWithKeys, func(a, b rankedWithKey) int {
		// Score desc (negate for reverse).
		if cmp.Compare(a.ranked.Score, b.ranked.Score) != 0 {
			return -cmp.Compare(a.ranked.Score, b.ranked.Score)
		}
		// Title asc.
		if cmp.Compare(a.ranked.Title, b.ranked.Title) != 0 {
			return cmp.Compare(a.ranked.Title, b.ranked.Title)
		}
		// Axis asc.
		if cmp.Compare(a.ranked.Axis, b.ranked.Axis) != 0 {
			return cmp.Compare(a.ranked.Axis, b.ranked.Axis)
		}
		// Group key asc (unambiguous tiebreaker).
		return cmp.Compare(a.gkeyStr, b.gkeyStr)
	})

	// Extract ranked findings in sorted order.
	if len(rankedWithKeys) == 0 {
		return nil
	}
	result := make([]RankedFinding, len(rankedWithKeys))
	for i, rwk := range rankedWithKeys {
		result[i] = rwk.ranked
	}

	return result
}
