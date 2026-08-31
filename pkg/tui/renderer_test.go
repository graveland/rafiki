// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
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

	cold := newRenderer().Lines(blocks, len(blocks), 100)

	warm := newRenderer()
	warm.Lines(blocks[:3], 3, 100) // prime
	got := warm.Lines(blocks, len(blocks), 100)

	if strings.Join(got, "\n") != strings.Join(cold, "\n") {
		t.Errorf("warm cache output differs from cold:\nwarm: %q\ncold: %q", got, cold)
	}
}

// TestLinesRebuildsWhenFinalizedShrinks: Finalized moving backwards means the
// transcript was replaced (a hop into a reused renderer, a reset). Appending
// to a stale cache there would splice two children's transcripts together.
func TestLinesRebuildsWhenFinalizedShrinks(t *testing.T) {
	r := newRenderer()
	r.Lines(finalizedBlocks(5), 5, 100)

	short := finalizedBlocks(2)
	got := r.Lines(short, 2, 100)
	want := newRenderer().Lines(short, 2, 100)

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
	var plain strings.Builder
	for i := 1; i <= 500; i++ {
		plain.WriteString("result line " + strconv.Itoa(i) + "\n")
	}
	blocks := []session.Block{{
		Kind:      session.KindAssistant,
		Final:     true,
		ToolCalls: []session.ToolCall{{Name: "grep", Result: plain.String()}},
	}}

	got := newRenderer().Lines(blocks, 1, 100)
	joined := strings.Join(got, "\n")

	if n := strings.Count(joined, "result line"); n > maxToolResultLines {
		t.Errorf("tool result not capped: %d occurrences, cap is %d", n, maxToolResultLines)
	}
	if !strings.Contains(joined, "earlier lines") {
		t.Errorf("capped output must say how much was elided; got:\n%s", joined)
	}
	// The TAIL survives, not the head: a command's ending carries its error,
	// and a long build's last lines are where it has got to.
	if !strings.Contains(joined, "result line 500") {
		t.Errorf("capped output dropped the LAST line; the tail is the part worth keeping:\n%s", joined)
	}
	if strings.Contains(joined, "result line 1\n") {
		t.Errorf("capped output kept the head; it must keep the tail:\n%s", joined)
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

	got := newRenderer().Lines(blocks, 1, 100)

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
	got := newRenderer().Lines(finalizedBlocks(3), 3, 100)
	if len(got) < 3 {
		t.Fatalf("expected at least one line per block, got %d: %q", len(got), got)
	}
	for i, l := range got {
		if strings.Contains(l, "\n") {
			t.Errorf("line %d contains a newline, so it is not one line: %q", i, l)
		}
	}
}

// An empty transcript is NOT a connection state. Lines returned the literal
// string "Connecting…" for any session with no blocks -- which is the steady
// state of a child that has not spoken yet, not a transient one. A freshly
// created agent therefore claimed to be connecting forever while the status
// line two rows below it said "connected". Emptiness is the shell's to
// describe, with the focus state it alone knows; the renderer renders blocks.
func TestEmptyTranscriptRendersNoLines(t *testing.T) {
	got := newRenderer().Lines(nil, 0, 100)
	if len(got) != 0 {
		t.Errorf("Lines(nil, 0) = %q, want no lines", got)
	}
	for _, l := range got {
		if strings.Contains(l, "onnecting") {
			t.Errorf("empty transcript claims to be connecting: %q", l)
		}
	}
}

// Seeing "bash" tells you nothing about whether to abort. The tool's own
// argument goes on the call line, the way pi's per-tool renderCall does it.
func TestToolCallShowsItsArgument(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"bash", `{"command":"go test ./..."}`, "go test ./..."},
		{"read", `{"path":"/tmp/x.go"}`, "/tmp/x.go"},
		{"grep", `{"pattern":"TODO","path":"/src"}`, "TODO"},
		// Unlisted tool: any string field beats showing nothing.
		{"mystery", `{"target":"the-thing"}`, "target=the-thing"},
		// No arguments at all must not become a meaningless "{}".
		{"noargs", `{}`, ""},
	} {
		got := toolArgSummary(tc.name, tc.input)
		if got != tc.want {
			t.Errorf("toolArgSummary(%q, %q) = %q, want %q", tc.name, tc.input, got, tc.want)
		}
	}
}

