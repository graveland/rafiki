// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

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
func renderRail(nodes []rail.Node, focused string, width int) string {
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
		row := clip(strings.Repeat("  ", n.Depth)+rail.Glyph(n)+" "+name+badge, width)
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

// clip truncates to width in RUNES. A child name holds whatever a spawner
// typed, including CJK and emoji, and byte truncation would split a rune and
// corrupt the whole line.
func clip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// padTo pads s with spaces to width, measured in runes.
func padTo(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return clip(s, width)
	}
	return s + strings.Repeat(" ", width-n)
}
