// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

// toolArgKeys names the argument each tool is ABOUT, so a call renders as
// `bash $ go test ./...` rather than `bash`. Modelled on pi, whose per-tool
// renderCall does the same job with a hand-written function per tool
// (`formatBashCall` renders `$ <command>`); a key per tool gets most of that
// for a fraction of the surface, and anything unlisted falls back to compact
// JSON rather than to nothing.
//
// Seeing the argument is not cosmetic: it is the difference between watching an
// agent work and watching it say "bash" while you decide whether to abort.
// Every key here must name a REGISTERED tool: TestToolArgKeysNameRealTools
// checks each against tools.TierOf, because a map of guessed names silently
// degrades to the JSON fallback and looks like it is working.
var toolArgKeys = map[string][]string{
	"bash":        {"command"},
	"bash_start":  {"command"},
	"bash_output": {"id", "job_id"},
	"bash_kill":   {"id", "job_id"},
	"read":        {"path", "file_path"},
	"write":       {"path", "file_path"},
	"edit":        {"path", "file_path"},
	"ls":          {"path"},
	"glob":        {"pattern", "glob"},
	"grep":        {"pattern"},
	"websearch":   {"query"},
	"webfetch":    {"url"},
	"skill":       {"name"},
	"agent_spawn": {"name", "prompt"},
	"agent_send":  {"child_id", "message"},
	"agent_view":  {"child_id"},
	"agent_kill":  {"child_id"},
}

// maxToolArgWidth caps the inline argument summary. A multi-line heredoc or a
// whole file's contents is a legitimate argument, and one of those unrolled
// into the transcript buries the conversation it is part of.
const maxToolArgWidth = 100

// toolArgSummary renders a tool's arguments as ONE line.
//
// Preference order: the tool's own key, then any string field (a tool nobody
// listed is still usually about one string), then the compact JSON. An empty
// object renders as nothing rather than as "{}", so a no-argument tool does not
// gain a meaningless suffix.
func toolArgSummary(name, input string) string {
	input = strings.TrimSpace(input)
	if input == "" || input == "{}" || input == "null" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil || len(args) == 0 {
		// Not an object: show it as-is. A model can emit anything here and the
		// point is to show what it emitted.
		return truncate(collapse(input), maxToolArgWidth)
	}
	for _, k := range toolArgKeys[name] {
		if v, ok := args[k].(string); ok && v != "" {
			return truncate(collapse(v), maxToolArgWidth)
		}
	}
	// Deterministic: ranging a map would reorder the summary between frames.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return truncate(k+"="+collapse(v), maxToolArgWidth)
		}
	}
	// No string anywhere. The batch tools (task_add's items, task_update's
	// changes) are arrays of objects, and their raw JSON is both long and
	// unreadable; a count is the honest one-line summary of a batch.
	for _, k := range keys {
		if v, ok := args[k].([]any); ok {
			return k + "×" + itoa(int64(len(v)))
		}
	}
	return truncate(collapse(input), maxToolArgWidth)
}

// collapse folds whitespace so a multi-line argument stays on its one line.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// maxToolResultLines caps how much of one tool result reaches the pane.
//
// The elided remainder is NOT reachable from the TUI — it lives in the event
// log and nothing surfaces it. That is a deliberate, known limitation (design
// §8): 500 lines of grep inline is unusable, and rendering it through glamour
// four times a second is worse. Raising this is a one-line change.
const maxToolResultLines = 20

// renderer caches finalized blocks and re-renders the live tail on demand.
// It follows the two-axis design rule (2026-08-12 design §4.2):
//  1. Immutable finalized blocks → cached styled strings
//  2. One live tail block → re-rendered each coalescence tick
type renderer struct {
	md *glamour.TermRenderer
	mu sync.Mutex

	// cached holds rendered lines for blocks[:cachedUpTo], all finalized and
	// therefore immutable. The live tail is re-rendered per call and is never
	// stored here.
	cached     []string
	cachedUpTo int

	lastFP  string
	liveOut []string

	// width the cache was built at. The viewport's SoftWrap is OFF (it re-wraps
	// every line on every Update AND every View: measured at 10.9ms and 10.6ms
	// on a 6933-line transcript, against 2.4µs and 167µs unwrapped), so the
	// renderer owns wrapping — which also puts the gutter on continuation rows,
	// where soft wrap left them bare.
	width int
}

func newRenderer() *renderer {
	r, _ := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(0), // let terminal wrap
	)
	return &renderer{md: r}
}

