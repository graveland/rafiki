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

// TestLinesCapsToolResultsOnPlainOutput uses the shape tool output actually
// has: plain newline-separated lines, like grep, ls or a stack trace.
//
// This fixture is deliberately the one the plan originally specified. It was
// changed to blank-line-separated during implementation because it did not
// exercise the cap — and that was the right call at the time, but it was
// treating the symptom. The cause was that tool results went through glamour,
// which joins consecutive newline-separated lines into ONE CommonMark
// paragraph: verified empirically, 500 plain lines rendered to exactly 1 line
// while fenced/indented/list input rendered to 500+. So the cap was inert for
// precisely the output it was written for.
//
// Tool output is not markdown. It is rendered preformatted now, which fixes
// both the cap and the loss of line structure.
func TestLinesCapsToolResultsOnPlainOutput(t *testing.T) {
	plain := strings.Repeat("result line\n", 500)
	blocks := []session.Block{{
		Kind:      session.KindAssistant,
		Final:     true,
		ToolCalls: []session.ToolCall{{Name: "grep", Result: plain}},
	}}

	got := newRenderer().Lines(blocks, 1)
	joined := strings.Join(got, "\n")

	if n := strings.Count(joined, "result line"); n > maxToolResultLines {
		t.Errorf("tool result not capped: %d occurrences, cap is %d", n, maxToolResultLines)
	}
	if !strings.Contains(joined, "more lines") {
		t.Errorf("capped output must say how much was elided; got:\n%s", joined)
	}
}

// TestLinesPreservesToolOutputLineStructure is the other half: plain tool
// output must stay one display line per source line. Reflowing a grep result
// into a prose paragraph makes it unreadable, which is what glamour did.
func TestLinesPreservesToolOutputLineStructure(t *testing.T) {
	blocks := []session.Block{{
		Kind:  session.KindAssistant,
		Final: true,
		ToolCalls: []session.ToolCall{{
			Name:   "grep",
			Result: "alpha\nbeta\ngamma",
		}},
	}}

	got := newRenderer().Lines(blocks, 1)

	var hits int
	for _, l := range got {
		if strings.Contains(l, "alpha") || strings.Contains(l, "beta") || strings.Contains(l, "gamma") {
			hits++
		}
	}
	if hits != 3 {
		t.Errorf("three tool output lines collapsed onto %d display lines: %q", hits, got)
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
