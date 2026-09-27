// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/tui/rail"

	"github.com/multigres/testkit/assert"
)

// TestClipMeasuresDisplayWidthNotRunes pins the bug that made the conversation
// pane lose ~20% of its width and bleed colour. clip() counted RUNES, and a
// lipgloss escape sequence is runes: a 40-visible-char styled string is 51
// runes, so clip(s, 45) cut inside the text and amputated the trailing reset.
//
// railHidden is false by default, so every conversation line goes through clip
// and every rail row through padTo on every frame.
func TestClipMeasuresDisplayWidthNotRunes(t *testing.T) {
	c := assert.NewCollecting(t)
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styled := style.Render(strings.Repeat("x", 40))

	c.Require().Eq(40, ansi.StringWidth(styled), "fixture is wrong: styled width")

	// Wider than the content: must be returned untouched.
	c.Eq(styled, clip(styled, 45), "clip at width 45 altered a 40-wide string")

	// Narrower: must keep exactly `width` display columns, ellipsis included.
	got := clip(styled, 12)
	w := ansi.StringWidth(got)
	c.Eq(12, w, "clip(width=12) produced %d display columns, want 12 (raw %q)", w, got)
	c.StrContains(got, "…", "clip did not add an ellipsis")
}

// TestPadToMeasuresDisplayWidth: the rail's columns must line up, which they
// cannot if padding counts escape bytes as visible.
func TestPadToMeasuresDisplayWidth(t *testing.T) {
	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("ok")
	got := padTo(styled, 10)
	w := ansi.StringWidth(got)
	assert.NewCollecting(t).Eq(10, w, "padTo(10) produced %d display columns, want 10 (raw %q)", w, got)
}

// TestClipHandlesWideRunes: a CJK glyph is two columns, not one.
func TestClipHandlesWideRunes(t *testing.T) {
	got := clip("日本語テキスト", 6)
	w := ansi.StringWidth(got)
	assert.NewCollecting(t).LessOrEqual(6, w, "clip produced %d columns for a 6-column budget: %q", w, got)
}

// TestTruncateIsRuneSafe pins the byte-slice bug: truncate did s[:n-3] on a
// string, so any multibyte character straddling the cut produced mojibake on
// the thinking line, which is always on screen.
func TestTruncateIsRuneSafe(t *testing.T) {
	c := assert.NewCollecting(t)
	in := strings.Repeat("é", 100) // 2 bytes each, 100 runes, 200 bytes
	got := truncate(in, 50)

	c.True(strings.ContainsRune(got, '…'), "expected an ellipsis, got %q", got)
	c.False(strings.ContainsRune(got, '�'), "truncate produced a replacement character (split a rune): %q", got)
	c.LessOrEqual(50, len([]rune(got)), "truncate returned")
	// Short input must be returned untouched.
	c.Eq("héllo", truncate("héllo", 50), "truncate altered a short string")
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
		got := fmtCost(tc.in, nil)
		assert.NewCollecting(t).Eq(tc.want, got, "fmtCost(%v, nil) = %q, want", tc.in, got)
	}
}

// Zero stays blank even with a currency configured -- the rail's "no noise
// beside idle agents" rule is independent of the conversion.
func TestFmtCostConverts(t *testing.T) {
	c := assert.NewCollecting(t)
	cur := &clientstate.Currency{Code: "CAD", Rate: 1.38}
	c.Eq("", fmtCost(0, cur), "fmtCost(0, cur)")
	got, want := fmtCost(1.0, cur), "$1.38 CAD"
	c.Eq(want, got, "fmtCost(1.0, cur)")
}

// The cost must be part of the PLAIN row so it counts against the width
// budget. Rows are clipped before styling; a cost appended afterwards would
// push the row past the pane and bleed into the transcript.
func TestRailRowCostCountsAgainstWidth(t *testing.T) {
	c := assert.NewCollecting(t)
	nodes := []rail.Node{
		{ChildID: "c1", Name: "root", Cost: 12.34},
		{ChildID: "c2", Name: "worker", ParentID: "c1", Depth: 1, Cost: 1.0},
	}
	out := renderRail(nodes, "c1", "c1", 30, false, nil, 0)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		w := ansi.StringWidth(line)
		c.LessOrEqual(30, w, "row is %d columns wide, budget is 30: %q", w, line)
	}
	c.StrContains(out, "$12.34", "cost missing from the rail:\n")
}

