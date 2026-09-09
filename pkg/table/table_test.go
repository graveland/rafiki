package table

import (
	"strings"
	"testing"
)

func renderTable(t *testing.T, opts Options, header []string, rows [][]string) string {
	t.Helper()
	var sb strings.Builder
	b := New(&sb, opts)
	b.Header(header...)
	for _, r := range rows {
		b.Row(r...)
	}
	if err := b.Render(); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return sb.String()
}

func TestSingleLineBorders(t *testing.T) {
	out := renderTable(t, Options{}, []string{"A", "B"}, [][]string{{"1", "2"}})
	if !strings.Contains(out, "─") {
		t.Errorf("expected single-line horizontal border, got:\n%s", out)
	}
	if strings.Contains(out, "╭") {
		t.Errorf("expected no rounded border, got:\n%s", out)
	}
	// Cells are padded one space each side, the width naturalWidth budgets
	// for and what go-pretty's tables looked like.
	if !strings.Contains(out, " A ") {
		t.Errorf("expected padded cell, got:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected render to end with a newline, got:\n%q", out)
	}
}

func TestNoANSIWhenColorDisabled(t *testing.T) {
	out := renderTable(t, Options{}, []string{"A", "B"}, [][]string{{"1", "2"}})
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("expected no ANSI escapes with Color off, got:\n%q", out)
	}
}

func TestWidthDropsInDeclaredOrder(t *testing.T) {
	header := []string{"c0", "c1", "c2", "c3", "c4"}
	rows := [][]string{{"aaaaaa", "bbbbbb", "cccccc", "dddddd", "eeeeee"}}

	// Drop order 2 then 1: first narrow width keeps 4 columns (drops c2),
	// narrower still drops c1 too.
	four := renderTable(t, Options{Width: 40, Drop: []int{2, 1}}, header, rows)
	if strings.Contains(four, "c2") {
		t.Errorf("expected column 2 dropped, got:\n%s", four)
	}
	if !strings.Contains(four, "c1") {
		t.Errorf("expected column 1 present, got:\n%s", four)
	}

	two := renderTable(t, Options{Width: 30, Drop: []int{2, 1}}, header, rows)
	if strings.Contains(two, "c2") || strings.Contains(two, "c1") {
		t.Errorf("expected columns 1 and 2 dropped, got:\n%s", two)
	}
	for _, keep := range []string{"c0", "c3", "c4"} {
		if !strings.Contains(two, keep) {
			t.Errorf("expected %s present, got:\n%s", keep, two)
		}
	}
}

func TestNeverDropsLastColumn(t *testing.T) {
	header := []string{"only", "wide-extra-column-name"}
	rows := [][]string{{"v", "another quite long cell value"}}
	// Drop order names only column 1, so column 0 can never go. It does not
	// fit the 5-column cap either, so its over-wide header truncates — the
	// guarantee is that column 1 was dropped and the frame stayed valid,
	// not that every survivor fits inside Width.
	out := renderTable(t, Options{Width: 5, Drop: []int{1}}, header, rows)
	if strings.Contains(out, "wide-extra-column-name") {
		t.Errorf("expected column 1 dropped, got:\n%s", out)
	}
	if !strings.Contains(out, "│ v │") {
		t.Errorf("expected column 0's cell to survive, got:\n%s", out)
	}
	if !strings.Contains(out, "┐") {
		t.Errorf("expected a complete frame with the right border intact, got:\n%s", out)
	}
}

func TestNoWidthCapKeepsAllColumns(t *testing.T) {
	header := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	rows := [][]string{{"1", "2", "3", "4", "5", "6", "7", "8"}}
	out := renderTable(t, Options{}, header, rows)
	for _, h := range header {
		if !strings.Contains(out, h) {
			t.Errorf("expected column %s present, got:\n%s", h, out)
		}
	}
}

func TestDimHeaderWhenColor(t *testing.T) {
	out := renderTable(t, Options{Color: true}, []string{"H", "B"}, [][]string{{"x", "y"}})
	// Padding renders outside the dim span: "│ \x1b[2mH\x1b[m │".
	if !strings.Contains(out, "\x1b[2mH\x1b[m") {
		t.Errorf("expected dimmed header, got:\n%q", out)
	}
	if !strings.Contains(out, "\x1b[2mB\x1b[m") {
		t.Errorf("expected dimmed header B, got:\n%q", out)
	}
	// Body cells must not carry the dim code.
	if strings.Contains(out, "\x1b[2mx") {
		t.Errorf("expected body cells undimmed, got:\n%q", out)
	}
}

func TestANSIAwareWidths(t *testing.T) {
	colored := "\x1b[31mstatus\x1b[0m"
	header := []string{"S", "N"}
	rows := [][]string{{colored, "name"}}
	// The colored cell measures 6 visible columns, so a width that fits both
	// columns intact must not drop either.
	out := renderTable(t, Options{Width: 30, Drop: []int{0}}, header, rows)
	if !strings.Contains(out, colored) {
		t.Errorf("expected colored cell kept, got:\n%q", out)
	}
}
