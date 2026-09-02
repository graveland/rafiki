// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

var (
	styleRailFocused = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleRailBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	styleRailDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// fmtCost renders a spend, converted through cur when set. Zero renders as
// nothing: a row of "$0.00" beside every idle agent is noise, and the number
// is only interesting once there is one. This is the rail's own zero rule
// layered over costfmt.Format, which otherwise renders zero as "-" (the
// right call for a data table, the wrong one for a live status rail).
func fmtCost(usd float64, cur *clientstate.Currency) string {
	if usd == 0 {
		return ""
	}
	return costfmt.Format(usd, cur)
}

// renderRail draws the tree.
//
// It returns "" for fewer than two rows on purpose: the rail GROWS OUT OF a
// normal session, so a fresh `create` shows a full-width conversation and the
// rail appears when the first child does. There is no cockpit to configure and
// no empty pane to look at.
//
// Rows are clipped BEFORE styling, so the width budget is measured on plain
// text and lipgloss escape sequences never count against it.
// railWidthFor sizes the rail to its CONTENT, clamped.
//
// Sizing to content rather than to the window is the point. The rail used to be
// a fixed 22 columns and truncated real names; making it a fraction of the
// window would fix that and reintroduce what the fixed width was chosen to
// avoid — a rail that resizes as you drag makes the conversation reflow on
// every frame of the drag. Content changes only when a child spawns, exits or
// is renamed, so this re-wraps about as often as the rail itself changes.
//
// The clamp matters in both directions: railMin keeps a two-agent cockpit from
// a sliver, and railMaxFrac stops one absurdly-named agent eating the
// transcript. The budget counts everything renderRail puts in the plain row —
// cursor, indent, glyph, name, badge and cost — because those are what get
// clipped.
func railWidthFor(nodes []rail.Node, total int, cur *clientstate.Currency) int {
	want := railMin
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.ChildID
		}
		w := 2 + 2*n.Depth + ansi.StringWidth(rail.Glyph(n)) + 1 + ansi.StringWidth(name)
		if n.Attention > 0 {
			w += 2 + len(strconv.Itoa(n.Attention))
		}
		if cost := fmtCost(n.TotalCost(), cur); cost != "" {
			w += 1 + len(cost)
		}
		if w > want {
			want = w
		}
	}
	if cap := total * railMaxPct / 100; cap > railMin && want > cap {
		want = cap
	}
	return want
}

func renderRail(nodes []rail.Node, focused, selected string, width int, paneFocused bool, cur *clientstate.Currency) string {
	if len(nodes) < 2 {
		return ""
	}
	var sb strings.Builder
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.ChildID
		}
		badge := ""
		if n.Attention > 0 {
			badge = " ●" + strconv.Itoa(n.Attention)
		}
		// The cursor is part of the PLAIN row so it counts against the width
		// budget like everything else. Every row carries the two columns,
		// selected or not, so rows do not shift as the cursor moves.
		// The cursor thickens when the rail HOLDS focus, so the pane that is
		// taking your arrow keys is identifiable without pressing one.
		cursor := "  "
		if n.ChildID == selected {
			cursor = "▸ "
			if paneFocused {
				cursor = "▶ "
			}
		}
		// The cost joins the PLAIN row so it counts against the width budget,
		// like the cursor and the badge. Rows are clipped before styling, so
		// anything appended afterwards escapes the pane and bleeds colour into
		// the transcript.
		left := cursor + strings.Repeat("  ", n.Depth) + rail.Glyph(n) + " " + name + badge
		cost := fmtCost(n.TotalCost(), cur)
		row := clip(left, width)
		if cost != "" {
			if gap := width - ansi.StringWidth(clip(left, width-len(cost)-1)) - len(cost); gap >= 1 {
				row = clip(left, width-len(cost)-1) + strings.Repeat(" ", gap) + cost
			}
		}
		switch {
		case n.ChildID == focused:
			sb.WriteString(styleRailFocused.Render(row))
		case n.Attention > 0:
			sb.WriteString(styleRailBadge.Render(row))
		case n.Exited:
			sb.WriteString(styleRailDim.Render(row))
		default:
			sb.WriteString(row)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// clip truncates s to width DISPLAY COLUMNS, appending an ellipsis when it cuts.
//
// Display columns, not runes: a lipgloss-styled string carries ANSI escape
// sequences that are runes but occupy no columns, and a CJK glyph is one rune
// occupying two. Counting runes made the conversation pane lose about a fifth
// of its width and amputate the trailing reset escape, bleeding colour into
// the rail. ansi.Truncate understands both and preserves the escapes it cuts
// across.
func clip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// padTo pads s with spaces to exactly width display columns, clipping if it is
// already wider. See clip for why display columns and not runes.
func padTo(s string, width int) string {
	w := ansi.StringWidth(s)
	if w >= width {
		return clip(s, width)
	}
	return s + strings.Repeat(" ", width-w)
}
