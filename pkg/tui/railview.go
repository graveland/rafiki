// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/tui/rail"
)

var (
	styleRailFocused = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleRailBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	styleRailDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// renderRail draws the tree.
//
// It returns "" for fewer than two rows on purpose: the rail GROWS OUT OF a
// normal session, so a fresh `create` shows a full-width conversation and the
// rail appears when the first child does. There is no cockpit to configure and
// no empty pane to look at.
//
// Rows are clipped BEFORE styling, so the width budget is measured on plain
// text and lipgloss escape sequences never count against it.
func renderRail(nodes []rail.Node, focused, selected string, width int) string {
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
		cursor := "  "
		if n.ChildID == selected {
			cursor = "▸ "
		}
		row := clip(cursor+strings.Repeat("  ", n.Depth)+rail.Glyph(n)+" "+name+badge, width)
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