// A node with only CostLive set (no Cost) must still show a live number --
// the rail reads n.TotalCost(), not n.Cost, so spend moves on every LLM
// reply rather than only once a whole exchange settles.
func TestRenderRailShowsLiveCost(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c1", Name: "root", CostLive: 0.42},
		{ChildID: "c2", Name: "worker", ParentID: "c1", Depth: 1},
	}
	out := renderRail(nodes, "", "", 80, false, nil, 0)
	assert.NewCollecting(t).StrContains(out, "$0.42", "render did not show the live cost")
}

// ── rail width ───────────────────────────────────────────────────────────────

// The rail used to be a fixed 22 columns and amputated real names.
func TestRailGrowsToFitTheLongestName(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Greater(narrow, wide, "width: short")
	c.StrContains(renderRail(long, "c_1", "c_1", wide, false, nil, 0), "executor-integration-reviewer", "the longest name is still truncated at the width chosen for it")
}

// It sizes to CONTENT, not to the window: a rail that tracks the window
// reflows the conversation on every frame of a drag, which is what the old
// fixed width was chosen to avoid.
func TestRailWidthIgnoresTheWindowUntilTheClamp(t *testing.T) {
	nodes := []rail.Node{{ChildID: "c_1", Name: "alpha"}, {ChildID: "c_2", Name: "beta"}}
	assert.NewCollecting(t).Eq(railWidthFor(nodes, 400, nil), railWidthFor(nodes, 100, nil), "rail width tracked the window; that reflows the transcript on every drag")
}

func TestRailNeverFallsBelowTheOldFixedWidth(t *testing.T) {
	nodes := []rail.Node{{ChildID: "c_1", Name: "a"}, {ChildID: "c_2", Name: "b"}}
	assert.NewCollecting(t).Eq(railMin, railWidthFor(nodes, 200, nil), "width")
}

// One absurdly-named agent must not eat the transcript.
func TestRailIsClampedToAFractionOfTheWindow(t *testing.T) {
	nodes := []rail.Node{
		{ChildID: "c_1", Name: "a"},
		{ChildID: "c_2", Name: strings.Repeat("x", 300)},
	}
	got := railWidthFor(nodes, 100, nil)
	assert.NewCollecting(t).LessOrEqual(100*railMaxPct/100, got, "width = %d, want it clamped to %d%% of a 100-col window", got, railMaxPct)
}

// Indentation and the cost readout are part of the row and so part of the
// budget -- they are what gets clipped when the width is too small.
func TestRailWidthCountsDepthAndCost(t *testing.T) {
	c := assert.NewCollecting(t)
	flat := []rail.Node{{ChildID: "c_1", Name: "alpha"}, {ChildID: "c_2", Name: "reviewer-agent-one"}}
	deep := []rail.Node{{ChildID: "c_1", Name: "alpha"},
		{ChildID: "c_2", Name: "reviewer-agent-one", Depth: 3}}
	c.Greater(railWidthFor(flat, 400, nil), railWidthFor(deep, 400, nil), "indentation did not count against the width budget")
	costly := []rail.Node{{ChildID: "c_1", Name: "alpha"},
		{ChildID: "c_2", Name: "reviewer-agent-one", Cost: 12.34}}
	c.Greater(railWidthFor(flat, 400, nil), railWidthFor(costly, 400, nil), "the cost readout did not count against the width budget")
}

func TestNativeSubagentsAreMarked(t *testing.T) {
	c := assert.NewCollecting(t)
	native := rail.Node{ChildID: "c_a:t1", Name: "task:t1", Status: "idle", Kind: "claude", Native: true}
	real := rail.Node{ChildID: "c_b", Name: "worker", Status: "idle", Kind: "claude"}
	c.NotEq("", nativeTag(native), "a native subagent must carry a marker")
	c.Eq("", nativeTag(real), "a real rafiki agent must not carry the native marker")
}

func TestRailWidthCountsTheKindTag(t *testing.T) {
	plain := []rail.Node{{ChildID: "c_a", Name: "worker", Status: "idle"}}
	tagged := []rail.Node{{ChildID: "c_a", Name: "worker", Status: "idle", Kind: "claude"}}
	// The floor would mask the difference, so use a name long enough to clear it.
	for i := range plain {
		plain[i].Name = strings.Repeat("n", railMin)
		tagged[i].Name = plain[i].Name
	}
	tw, pw := railWidthFor(tagged, 200, nil), railWidthFor(plain, 200, nil)
	// Exact: ansi.StringWidth (2 columns for " ᶜ"), not len (4 bytes). An
	// inequality alone cannot tell a len regression from a correct width.
	assert.NewAborting(t).Eq(ansi.StringWidth(kindTag("claude")), tw-pw, "tag width")
}
