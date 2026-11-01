// SPDX-License-Identifier: Apache-2.0

package routing

import "strings"

// RepetitionMinBytes is the text length below which RepetitionScore declines
// to score: short text repeats legitimately and says nothing.
const RepetitionMinBytes = 8192

// RepetitionScore is the distinct-word ratio of text: distinct lower-cased
// whitespace-separated words over total words. A repetition loop scores near
// 0 (prod loops: 0.006, 0.148); ordinary long output stays above ~0.24.
// ok is false when text is shorter than RepetitionMinBytes or has no words.
func RepetitionScore(text string) (score float64, ok bool) {
	if len(text) < RepetitionMinBytes {
		return 0, false
	}
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return 0, false
	}
	seen := make(map[string]struct{}, len(words))
	for _, w := range words {
		seen[w] = struct{}{}
	}
	return float64(len(seen)) / float64(len(words)), true
}
