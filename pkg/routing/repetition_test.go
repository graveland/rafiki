// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"fmt"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestRepetitionScoreDeclinesShortText pins the floor: below RepetitionMinBytes
// the ratio says nothing, because short text repeats legitimately ("yes yes
// yes"), and text with no words at all has no ratio to report.
func TestRepetitionScoreDeclinesShortText(t *testing.T) {
	ck := assert.NewAborting(t)

	short := strings.Repeat("word ", 1638) + "x"
	ck.Eq(RepetitionMinBytes-1, len(short), "fixture must be one byte short")
	if score, ok := RepetitionScore(short); ok {
		t.Errorf("RepetitionScore(8191 bytes) = (%v, true), want ok=false", score)
	}

	if score, ok := RepetitionScore(""); ok {
		t.Errorf("RepetitionScore(\"\") = (%v, true), want ok=false", score)
	}
}

// TestRepetitionScoreFlagsALoop pins the case the score exists for: a model
// stuck emitting the same few words scores near zero (prod loops measured
// 0.006 and 0.148).
func TestRepetitionScoreFlagsALoop(t *testing.T) {
	ck := assert.NewAborting(t)

	score, ok := RepetitionScore(strings.Repeat("emit.} go.} now.} ", 1000))
	ck.True(ok, "18KB of loop must be scored")
	ck.Less(0.01, score, "a three-word loop must score near zero")
}

// TestRepetitionScoreConcatenatedLoop pins the separator-less variant. A run of
// "Locked" with no whitespace collapses into ONE word, so on its own it scores
// ~1.0 — the blind spot RepetitionScore's doc comment names, and the reason the
// caller pairs it with other signals. Diluted by a repeated distinct
// vocabulary, that one giant token is just one word among many and the ratio
// lands as low as any other loop's.
func TestRepetitionScoreConcatenatedLoop(t *testing.T) {
	ck := assert.NewAborting(t)

	giant := strings.Repeat("Locked", 2000)
	ck.Greater(RepetitionMinBytes, len(giant), "fixture must clear the floor")
	if score, ok := RepetitionScore(giant); ok {
		ck.Greater(0.9, score, "a separator-less run is one word, so it scores high")
	}

	var b strings.Builder
	b.WriteString(giant)
	for range 20 {
		for i := range 50 {
			fmt.Fprintf(&b, " w%d", i)
		}
	}
	score, ok := RepetitionScore(b.String())
	ck.True(ok, "12KB of text must be scored")
	ck.Less(0.2, score, "a concatenated loop diluted by prose must score low")
}

// TestRepetitionScorePassesProse pins that ordinary long output is not flagged:
// a varied vocabulary repeated at most twice stays well above the ~0.24 floor
// the guard treats as normal.
func TestRepetitionScorePassesProse(t *testing.T) {
	ck := assert.NewAborting(t)

	var b strings.Builder
	for i := range 1500 {
		fmt.Fprintf(&b, "w%d w%d ", i, i)
	}
	ck.Greater(RepetitionMinBytes, b.Len(), "fixture must clear the floor")

	score, ok := RepetitionScore(b.String())
	ck.True(ok, "9KB of prose must be scored")
	ck.Greater(0.4, score, "varied prose must score high")
}
