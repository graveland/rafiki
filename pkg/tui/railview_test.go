// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestClipMeasuresDisplayWidthNotRunes pins the bug that made the conversation
// pane lose ~20% of its width and bleed colour. clip() counted RUNES, and a
// lipgloss escape sequence is runes: a 40-visible-char styled string is 51
// runes, so clip(s, 45) cut inside the text and amputated the trailing reset.
//
// railHidden is false by default, so every conversation line goes through clip
// and every rail row through padTo on every frame.
func TestClipMeasuresDisplayWidthNotRunes(t *testing.T) {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styled := style.Render(strings.Repeat("x", 40))

	if got := ansi.StringWidth(styled); got != 40 {
		t.Fatalf("fixture is wrong: styled width = %d, want 40", got)
	}

	// Wider than the content: must be returned untouched.
	if got := clip(styled, 45); got != styled {
		t.Errorf("clip at width 45 altered a 40-wide string")
	}

	// Narrower: must keep exactly `width` display columns, ellipsis included.
	got := clip(styled, 12)
	if w := ansi.StringWidth(got); w != 12 {
		t.Errorf("clip(width=12) produced %d display columns, want 12 (raw %q)", w, got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("clip did not add an ellipsis: %q", got)
	}
}

// TestPadToMeasuresDisplayWidth: the rail's columns must line up, which they
// cannot if padding counts escape bytes as visible.
func TestPadToMeasuresDisplayWidth(t *testing.T) {
	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("ok")
	got := padTo(styled, 10)
	if w := ansi.StringWidth(got); w != 10 {
		t.Errorf("padTo(10) produced %d display columns, want 10 (raw %q)", w, got)
	}
}

// TestClipHandlesWideRunes: a CJK glyph is two columns, not one.
func TestClipHandlesWideRunes(t *testing.T) {
	got := clip("日本語テキスト", 6)
	if w := ansi.StringWidth(got); w > 6 {
		t.Errorf("clip produced %d columns for a 6-column budget: %q", w, got)
	}
}

// TestTruncateIsRuneSafe pins the byte-slice bug: truncate did s[:n-3] on a
// string, so any multibyte character straddling the cut produced mojibake on
// the thinking line, which is always on screen.
func TestTruncateIsRuneSafe(t *testing.T) {
	in := strings.Repeat("é", 100) // 2 bytes each, 100 runes, 200 bytes
	got := truncate(in, 50)

	if !strings.ContainsRune(got, '…') {
		t.Errorf("expected an ellipsis, got %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("truncate produced a replacement character (split a rune): %q", got)
	}
	if n := len([]rune(got)); n > 50 {
		t.Errorf("truncate returned %d runes, want <= 50", n)
	}
	// Short input must be returned untouched.
	if got := truncate("héllo", 50); got != "héllo" {
		t.Errorf("truncate altered a short string: %q", got)
	}
}