// A multi-line argument must not unroll into the transcript and bury the
// conversation it is part of.
func TestToolArgumentIsOneBoundedLine(t *testing.T) {
	got := toolArgSummary("bash", `{"command":"`+strings.Repeat("x", 500)+`"}`)
	if strings.Contains(got, "\n") {
		t.Error("argument summary spans lines")
	}
	if len([]rune(got)) > maxToolArgWidth {
		t.Errorf("argument summary is %d runes, cap is %d", len([]rune(got)), maxToolArgWidth)
	}

	multi := toolArgSummary("write", `{"path":"a\nb\nc"}`)
	if strings.Contains(multi, "\n") {
		t.Errorf("multi-line argument was not collapsed: %q", multi)
	}
}

// A guessed tool name degrades silently to the JSON fallback and looks like it
// works, so the map is pinned against the real registry.
func TestToolArgKeysNameRealTools(t *testing.T) {
	for name := range toolArgKeys {
		if _, ok := tools.TierOf(name); !ok {
			t.Errorf("toolArgKeys names %q, which is not a registered tool", name)
		}
	}
}

// The batch tools carry arrays of objects, not strings; their raw JSON is long
// and unreadable and a count is the honest summary.
func TestBatchToolArgumentsSummariseAsACount(t *testing.T) {
	got := toolArgSummary("task_add", `{"items":[{"content":"a"},{"content":"b"},{"content":"c"}]}`)
	if got != "items×3" {
		t.Errorf("toolArgSummary(task_add, 3 items) = %q, want %q", got, "items×3")
	}
}

// Three weights, by gutter rather than by background: pi backgrounds its tool
// calls, which on a working agent is most of the screen. The scarce thing is
// the agent's own prose, so that gets the solid bar, thinking a dotted one, and
// tool calls none at all.
func TestTranscriptWeightsAreDistinguishable(t *testing.T) {
	blocks := []session.Block{{
		Kind:      session.KindAssistant,
		Final:     true,
		Text:      "the prose",
		ThinkText: "the reasoning",
		ToolCalls: []session.ToolCall{{Name: "bash", Input: `{"command":"ls"}`}},
	}}
	lines := newRenderer().Lines(blocks, 1, 100)

	var prose, think, tool string
	for _, l := range lines {
		p := ansi.Strip(l)
		switch {
		case strings.Contains(p, "the prose"):
			prose = p
		case strings.Contains(p, "thinking…"):
			think = p
		case strings.Contains(p, "⚒"):
			tool = p
		}
	}
	if prose == "" || think == "" || tool == "" {
		t.Fatalf("missing a weight: prose=%q think=%q tool=%q", prose, think, tool)
	}
	if !strings.HasPrefix(prose, "▌") {
		t.Errorf("assistant prose lacks the solid gutter: %q", prose)
	}
	if !strings.HasPrefix(think, "┊") {
		t.Errorf("thinking lacks the dotted gutter: %q", think)
	}
	if strings.HasPrefix(tool, "▌") || strings.HasPrefix(tool, "┊") {
		t.Errorf("tool calls must stay unadorned: %q", tool)
	}
}

// A "── tool_use" rule under every block of a tool-calling turn — which is
// most blocks — competes with the content while repeating what the ⚒ line
// already showed. The unusual endings still show.
func TestOnlyInterestingStopReasonsAreShown(t *testing.T) {
	render := func(reason string) string {
		blocks := []session.Block{{
			Kind: session.KindAssistant, Final: true,
			Text: "hi", StopReason: reason,
		}}
		return ansi.Strip(strings.Join(newRenderer().Lines(blocks, 1, 100), "\n"))
	}
	for _, quiet := range []string{"end_turn", "tool_use", "stop", ""} {
		if strings.Contains(render(quiet), "──") {
			t.Errorf("stop reason %q is routine and must not be printed", quiet)
		}
	}
	for _, loud := range []string{"max_tokens", "refusal", "error"} {
		if !strings.Contains(render(loud), loud) {
			t.Errorf("stop reason %q is worth reading and must be printed", loud)
		}
	}
}
