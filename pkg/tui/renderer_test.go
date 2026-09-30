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

	"github.com/multigres/testkit/assert"
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

	assert.NewCollecting(t).Eq(strings.Join(cold, "\n"), strings.Join(got, "\n"), "warm cache output differs from cold:\nwarm: %q\ncold: %q", got, cold)
}

// A user message's text is styled AFTER wrapping, one physical row at a time
// -- not styled as one span before wrapping. ansi.Wordwrap does not reapply
// an opening SGR code on a continuation row it creates by breaking a single
// already-styled span, so styling first left every wrapped continuation row,
// and every logical line but the message's first, with no colour at all.
// This exercises both sources of a continuation row: an embedded newline in
// the prompt, and a single logical line long enough to wrap on its own.
func TestUserMessageEveryPhysicalRowIsStyled(t *testing.T) {
	c := assert.NewCollecting(t)
	r := newRenderer()
	r.width = 30
	b := session.Block{Kind: session.KindUser, Text: "first line\n" +
		"a second line long enough that it must wrap across more than one physical row at this width"}
	out := r.renderBlock(b)

	lines := strings.Split(strings.TrimPrefix(out, "\n"), "\n")
	c.Require().GreaterOrEqual(3, len(lines), "test setup: expected at least 3 physical rows (1 short line + a wrapped long one), got %d: %q", len(lines), lines)
	for i, l := range lines {
		c.StrContains(l, "\x1b[1;36m", "row %d = %q, want it to carry its own opening style code -- a row with none renders unstyled", i, l)
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

	assert.NewCollecting(t).Eq(strings.Join(want, "\n"), strings.Join(got, "\n"), "shrinking Finalized did not rebuild:\ngot:  %q\nwant: %q", got, want)
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
	c := assert.NewCollecting(t)
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

	c.LessOrEqual(toolResultHeadLines+toolResultTailLines, strings.Count(joined, "result line"), "tool result not capped")
	c.StrContains(joined, "omitted", "capped output must say how much was elided; got:\n")
	// The TAIL survives, not the head: a command's ending carries its error,
	// and a long build's last lines are where it has got to.
	c.StrContains(joined, "result line 500", "capped output dropped the LAST line; the tail is the part worth keeping:\n")
	c.NotStrContains(joined, "result line 1\n", "capped output kept the head; it must keep the tail:\n")
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
	assert.NewCollecting(t).Eq(3, hits, "three tool output lines collapsed onto %d display lines: %q", hits, got)
}

// A tool result is arbitrary bytes, and this shape actually occurred: a
// 1029-commit git rebase prints progress with bare \r, and a result that once
// talked to a tty carries ANSI cursor movement. The wrapper preserves the
// sequences it skips over, so they used to sail into the terminal and repaint
// the cockpit's own frame from inside the transcript. The renderer's own
// lipgloss styling legitimately carries escapes — what must be gone is the
// INPUT's: a carriage return (lipgloss never emits one) and the sequences the
// result carried in, which can only appear immediately adjacent to the text
// they decorated. Observed 2026-09-09.
func TestToolResultControlCharsNeverReachTheTerminal(t *testing.T) {
	c := assert.NewCollecting(t)
	res := "Rebasing (1/1029)\rRebasing (2/1029)\x1b[2K\x1b[1merror:\x1b[0m could not apply a1b2c3"
	blocks := []session.Block{{
		Kind:      session.KindAssistant,
		Final:     true,
		ToolCalls: []session.ToolCall{{Name: "bash", Result: res}},
	}}

	got := strings.Join(newRenderer().Lines(blocks, 1, 100), "\n")

	c.NotStrContains(got, "\r", "a carriage return reached the terminal:\n")
	c.False(strings.Contains(got, "\x1b[2K") || strings.Contains(got, "\x1b[1merror") || strings.Contains(got, "apply\x1b[0m"), "the result's own escape sequences survived:\n%q", got)
	c.StrContains(got, "Rebasing (2/1029)", "the CR-folded progress line was lost:\n")
	c.StrContains(got, "error: could not apply", "stripped text was lost:\n")
}

// The command line is transcript content too: a command carrying an escape or
// a CR must not reach the terminal any more than a result carrying one.
func TestToolArgControlCharsNeverReachTheTerminal(t *testing.T) {
	c := assert.NewCollecting(t)
	blocks := []session.Block{{
		Kind:  session.KindAssistant,
		Final: true,
		ToolCalls: []session.ToolCall{{
			Name:      "bash",
			Input:     `{"command":"printf '\u001b[31mred\u001b[0m'\r"}`,
			Result:    "red",
			HasResult: true,
		}},
	}}

	got := strings.Join(newRenderer().Lines(blocks, 1, 100), "\n")
	c.NotStrContains(got, "\r", "a carriage return reached the terminal from the command line:\n")
	c.False(strings.Contains(got, "\x1b[31mred") || strings.Contains(got, "red\x1b[0m"), "the command's own escape sequences survived:\n%q", got)
	c.StrContains(got, "printf 'red'", "the stripped command text was lost:\n")
}

// sanitizeControlChars is the byte-level contract the two tests above pin
// through the renderer: CR folds (both CRLF and bare), escapes strip, other
// C0 controls and DEL drop, tab and newline survive.
func TestSanitizeControlChars(t *testing.T) {
	got := sanitizeControlChars("a\r\nb\rc\x1b[2KD\x1b[0m\x07e\x7ff\tg")
	assert.NewCollecting(t).Eq("a\nb\ncDef\tg", got, "sanitizeControlChars")
}

// TestLinesReturnsLinesNotOneString: Task 7 feeds this to
// viewport.SetContentLines, and a prepend's YOffset shift is exactly
// len(prepended) only if a block's lines are separate elements.
func TestLinesReturnsLinesNotOneString(t *testing.T) {
	c := assert.NewCollecting(t)
	got := newRenderer().Lines(finalizedBlocks(3), 3, 100)
	c.Require().GreaterOrEqual(3, len(got), "expected at least one line per block, got %d: %q", len(got), got)
	for i, l := range got {
		c.NotStrContains(l, "\n", "line %d contains a newline, so it is not one line", i)
	}
}

// An empty transcript is NOT a connection state. Lines returned the literal
// string "Connecting…" for any session with no blocks -- which is the steady
// state of a child that has not spoken yet, not a transient one. A freshly
// created agent therefore claimed to be connecting forever while the status
// line two rows below it said "connected". Emptiness is the shell's to
// describe, with the focus state it alone knows; the renderer renders blocks.
func TestEmptyTranscriptRendersNoLines(t *testing.T) {
	c := assert.NewCollecting(t)
	got := newRenderer().Lines(nil, 0, 100)
	c.Empty(got, "Lines(nil, 0)")
	for _, l := range got {
		c.NotStrContains(l, "onnecting", "empty transcript claims to be connecting")
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
		got := toolArgSummary(tc.name, tc.input, maxToolArgWidth)
		assert.NewCollecting(t).Eq(tc.want, got, "toolArgSummary(%q, %q, maxToolArgWidth) = %q, want", tc.name, tc.input, got)
	}
}

// A multi-line argument must not unroll into the transcript and bury the
// conversation it is part of.
func TestToolArgumentIsOneBoundedLine(t *testing.T) {
	c := assert.NewCollecting(t)
	got := toolArgSummary("bash", `{"command":"`+strings.Repeat("x", 500)+`"}`, maxToolArgWidth)
	c.NotStrContains(got, "\n", "argument summary spans lines")
	c.LessOrEqual(maxToolArgWidth, len([]rune(got)), "argument summary is")

	multi := toolArgSummary("write", `{"path":"a\nb\nc"}`, maxToolArgWidth)
	c.NotStrContains(multi, "\n", "multi-line argument was not collapsed")
}

// A guessed tool name degrades silently to the JSON fallback and looks like it
// works, so the map is pinned against the real registry.
func TestToolArgKeysNameRealTools(t *testing.T) {
	for name := range toolArgKeys {
		_, ok := tools.TierOf(name)
		assert.NewCollecting(t).True(ok, "toolArgKeys names %q, which is not a registered tool", name)
	}
}

func TestToolArgKeysNameRealSchemaProperties(t *testing.T) {
	c := assert.NewCollecting(t)
	// The value side of toolArgKeys was never checked, so agent_send/view/kill
	// all carried "child_id" while every one of those tools declares "agent".
	want := map[string][]string{
		"agent_send": {"agent"},
		"agent_view": {"agent"},
		"agent_kill": {"agent"},
	}
	for tool, keys := range want {
		got, ok := toolArgKeys[tool]
		c.Require().True(ok, "toolArgKeys has no entry for %q", tool)
		c.Eq(keys[0], got[0], "toolArgKeys[%q][0] = %q, want", tool, got[0])
	}
}

// The batch tools carry arrays of objects, not strings; their raw JSON is long
// and unreadable and a count is the honest summary.
func TestBatchToolArgumentsSummariseAsACount(t *testing.T) {
	got := toolArgSummary("task_add", `{"items":[{"content":"a"},{"content":"b"},{"content":"c"}]}`, maxToolArgWidth)
	assert.NewCollecting(t).Eq("items×3", got, "toolArgSummary(task_add, 3 items, maxToolArgWidth)")
}

// Three weights, by gutter rather than by background: pi backgrounds its tool
// calls, which on a working agent is most of the screen. The scarce thing is
// the agent's own prose, so that gets the solid bar, thinking a dotted one, and
// tool calls none at all.
func TestTranscriptWeightsAreDistinguishable(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().False(prose == "" || think == "" || tool == "", "missing a weight: prose=%q think=%q tool=%q", prose, think, tool)
	if !strings.HasPrefix(prose, "▌") {
		t.Errorf("assistant prose lacks the solid gutter: %q", prose)
	}
	if !strings.HasPrefix(think, "┊") {
		t.Errorf("thinking lacks the dotted gutter: %q", think)
	}
	c.False(strings.HasPrefix(tool, "▌") || strings.HasPrefix(tool, "┊"), "tool calls must stay unadorned: %q", tool)
}

// A "── tool_use" rule under every block of a tool-calling turn — which is
// most blocks — competes with the content while repeating what the ⚒ line
// already showed. The unusual endings still show.
func TestOnlyInterestingStopReasonsAreShown(t *testing.T) {
	c := assert.NewCollecting(t)
	render := func(reason string) string {
		blocks := []session.Block{{
			Kind: session.KindAssistant, Final: true,
			Text: "hi", StopReason: reason,
		}}
		return ansi.Strip(strings.Join(newRenderer().Lines(blocks, 1, 100), "\n"))
	}
	for _, quiet := range []string{"end_turn", "tool_use", "stop", ""} {
		c.NotStrContains(render(quiet), "──", "stop reason %q is routine and must not be printed", quiet)
	}
	for _, loud := range []string{"max_tokens", "refusal", "error"} {
		c.StrContains(render(loud), loud, "stop reason")
	}
}

// A failed tool call is what you scroll to find. It gets a red bar down its
// ENTIRE height — the call line and every row of output — so it is findable at
// a glance rather than by reading for a ✗ among the ✓s.
func TestFailedToolCallIsMarkedDownItsWholeHeight(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NotEq(0, total, "nothing rendered")
	c.Eq(total, marked, "%d of %d rows carry the failure bar; every row of a failed call must:\n%s", marked, total, strings.Join(lines, "\n"))
	c.StrContains(ansi.Strip(strings.Join(lines, "\n")), "✗", "a failed call must still be marked ✗")
}

// A successful call stays unadorned — the bar has to mean something.
func TestSuccessfulToolCallKeepsNoFailureBar(t *testing.T) {
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{Name: "bash", Result: "ok", IsError: false}},
	}}
	joined := ansi.Strip(strings.Join(newRenderer().Lines(blocks, 1, 100), "\n"))
	assert.NewCollecting(t).NotStrContains(joined, "▌", "a successful call must carry no failure bar:\n")
}

