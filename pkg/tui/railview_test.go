// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/tui/rail"
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

func TestFmtCost(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, ""},
		{0.004, "$0.0040"},
		{0.42, "$0.42"},
		{12.5, "$12.50"},
		{1234.5, "$1234.50"},
	} {
		if got := fmtCost(tc.in, nil); got != tc.want {
			t.Errorf("fmtCost(%v, nil) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Zero stays blank even with a currency configured -- the rail's "no noise
// beside idle agents" rule is independent of the conversion.
func TestFmtCostConverts(t *testing.T) {
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	if got := fmtCost(0, cur); got != "" {
		t.Errorf("fmtCost(0, cur) = %q, want blank", got)
	}
	if got, want := fmtCost(1.0, cur), "$1.38 CAD"; got != want {
		t.Errorf("fmtCost(1.0, cur) = %q, want %q", got, want)
	}
}

// The cost must be part of the PLAIN row so it counts against the width
// budget. Rows are clipped before styling; a cost appended afterwards would
// push the row past the pane and bleed into the transcript.
func TestRailRowCostCountsAgainstWidth(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c1", Name: "root", Cost: 12.34},
		{ChildID: "c2", Name: "worker", ParentID: "c1", Depth: 1, Cost: 1.0},
	}
	out := renderRail(nodes, "c1", "c1", 30, false, nil)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := ansi.StringWidth(line); w > 30 {
			t.Errorf("row is %d columns wide, budget is 30: %q", w, line)
		}
	}
	if !strings.Contains(out, "$12.34") {
		t.Errorf("cost missing from the rail:\n%s", out)
	}
}

// ── rail width ───────────────────────────────────────────────────────────────

// The rail used to be a fixed 22 columns and amputated real names.
func TestRailGrowsToFitTheLongestName(t *testing.T) {
	short := []rail.Node{
		{ChildID: "c_1", Name: "alpha"},
		{ChildID: "c_2", Name: "beta"},
	}
	long := []rail.Node{
		{ChildID: "c_1", Name: "alpha"},
		{ChildID: "c_2", Name: "executor-integration-reviewer"},
	}
	narrow := railWidthFor(short, 200, nil)
	wide := railWidthFor(long, 200, nil)
	if wide <= narrow {
		t.Errorf("width: short=%d long=%d, want the longer name to widen the rail", narrow, wide)
	}
	if got := renderRail(long, "c_1", "c_1", wide, false, nil); !strings.Contains(got, "executor-integration-reviewer") {
		t.Error("the longest name is still truncated at the width chosen for it")
	}
}

// It sizes to CONTENT, not to the window: a rail that tracks the window
// reflows the conversation on every frame of a drag, which is what the old
// fixed width was chosen to avoid.
func TestRailWidthIgnoresTheWindowUntilTheClamp(t *testing.T) {
	nodes := []rail.Node{{ChildID: "c_1", Name: "alpha"}, {ChildID: "c_2", Name: "beta"}}
	if railWidthFor(nodes, 100, nil) != railWidthFor(nodes, 400, nil) {
		t.Error("rail width tracked the window; that reflows the transcript on every drag")
	}
}

func TestRailNeverFallsBelowTheOldFixedWidth(t *testing.T) {
	nodes := []rail.Node{{ChildID: "c_1", Name: "a"}, {ChildID: "c_2", Name: "b"}}
	if got := railWidthFor(nodes, 200, nil); got != railMin {
		t.Errorf("width = %d, want the floor %d", got, railMin)
	}
}

// One absurdly-named agent must not eat the transcript.
func TestRailIsClampedToAFractionOfTheWindow(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "a"},
		{ChildID: "c_2", Name: strings.Repeat("x", 300)},
	}
	got := railWidthFor(nodes, 100, nil)
	if got > 100*railMaxPct/100 {
		t.Errorf("width = %d, want it clamped to %d%% of a 100-col window", got, railMaxPct)
	}
}

// Indentation and the cost readout are part of the row and so part of the
// budget -- they are what gets clipped when the width is too small.
func TestRailWidthCountsDepthAndCost(t *testing.T) {
	flat := []rail.Node{{ChildID: "c_1", Name: "alpha"}, {ChildID: "c_2", Name: "reviewer-agent-one"}}
	deep := []rail.Node{{ChildID: "c_1", Name: "alpha"},
		{ChildID: "c_2", Name: "reviewer-agent-one", Depth: 3}}
	if railWidthFor(deep, 400, nil) <= railWidthFor(flat, 400, nil) {
		t.Error("indentation did not count against the width budget")
	}
	costly := []rail.Node{{ChildID: "c_1", Name: "alpha"},
		{ChildID: "c_2", Name: "reviewer-agent-one", Cost: 12.34}}
	if railWidthFor(costly, 400, nil) <= railWidthFor(flat, 400, nil) {
		t.Error("the cost readout did not count against the width budget")
	}
}