var (
	styleUser       = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)   // cyan
	styleMeta       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))              // grey
	styleTool       = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))              // yellow
	styleThink      = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true) // grey italic
	styleError      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))              // red
	stylePending    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Italic(true) // yellow italic
	styleRunning    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))              // cyan
	styleToolResult = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))              // grey
	styleToolArg    = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))              // arg summary
	styleDivider    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("───")

	// Focus is shown two ways because one was not enough: a reversed badge in
	// the footer, and an accent edge on the pane itself.
	styleFocusBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).
			Background(lipgloss.Color("6")).Bold(true)
	styleFocusEdge = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))

	// The transcript's three weights, by GUTTER rather than by background.
	// pi backgrounds its tool calls, which on a working agent is most of the
	// screen; the scarce thing is the agent's own prose, so that gets the solid
	// bar, thinking gets a dotted one, and tool calls get none at all.
	styleAssistantBar = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleThinkBar     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// renderBlock converts one block into its styled string.
func (r *renderer) renderBlock(b session.Block) string {
	switch b.Kind {
	case session.KindPendingUser:
		return stylePending.Render("⏳ ") + stylePending.Render(b.Text)
	case session.KindUser:
		// A blank line ABOVE, so a prompt is separated from whatever the agent
		// was doing before it. The prompt's own styling is untouched.
		return "\n" + strings.Join(
			wrapTo(styleUser.Render("▸ "), styleUser.Render(b.Text), r.width), "\n")
	case session.KindSystem:
		return styleMeta.Render("⚙  ") + styleMeta.Render(b.Text)
	case session.KindAssistant:
		return r.renderAssistant(b)
	}
	return ""
}

// renderAssistant renders an assistant turn with thinking, tool calls, and results.
func (r *renderer) renderAssistant(b session.Block) string {
	var sb strings.Builder

	if b.ThinkText != "" {
		r.writeWrapped(&sb, styleThinkBar.Render("┊ "),
			styleThink.Render("thinking… "+truncate(b.ThinkText, 120)))
	}

	for _, tc := range b.ToolCalls {
		arg := toolArgSummary(tc.Name, tc.Input)
		if arg != "" {
			arg = " " + arg
		}
		if tc.Running {
			sb.WriteString(styleRunning.Render("  ⚒ "+tc.Name) + styleToolArg.Render(arg) +
				styleRunning.Render(" …"))
			sb.WriteString("\n")
		} else {
			dur := ""
			if tc.DurationMs > 0 {
				dur = styleMeta.Render(" (" + durStr(tc.DurationMs) + ")")
			}
			prefix := styleTool.Render("  ⚒ "+tc.Name) + styleToolArg.Render(arg) + dur
			if tc.IsError {
				prefix += styleError.Render(" ✗")
			} else {
				prefix += styleMeta.Render(" ✓")
			}
			sb.WriteString(prefix)
			sb.WriteString("\n")
			if tc.Result != "" {
				// Tool output is NOT markdown and must never be reflowed as
				// prose. glamour joins consecutive newline-separated lines
				// into one CommonMark paragraph, so "alpha\nbeta\ngamma"
				// rendered as "alpha beta gamma" and a 500-line grep collapsed
				// to a single ~6000-column line. Measured: plain
				// newline-separated input renders to exactly 1 line, while
				// fenced, indented and list input render to 500+.
				//
				// That also made maxToolResultLines inert for exactly the
				// output it was written for — one line is never over a 20-line
				// cap — so the cap only ever fired on tool results that
				// happened to look like markdown. Splitting the RAW result
				// fixes both the structure and the cap.
				// The TAIL, not the head, and the marker goes above it --
				// pi's truncateToVisualLines does the same (`slice(-max)`).
				// A command's ending is where its error is and where a long
				// build's progress has got to; the first 20 lines of a failing
				// test run are the banner.
				lines := strings.Split(strings.TrimRight(tc.Result, "\n"), "\n")
				elided := 0
				if len(lines) > maxToolResultLines {
					elided = len(lines) - maxToolResultLines
					lines = lines[len(lines)-maxToolResultLines:]
				}
				if elided > 0 {
					sb.WriteString(styleMeta.Render(
						"    │ … " + itoa(int64(elided)) + " earlier lines"))
					sb.WriteString("\n")
				}
				for _, line := range lines {
					r.writeWrapped(&sb, styleToolResult.Render("    │ "),
						styleToolResult.Render(line))
				}
			}
		}
	}

	if b.Text != "" {
		// The agent's own prose is the scarce thing on this screen. A bar on
		// every text line alone was too quiet to find while scrolling, so the
		// block is given room and the gutter is given HEIGHT: a blank line
		// separates it from what came before, and the bar runs one row above
		// and one below the text. That turns a colour difference — which the
		// eye has to look for — into a shape, which it does not.
		//
		// Vertical space is cheap now that PgUp/PgDn reach the transcript from
		// the input box; before that, every row spent on framing was a row of
		// conversation someone had to tab away to recover.
		bar := styleAssistantBar.Render("▌ ")
		edge := styleAssistantBar.Render("▌")
		sb.WriteString("\n" + edge + "\n")
		rendered, err := r.md.Render(b.Text)
		if err == nil {
			rendered = strings.TrimSpace(rendered)
			for _, line := range strings.Split(rendered, "\n") {
				r.writeWrapped(&sb, bar, line)
			}
		} else {
			r.writeWrapped(&sb, bar, b.Text)
		}
		sb.WriteString(edge + "\n")
	}

	// Only a stop reason worth reading. end_turn is the normal ending and
	// tool_use only repeats what the ⚒ line above already showed — printing
	// them put a "── tool_use" rule under EVERY block of a tool-calling turn,
	// which is most blocks, competing with the content for attention while
	// carrying none.
	if b.Final && interestingStop(b.StopReason) {
		sb.WriteString(styleMeta.Render("  ── " + b.StopReason))
		sb.WriteString("\n")
	}

	return sb.String()
}

