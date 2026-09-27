package table

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func renderTable(t *testing.T, opts Options, header []string, rows [][]string) string {
	t.Helper()
	var sb strings.Builder
	b := New(&sb, opts)
	b.Header(header...)
	for _, r := range rows {
		b.Row(r...)
	}
	assert.NewAborting(t).NoError(b.Render(), "Render")
	return sb.String()
}

func TestSingleLineBorders(t *testing.T) {
	c := assert.NewCollecting(t)
	out := renderTable(t, Options{}, []string{"A", "B"}, [][]string{{"1", "2"}})
	c.StrContains(out, "─", "expected single-line horizontal border, got:\n")
	c.NotStrContains(out, "╭", "expected no rounded border, got:\n")
	// Cells are padded one space each side, the width naturalWidth budgets
	// for and what go-pretty's tables looked like.
	c.StrContains(out, " A ", "expected padded cell, got:\n")
	c.True(strings.HasSuffix(out, "\n"), "expected render to end with a newline, got:\n%q", out)
}

func TestNoANSIWhenColorDisabled(t *testing.T) {
	out := renderTable(t, Options{}, []string{"A", "B"}, [][]string{{"1", "2"}})
	assert.NewCollecting(t).False(strings.ContainsRune(out, '\x1b'), "expected no ANSI escapes with Color off, got:\n%q", out)
}

func TestWidthDropsInDeclaredOrder(t *testing.T) {
	c := assert.NewCollecting(t)
	header := []string{"c0", "c1", "c2", "c3", "c4"}
	rows := [][]string{{"aaaaaa", "bbbbbb", "cccccc", "dddddd", "eeeeee"}}

	// Drop order 2 then 1: first narrow width keeps 4 columns (drops c2),
	// narrower still drops c1 too.
	four := renderTable(t, Options{Width: 40, Drop: []int{2, 1}}, header, rows)
	c.NotStrContains(four, "c2", "expected column 2 dropped, got:\n")
	c.StrContains(four, "c1", "expected column 1 present, got:\n")

	two := renderTable(t, Options{Width: 30, Drop: []int{2, 1}}, header, rows)
	c.False(strings.Contains(two, "c2") || strings.Contains(two, "c1"), "expected columns 1 and 2 dropped, got:\n%s", two)
	for _, keep := range []string{"c0", "c3", "c4"} {
		c.StrContains(two, keep, "expected")
	}
}

func TestNeverDropsLastColumn(t *testing.T) {
	c := assert.NewCollecting(t)
	header := []string{"only", "wide-extra-column-name"}
	rows := [][]string{{"v", "another quite long cell value"}}
	// Drop order names only column 1, so column 0 can never go. It does not
	// fit the 5-column cap either, so its over-wide header truncates — the
	// guarantee is that column 1 was dropped and the frame stayed valid,
	// not that every survivor fits inside Width.
	out := renderTable(t, Options{Width: 5, Drop: []int{1}}, header, rows)
	c.NotStrContains(out, "wide-extra-column-name", "expected column 1 dropped, got:\n")
	c.StrContains(out, "│ v │", "expected column 0's cell to survive, got:\n")
	c.StrContains(out, "┐", "expected a complete frame with the right border intact, got:\n")
}

func TestNoWidthCapKeepsAllColumns(t *testing.T) {
	header := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	rows := [][]string{{"1", "2", "3", "4", "5", "6", "7", "8"}}
	out := renderTable(t, Options{}, header, rows)
	for _, h := range header {
		assert.NewCollecting(t).StrContains(out, h, "expected column")
	}
}

func TestDimHeaderWhenColor(t *testing.T) {
	c := assert.NewCollecting(t)
	out := renderTable(t, Options{Color: true}, []string{"H", "B"}, [][]string{{"x", "y"}})
	// Padding renders outside the dim span: "│ \x1b[2mH\x1b[m │".
	c.StrContains(out, "\x1b[2mH\x1b[m", "expected dimmed header, got:\n")
	c.StrContains(out, "\x1b[2mB\x1b[m", "expected dimmed header B, got:\n")
	// Body cells must not carry the dim code.
	c.NotStrContains(out, "\x1b[2mx", "expected body cells undimmed, got:\n")
}

func TestANSIAwareWidths(t *testing.T) {
	colored := "\x1b[31mstatus\x1b[0m"
	header := []string{"S", "N"}
	rows := [][]string{{colored, "name"}}
	// The colored cell measures 6 visible columns, so a width that fits both
	// columns intact must not drop either.
	out := renderTable(t, Options{Width: 30, Drop: []int{0}}, header, rows)
	assert.NewCollecting(t).StrContains(out, colored, "expected colored cell kept, got:\n")
}
