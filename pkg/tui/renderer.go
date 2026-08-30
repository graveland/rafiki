// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"go.graveland.dev/rafiki/pkg/tui/session"
)

// renderer caches finalized blocks and re-renders the live tail on demand.
// It follows the two-axis design rule (2026-08-12 design §4.2):
//  1. Immutable finalized blocks → cached styled strings
//  2. One live tail block → re-rendered each coalescence tick
type renderer struct {
	md      *glamour.TermRenderer
	mu      sync.Mutex
	lastFP  string // fingerprint of the last rendered live tail
	liveOut string // current live tail rendering
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
		if tc.Running {
			sb.WriteString(styleRunning.Render("  ⚒ " + tc.Name + " running…"))
			sb.WriteString("\n")
		} else {
			dur := ""
			if tc.DurationMs > 0 {
				dur = styleMeta.Render(" (" + durStr(tc.DurationMs) + ")")
			}
			prefix := styleTool.Render("  ⚒ "+tc.Name) + dur
			if tc.IsError {
				prefix += styleError.Render(" ✗")
			} else {
				prefix += styleMeta.Render(" ✓")
			}
			sb.WriteString(prefix)
			sb.WriteString("\n")
			if tc.Result != "" {
				rendered, _ := r.md.Render(tc.Result)
				rendered = strings.TrimSpace(rendered)
				for _, line := range strings.Split(rendered, "\n") {
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

// renderBlocks returns the full rendered transcript. finalized is the index
// into blocks after which everything is finalized (cached); blocks after that
// (the live tail) are re-rendered.
func (r *renderer) renderBlocks(blocks []session.Block, finalized int) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(blocks) == 0 {
		return styleMeta.Render("Connecting…")
	}

	needRender := false
	if finalized < len(blocks) {
		live := &blocks[len(blocks)-1]
		if live.Kind == session.KindAssistant && !live.Final {
			fp := live.Fingerprint()
			if fp != r.lastFP {
				r.lastFP = fp
				needRender = true
			}
		}
	}

	var sb strings.Builder
	for i := 0; i < finalized; i++ {
		sb.WriteString(r.renderBlock(blocks[i]))
		sb.WriteString("\n")
	}

	if needRender || r.liveOut == "" {
		var liveSb strings.Builder
		for i := finalized; i < len(blocks); i++ {
			liveSb.WriteString(r.renderBlock(blocks[i]))
			liveSb.WriteString("\n")
		}
		if !needRender {
			// Only the live tail changed; only re-render it
			r.liveOut = liveSb.String()
		}
	} else {
		// All blocks finalized; no tail to re-render
		r.liveOut = ""
	}

	sb.WriteString(r.liveOut)
	return sb.String()
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "…"
}