// Lines renders the transcript as display lines: cached output for the
// finalized prefix, freshly rendered output for the live tail.
//
// Before this, every finalized block was re-rendered through glamour on every
// tick (250ms), and then all but the visible tail was discarded. Cost grew
// linearly with conversation length for output nobody saw. Returning LINES
// rather than one string is what lets viewport.SetContentLines take it
// directly, and makes a future prepend's YOffset shift exactly len(prepended).
func (r *renderer) Lines(blocks []session.Block, finalized, width int) []string {
	if width != r.width {
		// Every cached line was wrapped to the old width.
		r.width = width
		r.cached, r.cachedUpTo, r.lastFP, r.liveOut = nil, 0, "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if finalized < 0 {
		finalized = 0
	}
	if finalized > len(blocks) {
		finalized = len(blocks)
	}

	// Finalized moving backwards means this is a different transcript, not a
	// shorter one. Appending to the old cache would splice two together.
	if finalized < r.cachedUpTo {
		r.cached = nil
		r.cachedUpTo = 0
		r.lastFP = ""
		r.liveOut = nil
	}
	for i := r.cachedUpTo; i < finalized; i++ {
		r.cached = append(r.cached, blockLines(r.renderBlock(blocks[i]))...)
	}
	r.cachedUpTo = finalized

	// Live tail: recompute when its fingerprint changed, when nothing is
	// cached, and when the tail is empty (so the cache empties rather than
	// stranding the last streaming fragment below finalized content).
	fp := ""
	if finalized < len(blocks) {
		if live := &blocks[len(blocks)-1]; live.Kind == session.KindAssistant && !live.Final {
			fp = live.Fingerprint()
		}
	}
	if fp != r.lastFP || r.liveOut == nil || finalized >= len(blocks) {
		r.lastFP = fp
		var tail []string
		for i := finalized; i < len(blocks); i++ {
			tail = append(tail, blockLines(r.renderBlock(blocks[i]))...)
		}
		r.liveOut = tail
	}

	// No blocks, no lines. An empty transcript is not a connection state and
	// this function cannot tell the difference anyway -- the shell knows
	// whether a stream is open and renders the empty case itself.
	if len(r.cached) == 0 && len(r.liveOut) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.cached)+len(r.liveOut))
	out = append(out, r.cached...)
	out = append(out, r.liveOut...)
	return out
}

// interestingStop reports whether a stop reason tells the reader something.
// The unusual endings — a truncated answer, a refusal, an error — all do.
func interestingStop(reason string) bool {
	switch reason {
	case "", "end_turn", "tool_use", "stop":
		return false
	}
	return true
}

// wrapTo folds one already-styled line to width, repeating prefix on every
// continuation row so a gutter survives wrapping. prefix is measured on its
// PLAIN width — it carries ANSI colour and counting the escapes would eat the
// text budget.
func wrapTo(prefix, text string, width int) []string {
	indent := ansi.StringWidth(ansi.Strip(prefix))
	avail := width - indent
	if avail < 8 {
		avail = 8
	}
	wrapped := ansi.Hardwrap(ansi.Wordwrap(text, avail, " -"), avail, true)
	rows := strings.Split(strings.TrimRight(wrapped, "\n"), "\n")
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, prefix+row)
	}
	return out
}

// writeWrapped is wrapTo straight into the builder.
func (r *renderer) writeWrapped(sb *strings.Builder, prefix, text string) {
	for _, row := range wrapTo(prefix, text, r.width) {
		sb.WriteString(row)
		sb.WriteString("\n")
	}
}

// blockLines splits one rendered block into display lines, dropping the
// trailing empty element a block's final newline produces.
func blockLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func durStr(ms int64) string {
	if ms < 1000 {
		return itoa(ms) + "ms"
	}
	return itoa(ms/1000) + "." + itoa((ms%1000)/100) + "s"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte(n%10) + '0'
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// truncate shortens s to at most n runes, appending an ellipsis when it cuts.
//
// Runes, not bytes: this is called on model-produced thinking text, and the
// old s[:n-3] split multibyte characters and rendered mojibake on a line that
// is always on screen. Not ansi.Truncate — this text carries no escapes, and a
// rune budget is what the caller means.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
