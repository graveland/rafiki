// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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

	if n := strings.Count(joined, "result line"); n > toolResultHeadLines+toolResultTailLines {
		t.Errorf("tool result not capped: %d occurrences, cap is %d",
			n, toolResultHeadLines+toolResultTailLines)
	}
	if !strings.Contains(joined, "omitted") {
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

// A failed tool call is what you scroll to find. It gets a red bar down its
// ENTIRE height — the call line and every row of output — so it is findable at
// a glance rather than by reading for a ✗ among the ✓s.
func TestFailedToolCallIsMarkedDownItsWholeHeight(t *testing.T) {
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{
			Name: "bash", Input: `{"command":"git rev-list --count HEAD"}`,
			Result:  "spawn refused: no executor satisfies\n  0 live executor(s)",
			IsError: true,
		}},
	}}
	lines := newRenderer().Lines(blocks, 1, 100)

	var marked, total int
	for _, l := range lines {
		p := strings.TrimSpace(ansi.Strip(l))
		if p == "" {
			continue
		}
		total++
		if strings.HasPrefix(p, "▌") {
			marked++
		}
	}
	if total == 0 {
		t.Fatal("nothing rendered")
	}
	if marked != total {
		t.Errorf("%d of %d rows carry the failure bar; every row of a failed call must:\n%s",
			marked, total, strings.Join(lines, "\n"))
	}
	if !strings.Contains(ansi.Strip(strings.Join(lines, "\n")), "✗") {
		t.Error("a failed call must still be marked ✗")
	}
}

// A successful call stays unadorned — the bar has to mean something.
func TestSuccessfulToolCallKeepsNoFailureBar(t *testing.T) {
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{Name: "bash", Result: "ok", IsError: false}},
	}}
	joined := ansi.Strip(strings.Join(newRenderer().Lines(blocks, 1, 100), "\n"))
	if strings.Contains(joined, "▌") {
		t.Errorf("a successful call must carry no failure bar:\n%s", joined)
	}
}

// A call that ended with no result must not claim success. HasResult is not
// the same as a non-empty Result — a tool can legitimately return nothing —
// and without the distinction an interrupted call is indistinguishable from a
// silent success. There are real instances: a production database here holds
// 38 bash calls with no matching tool_result.
func TestToolCallWithNoResultDoesNotClaimSuccess(t *testing.T) {
	none := render(session.ToolCall{Name: "bash"})
	if strings.Contains(none, "✓") {
		t.Errorf("a call with no result claims success:\n%s", none)
	}
	if !strings.Contains(none, "⋯") {
		t.Errorf("a call the turn abandoned must be marked:\n%s", none)
	}
	if strings.Contains(none, "no result") {
		t.Errorf("the verbose 'no result' text is gone; the glyph is the whole marker:\n%s", none)
	}

	// A tool that legitimately returned nothing still succeeded.
	empty := render(session.ToolCall{Name: "bash", HasResult: true})
	if !strings.Contains(empty, "✓") {
		t.Errorf("an empty-but-real result must still read as success:\n%s", empty)
	}
}

// The regression that would have caught the frozen transcript. A tool result
// arriving after the assistant message must appear on the NEXT render.
func TestToolResultArrivingLateIsRendered(t *testing.T) {
	s := session.New("c1")
	s.Apply(&rafikiv1.Event{
		ChildId: "c1",
		Payload: &rafikiv1.Event_AssistantMessage{
			AssistantMessage: &rafikiv1.AssistantMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
						Id: "tu_1", Name: "bash", InputJson: `{"command":"ls"}`,
					}},
				}},
			},
		},
	})

	r := newRenderer()
	first := strings.Join(r.Lines(s.Blocks, s.Finalized, 80), "\n")
	if strings.Contains(first, "MARKER_OUTPUT") {
		t.Fatalf("result present before it arrived:\n%s", first)
	}

	s.Apply(&rafikiv1.Event{
		ChildId: "c1",
		Payload: &rafikiv1.Event_UserMessage{
			UserMessage: &rafikiv1.UserMessage{
				Content: []*rafikiv1.ContentBlock{{
					Index: 0,
					Block: &rafikiv1.ContentBlock_ToolResult{ToolResult: &rafikiv1.ToolResultBlock{
						ToolUseId: "tu_1",
						Content: []*rafikiv1.ContentBlock{{
							Index: 0,
							Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "MARKER_OUTPUT"}},
						}},
					}},
				}},
			},
		},
	})

	second := strings.Join(r.Lines(s.Blocks, s.Finalized, 80), "\n")
	if !strings.Contains(second, "MARKER_OUTPUT") {
		t.Errorf("a tool result that arrived after its assistant message was never rendered:\n%s", second)
	}
}

// With more than one unfinalized block, a change in an EARLIER one must still
// invalidate the live region. Fingerprinting only the last block leaves a
// stale-render hole.
func TestLiveFingerprintCoversEveryUnfinalizedBlock(t *testing.T) {
	blocks := []session.Block{
		{Kind: session.KindAssistant, Final: true,
			ToolCalls: []session.ToolCall{{ID: "a", Name: "bash"}}},
		{Kind: session.KindAssistant, Final: false, Text: "tail"},
	}
	before := session.LiveFingerprint(blocks, 0)
	blocks[0].ToolCalls[0].Result = "changed"
	blocks[0].ToolCalls[0].HasResult = true
	after := session.LiveFingerprint(blocks, 0)
	if before == after {
		t.Error("a change in a non-final block that is not the last one did not change the fingerprint")
	}
}

