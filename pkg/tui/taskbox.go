// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// taskBoxMaxRows caps the box. It costs transcript height, and an agent that
// decomposed its work into forty items must not take the screen with it.
const taskBoxMaxRows = 6

var (
	styleTaskBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleTaskActive = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleTaskDone   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleTaskBlock  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// taskGlyph maps a status to its marker.
//
// ⊘ is a BLOCKED TASK and nothing else on this screen: an abandoned tool call
// is ⋯. One glyph, one meaning -- both are visible at once.
func taskGlyph(status string) string {
	switch status {
	case "completed":
		return "✓"
	case "in_progress":
		return "▶"
	case "blocked":
		return "⊘"
	case "failed":
		return "✗"
	case "orphaned":
		return "⚑"
	default:
		return "○"
	}
}

// liveTask reports whether a row is worth screen space. Terminal rows are
// history; the box answers "what is this agent doing now".
func liveTask(status string) bool {
	switch status {
	case "completed", "failed", "dropped":
		return false
	}
	return true
}

// renderTaskBox draws the focused agent's ledger, or NOTHING when it has no
// live work. Returning no lines rather than an empty box is what lets the
// caller subtract zero height.
func renderTaskBox(rows []*rafikiv1.TaskRow, width int) []string {
	if width < 12 {
		return nil
	}
	var live []*rafikiv1.TaskRow
	for _, r := range rows {
		if liveTask(r.GetStatus()) {
			live = append(live, r)
		}
	}
	if len(live) == 0 {
		return nil
	}

	inner := width - 4
	top := "╭ tasks " + strings.Repeat("─", maxInt(0, width-9)) + "╮"
	out := []string{styleTaskBorder.Render(clip(top, width))}

	shown := live
	more := 0
	if len(shown) > taskBoxMaxRows {
		more = len(shown) - taskBoxMaxRows
		shown = shown[:taskBoxMaxRows]
	}
	for _, r := range shown {
		body := r.GetHandle() + " " + taskGlyph(r.GetStatus()) + " " + r.GetContent()
		style := styleTaskDone
		switch r.GetStatus() {
		case "in_progress":
			style = styleTaskActive
		case "blocked", "orphaned":
			style = styleTaskBlock
		}
		out = append(out, styleTaskBorder.Render("│ ")+
			style.Render(padTo(clip(body, inner), inner))+
			styleTaskBorder.Render(" │"))
	}
	if more > 0 {
		out = append(out, styleTaskBorder.Render("│ ")+
			styleMeta.Render(padTo("+"+itoa(int64(more))+" more", inner))+
			styleTaskBorder.Render(" │"))
	}
	out = append(out, styleTaskBorder.Render(clip("╰"+strings.Repeat("─", maxInt(0, width-2))+"╯", width)))
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
