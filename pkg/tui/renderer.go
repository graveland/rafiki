// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

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
)

// renderBlock converts one block into its styled string.
func (r *renderer) renderBlock(b session.Block) string {
	switch b.Kind {
	case session.KindPendingUser:
		return stylePending.Render("⏳ ") + stylePending.Render(b.Text)
	case session.KindUser:
		return styleUser.Render("▸ ") + styleUser.Render(b.Text)
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
		sb.WriteString(styleThink.Render("  thinking… "))
		sb.WriteString(styleThink.Render(truncate(b.ThinkText, 120)))
		sb.WriteString("\n")
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
					sb.WriteString(styleToolResult.Render("    │ " + line))
					sb.WriteString("\n")
				}
			}
		}
	}

	if b.Text != "" {
		rendered, err := r.md.Render(b.Text)
		if err == nil {
			rendered = strings.TrimSpace(rendered)
			for _, line := range strings.Split(rendered, "\n") {
				sb.WriteString("  " + line + "\n")
			}
		} else {
			sb.WriteString("  " + b.Text + "\n")
		}
	}

	if b.Final && b.StopReason != "" {
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
func (r *renderer) Lines(blocks []session.Block, finalized int) []string {
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