// render draws one tool call in a finalized assistant block, stripped of ANSI
// so assertions read plain text.
func render(tc session.ToolCall) string {
	return ansi.Strip(strings.Join(newRenderer().Lines([]session.Block{{
		Kind: session.KindAssistant, Final: true, ToolCalls: []session.ToolCall{tc},
	}}, 1, 100), "\n"))
}

// One glyph, one meaning. ⊘ is a BLOCKED TASK in the task box; an abandoned
// tool call is ⋯. Both are on screen at once, so they must not collide.
func TestAbandonedToolCallDoesNotUseTheBlockedTaskGlyph(t *testing.T) {
	out := render(session.ToolCall{Name: "bash"})
	if strings.Contains(out, "⊘") {
		t.Errorf("⊘ means a blocked task; an abandoned tool call must not use it:\n%s", out)
	}
}

// A long result shows both ends. The head carries a command's banner and its
// first error; the tail carries how it ended.
func TestLongToolResultShowsHeadAndTail(t *testing.T) {
	var lines []string
	for i := 1; i <= 300; i++ {
		lines = append(lines, "L"+strconv.Itoa(i))
	}
	out := render(session.ToolCall{
		Name:      "bash",
		HasResult: true,
		Result:    strings.Join(lines, "\n"),
	})

	for _, want := range []string{"L1", "L4", "L289", "L300"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from head/tail window:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"L5", "L150", "L288"} {
		if strings.Contains(out, notWant) {
			t.Errorf("%q should have been elided:\n%s", notWant, out)
		}
	}
	// 300 - 4 - 12 = 284
	if !strings.Contains(out, "[omitted 284 lines]") {
		t.Errorf("missing or wrong omission marker:\n%s", out)
	}
}

// Exactly the budget, and one under it, must not be elided at all.
func TestShortToolResultIsNotElided(t *testing.T) {
	for _, n := range []int{1, 15, 16} {
		var lines []string
		for i := 1; i <= n; i++ {
			lines = append(lines, "L"+strconv.Itoa(i))
		}
		out := render(session.ToolCall{
			Name: "bash", HasResult: true, Result: strings.Join(lines, "\n"),
		})
		if strings.Contains(out, "omitted") {
			t.Errorf("a %d-line result was elided; the budget is 16:\n%s", n, out)
		}
	}
}

// Every argument, not just the one the tool is "about". Seeing only the path
// of an edit tells you nothing about what the edit does.
func TestCompactToolArgsListEveryKey(t *testing.T) {
	got := toolArgLines("edit", `{"path":"src/main.go","old_string":"a","new_string":"b","replace_all":false}`, false)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"old_string", "new_string", "replace_all"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing argument %q:\n%s", want, joined)
		}
	}
	// The headline argument is on the call line already and must not repeat.
	if strings.Contains(joined, "path:") {
		t.Errorf("the headline argument was repeated in the list:\n%s", joined)
	}
}

// Deterministic ordering: ranging a map reorders the list between frames.
func TestToolArgLinesAreSorted(t *testing.T) {
	in := `{"zebra":"z","alpha":"a","monkey":"m"}`
	got := toolArgLines("nosuchtool", in, false)
	joined := strings.Join(got, "\n")
	ia := strings.Index(joined, "alpha")
	im := strings.Index(joined, "monkey")
	iz := strings.Index(joined, "zebra")
	if ia >= im || im >= iz {
		t.Errorf("arguments not sorted by key:\n%s", joined)
	}
}

// Compact folds a multi-line value to one line and says how big it was.
func TestCompactFoldsMultilineValues(t *testing.T) {
	in := `{"path":"n.md","content":"one\ntwo\nthree"}`
	got := toolArgLines("write", in, false)
	joined := strings.Join(got, "\n")
	if strings.Count(joined, "\n") != len(got)-1 {
		t.Errorf("a compact argument line contains a newline:\n%q", joined)
	}
	if !strings.Contains(joined, "B)") {
		t.Errorf("missing size marker on a folded value:\n%s", joined)
	}
}

// Expanded prints the value in full, across lines.
func TestExpandedShowsFullMultilineValues(t *testing.T) {
	in := `{"path":"n.md","content":"one\ntwo\nthree"}`
	joined := strings.Join(toolArgLines("write", in, true), "\n")
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expanded output missing %q:\n%s", want, joined)
		}
	}
}

// ^O reaches the whole transcript, not just the live tail. renderer.Lines
// reuses r.cached for every block below Finalized, so toggling the flag
// without discarding that cache changed nothing a reader could see.
func TestExpandArgsChangesAFinalizedBlock(t *testing.T) {
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{
			ID: "t1", Name: "write", HasResult: true,
			Input: `{"path":"n.md","content":"one\ntwo\nthree"}`,
		}},
	}}

	r := newRenderer()
	r.expandArgs = false
	compact := strings.Join(r.Lines(blocks, 1, 80), "\n")

	// Same renderer, flag flipped, cache discarded the way the cockpit does it.
	r.expandArgs = true
	r.cached, r.cachedUpTo, r.lastFP, r.liveOut = nil, 0, "", nil
	expanded := strings.Join(r.Lines(blocks, 1, 80), "\n")

	if compact == expanded {
		t.Fatalf("expanding a finalized block changed nothing:\n%s", compact)
	}
	if !strings.Contains(expanded, "two") {
		t.Errorf("expanded output is missing the full value:\n%s", expanded)
	}
}