// A call that ended with no result must not claim success. HasResult is not
// the same as a non-empty Result — a tool can legitimately return nothing —
// and without the distinction an interrupted call is indistinguishable from a
// silent success. There are real instances: a production database here holds
// 38 bash calls with no matching tool_result.
func TestToolCallWithNoResultDoesNotClaimSuccess(t *testing.T) {
	c := assert.NewCollecting(t)
	none := render(session.ToolCall{Name: "bash"})
	c.NotStrContains(none, "✓", "a call with no result claims success:\n")
	c.StrContains(none, "⋯", "a call the turn abandoned must be marked:\n")
	c.NotStrContains(none, "no result", "the verbose 'no result' text is gone; the glyph is the whole marker:\n")

	// A tool that legitimately returned nothing still succeeded.
	empty := render(session.ToolCall{Name: "bash", HasResult: true})
	c.StrContains(empty, "✓", "an empty-but-real result must still read as success:\n")
}

// The regression that would have caught the frozen transcript. A tool result
// arriving after the assistant message must appear on the NEXT render.
func TestToolResultArrivingLateIsRendered(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NotStrContains(first, "MARKER_OUTPUT", "result present before it arrived:\n")

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
	c.StrContains(second, "MARKER_OUTPUT", "a tool result that arrived after its assistant message was never rendered:\n")
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
	assert.NewCollecting(t).NotEq(after, before, "a change in a non-final block that is not the last one did not change the fingerprint")
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
	assert.NewCollecting(t).NotStrContains(out, "⊘", "⊘ means a blocked task; an abandoned tool call must not use it:\n")
}

