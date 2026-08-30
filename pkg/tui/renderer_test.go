// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

func finalizedBlocks(n int) []session.Block {
	out := make([]session.Block, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, session.Block{
			Kind:  session.KindUser,
			Text:  "message " + string(rune('a'+i%26)),
			Final: true,
		})
	}
	return out
}

// TestLinesCacheIsTransparent: a warm cache must produce byte-identical output
// to a cold one. The cache is the whole point of this change, and a cache that
// changes what you see is worse than no cache.
func TestLinesCacheIsTransparent(t *testing.T) {
	blocks := finalizedBlocks(5)

	cold := newRenderer().Lines(blocks, len(blocks))

	warm := newRenderer()
	warm.Lines(blocks[:3], 3) // prime
	got := warm.Lines(blocks, len(blocks))

	if strings.Join(got, "\n") != strings.Join(cold, "\n") {
		t.Errorf("warm cache output differs from cold:\nwarm: %q\ncold: %q", got, cold)
	}
}

// TestLinesRebuildsWhenFinalizedShrinks: Finalized moving backwards means the
// transcript was replaced (a hop into a reused renderer, a reset). Appending
// to a stale cache there would splice two children's transcripts together.
func TestLinesRebuildsWhenFinalizedShrinks(t *testing.T) {
	r := newRenderer()
	r.Lines(finalizedBlocks(5), 5)

	short := finalizedBlocks(2)
	got := r.Lines(short, 2)
	want := newRenderer().Lines(short, 2)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("shrinking Finalized did not rebuild:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestLinesCapsToolResults pins the tool-result cap. Without it a 500-line
// grep is glamour-rendered in full on every one of the four frames a second.
//
// Blank-line-separated, not newline-separated: glamour treats a bare "a\nb"
// as one soft-wrapped paragraph and joins it onto a single rendered line
// (verified empirically — a naive strings.Repeat("result line\n", 500)
// collapses to ONE output line and never reaches the cap at all). Separate
// paragraphs are what force genuinely distinct output lines.
func TestLinesCapsToolResults(t *testing.T) {
	huge := strings.Repeat("result line\n\n", 500)
	blocks := []session.Block{{
		Kind:  session.KindAssistant,
		Final: true,
		ToolCalls: []session.ToolCall{{
			Name: "grep", Result: huge,
		}},
	}}

	got := strings.Join(newRenderer().Lines(blocks, 1), "\n")

	if strings.Count(got, "result line") > maxToolResultLines {
		t.Errorf("tool result was not capped: %d occurrences, cap is %d",
			strings.Count(got, "result line"), maxToolResultLines)
	}
	if !strings.Contains(got, "more lines") {
		t.Errorf("capped output must say how much was elided; got:\n%s", got)
	}
}

// TestLinesReturnsLinesNotOneString: Task 7 feeds this to
// viewport.SetContentLines, and a prepend's YOffset shift is exactly
// len(prepended) only if a block's lines are separate elements.
func TestLinesReturnsLinesNotOneString(t *testing.T) {
	got := newRenderer().Lines(finalizedBlocks(3), 3)
	if len(got) < 3 {
		t.Fatalf("expected at least one line per block, got %d: %q", len(got), got)
	}
	for i, l := range got {
		if strings.Contains(l, "\n") {
			t.Errorf("line %d contains a newline, so it is not one line: %q", i, l)
		}
	}
}