// A long result shows both ends. The head carries a command's banner and its
// first error; the tail carries how it ended.
func TestLongToolResultShowsHeadAndTail(t *testing.T) {
	c := assert.NewCollecting(t)
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
		c.StrContains(out, want, "missing")
	}
	for _, notWant := range []string{"L5", "L150", "L288"} {
		c.NotStrContains(out, notWant, "%q should have been elided:\n", notWant)
	}
	// 300 - 4 - 12 = 284
	c.StrContains(out, "[omitted 284 lines]", "missing or wrong omission marker:\n")
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
		assert.NewCollecting(t).NotStrContains(out, "omitted", "a %d-line result was elided; the budget is 16:\n", n)
	}
}

// Every argument, not just the one the tool is "about". Seeing only the path
// of an edit tells you nothing about what the edit does.
func TestCompactToolArgsListEveryKey(t *testing.T) {
	c := assert.NewCollecting(t)
	got := toolArgLines("edit", `{"path":"src/main.go","old_string":"a","new_string":"b","replace_all":false}`, false, maxToolArgWidth)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"old_string", "new_string", "replace_all"} {
		c.StrContains(joined, want, "missing argument")
	}
	// The headline argument is on the call line already and must not repeat.
	c.NotStrContains(joined, "path:", "the headline argument was repeated in the list:\n")
}

// Deterministic ordering: ranging a map reorders the list between frames.
func TestToolArgLinesAreSorted(t *testing.T) {
	in := `{"zebra":"z","alpha":"a","monkey":"m"}`
	got := toolArgLines("nosuchtool", in, false, maxToolArgWidth)
	joined := strings.Join(got, "\n")
	ia := strings.Index(joined, "alpha")
	im := strings.Index(joined, "monkey")
	iz := strings.Index(joined, "zebra")
	assert.NewCollecting(t).False(ia >= im || im >= iz, "arguments not sorted by key:\n%s", joined)
}

// Compact folds a multi-line value to one line and says how big it was.
func TestCompactFoldsMultilineValues(t *testing.T) {
	c := assert.NewCollecting(t)
	in := `{"path":"n.md","content":"one\ntwo\nthree"}`
	got := toolArgLines("write", in, false, maxToolArgWidth)
	joined := strings.Join(got, "\n")
	c.Eq(len(got)-1, strings.Count(joined, "\n"), "a compact argument line contains a newline:\n%q", joined)
	c.StrContains(joined, "B)", "missing size marker on a folded value:\n")
}

// Expanded prints the value in full, across lines.
func TestExpandedShowsFullMultilineValues(t *testing.T) {
	in := `{"path":"n.md","content":"one\ntwo\nthree"}`
	joined := strings.Join(toolArgLines("write", in, true, maxToolArgWidth), "\n")
	for _, want := range []string{"one", "two", "three"} {
		assert.NewCollecting(t).StrContains(joined, want, "expanded output missing")
	}
}

// ^O reaches the whole transcript, not just the live tail. renderer.Lines
// reuses r.cached for every block below Finalized, so toggling the flag
// without discarding that cache changed nothing a reader could see.
func TestExpandArgsChangesAFinalizedBlock(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Require().NotEq(expanded, compact, "expanding a finalized block changed nothing:\n")
	c.StrContains(expanded, "two", "expanded output is missing the full value:\n")
}

// bash's `command` is its ONLY argument, so skipping the headline key in both
// modes made the full command unreachable from the cockpit: truncated on the
// call line, and absent from the detail list. ^O exists to fix exactly this.
func TestExpandShowsTheHeadlineArgumentInFull(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := "echo " + strings.Repeat("alpha beta gamma delta ", 12)
	input := `{"command":"` + cmd + `"}`

	compact := strings.Join(toolArgLines("bash", input, false, maxToolArgWidth), "\n")
	c.NotStrContains(compact, "command", "compact must leave the headline on the call line:\n")

	expanded := strings.Join(toolArgLines("bash", input, true, maxToolArgWidth), "\n")
	c.Require().StrContains(expanded, "command", "expanded dropped bash's only argument:\n")
	// The tail of the command, which the call line's truncation cuts off.
	c.StrContains(expanded, "delta", "expanded is still truncating the command:\n")
}

// A wide pane must not be capped at the narrow-pane floor. bash's command IS
// the call, so throwing away half a 200-column terminal loses the part of a
// long command that says what it actually did.
func TestArgBudgetGrowsWithThePane(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(maxToolArgWidth, argBudget(80, "bash"), "argBudget(80)")
	wide := argBudget(200, "bash")
	c.Greater(maxToolArgWidth, wide, "argBudget(200)")

	long := "echo " + strings.Repeat("x y ", 60)
	got := toolArgSummary("bash", `{"command":"`+long+`"}`, wide)
	c.Greater(maxToolArgWidth, len([]rune(got)), "a wide pane still truncated at the floor")
}

// Thinking was truncate(ThinkText, 120) -- one line, cut mid-sentence, which
// is where the reasoning gets interesting. It is bounded in wrapped ROWS now,
// keeping both ends, and ^O lifts the bound entirely.
func TestThinkingIsBoundedByRowsAndFullyShownWhenExpanded(t *testing.T) {
	c := assert.NewCollecting(t)
	think := strings.TrimSpace(strings.Repeat("The user wants a test command. ", 60))
	blocks := []session.Block{{Kind: session.KindAssistant, Final: true, ThinkText: think}}

	r := newRenderer()
	compact := strings.Join(r.Lines(blocks, 1, 80), "\n")
	c.StrContains(compact, "[omitted", "a long thinking block must be elided, not silently cut:\n")
	n := strings.Count(compact, "\n")
	c.LessOrEqual(thinkHeadRows+thinkTailRows+4, n, "thinking took %d rows, budget is %d+%d plus chrome", n, thinkHeadRows, thinkTailRows)

	r2 := newRenderer()
	r2.expandArgs = true
	expanded := strings.Join(r2.Lines(blocks, 1, 80), "\n")
	c.NotStrContains(expanded, "[omitted", "^O must show the whole thinking block:\n")
	c.Greater(len(compact), len(expanded), "expanded thinking is not longer than the elided form")
}

// The old cap was 120 characters on one line. A short thinking block must not
// be elided at all, and a normal one must survive past 120 characters.
func TestShortThinkingIsNotElided(t *testing.T) {
	c := assert.NewCollecting(t)
	think := "The user wants me to run a test bash command with a good number of arguments. " +
		"Let me run something with many arguments so the rendering is exercised properly."
	blocks := []session.Block{{Kind: session.KindAssistant, Final: true, ThinkText: think}}
	out := strings.Join(newRenderer().Lines(blocks, 1, 100), "\n")
	c.NotStrContains(out, "[omitted", "a two-sentence thinking block was elided:\n")
	// The tail, which truncate(_, 120) cut off.
	c.StrContains(out, "properly", "thinking is still cut at 120 characters:\n")
}

// Images ride the block's own gutter, and for a user block they come BEFORE
// the text — sendWith's block order is image-first, so the transcript must
// read the same way the message was built.
func TestImageRendersBeforeUserText(t *testing.T) {
	c := assert.NewCollecting(t)
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
	r := newRenderer()
	r.width = 80
	out := r.renderBlock(session.Block{
		Kind: session.KindUser, Text: "look", Images: []*rafikiv1.ImageBlock{img},
	})

	var imgAt, textAt int
	found := false
	for i, l := range strings.Split(ansi.Strip(out), "\n") {
		if strings.Contains(l, "image/png") {
			imgAt, found = i, true
		}
		if strings.Contains(l, "look") {
			textAt = i
		}
	}
	c.Require().True(found, "no image line in:\n%s", out)
	c.Require().True(textAt > imgAt, "the image must render before the text: image at row %d, text at row %d:\n%s", imgAt, textAt, out)
	for _, l := range strings.Split(ansi.Strip(out), "\n") {
		if strings.Contains(l, "image/png") || strings.Contains(l, "look") {
			c.StrContains(l, "▌", "each row must carry the user gutter: %q", l)
		}
	}
}

// An image-only prompt gets its image line and nothing else — in particular no
// blank ▌ row for the text it does not have.
func TestImageOnlyUserBlockHasNoBlankTextRow(t *testing.T) {
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
	r := newRenderer()
	r.width = 80
	out := r.renderBlock(session.Block{
		Kind: session.KindUser, Images: []*rafikiv1.ImageBlock{img},
	})
	lines := strings.Split(strings.TrimPrefix(ansi.Strip(out), "\n"), "\n")
	assert.NewCollecting(t).Eq(1, len(lines), "an image-only block rendered %d lines:\n%q", len(lines), out)
	assert.NewCollecting(t).StrContains(lines[0], "image/png", "the image line")
}

// A tool result's image draws under the SAME gutter as the result's text —
// gutters survive, per the design.
func TestImageInToolResultRendersUnderTheGutter(t *testing.T) {
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		ToolCalls: []session.ToolCall{{
			Name: "bash", HasResult: true, Images: []*rafikiv1.ImageBlock{img},
		}},
	}}
	joined := ansi.Strip(strings.Join(newRenderer().Lines(blocks, 1, 100), "\n"))
	assert.NewCollecting(t).StrContains(joined, "image/png", "the image was not rendered:\n")
	var hit bool
	for _, l := range strings.Split(joined, "\n") {
		if strings.Contains(l, "image/png") {
			hit = hit || strings.Contains(l, "│")
		}
	}
	assert.NewCollecting(t).True(hit, "no image row carries the result gutter:\n%s", joined)
}

// An assistant block's images are named too, after its prose.
func TestImageInAssistantBlockIsNamed(t *testing.T) {
	img := &rafikiv1.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}}
	blocks := []session.Block{{
		Kind: session.KindAssistant, Final: true,
		Images: []*rafikiv1.ImageBlock{img},
	}}
	joined := ansi.Strip(strings.Join(newRenderer().Lines(blocks, 1, 100), "\n"))
	assert.NewCollecting(t).StrContains(joined, "image/png", "the image was not rendered:\n")
}

// MediaType is data, so it is sanitized like any transcript text: an escape
// sequence or a bare CR arriving inside a media type must not reach the
// terminal.
func TestImagePlaceholderLineIsSanitized(t *testing.T) {
	c := assert.NewCollecting(t)
	img := &rafikiv1.ImageBlock{MediaType: "image/png\x1b[2J\r", Data: []byte{1}}

	r := newRenderer()
	r.width = 80
	userOut := ansi.Strip(r.renderBlock(session.Block{
		Kind: session.KindUser, Images: []*rafikiv1.ImageBlock{img},
	}))
	c.StrContains(userOut, "image/png", "user block")
	c.NotStrContains(userOut, "\x1b[2J", "user block must not leak the escape sequence:\n")
	c.NotStrContains(userOut, "\r", "user block must not leak the carriage return:\n")

	toolOut := render(session.ToolCall{
		Name: "bash", HasResult: true, Images: []*rafikiv1.ImageBlock{img},
	})
	c.StrContains(toolOut, "image/png", "tool call")
	c.NotStrContains(toolOut, "\x1b[2J", "tool call must not leak the escape sequence:\n")
	c.NotStrContains(toolOut, "\r", "tool call must not leak the carriage return:\n")
}
