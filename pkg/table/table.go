// Package table is the CLI's one shared table renderer, a thin wrapper over
// charm.land/lipgloss/v2/table. One style everywhere: single-line
// (NormalBorder) borders, dimmed headers when color is on. Callers apply
// their own color inside cell strings; the renderer adds none of its own
// unless Options.Color is set.
package table

import (
	"io"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"
)

// Options configures a rendered table.
//
// Width is the total terminal width to fit; 0 means no cap (pipes): every
// column renders, nothing is dropped or wrapped. Drop lists column indices
// in the order they may be sacrificed when the natural width exceeds Width;
// the last surviving column is never dropped.
type Options struct {
	Color bool
	Width int
	Drop  []int
}

// Builder accumulates header and rows, then renders to a writer.
type Builder struct {
	w       io.Writer
	opts    Options
	headers []string
	rows    [][]string
}

// New returns a Builder that renders to w.
func New(w io.Writer, opts Options) *Builder {
	return &Builder{w: w, opts: opts}
}

// Header sets the column header names. When Options.Color is set the
// renderer dims them.
func (b *Builder) Header(names ...string) {
	b.headers = append([]string(nil), names...)
}

// Row appends one data row.
func (b *Builder) Row(cells ...string) {
	b.rows = append(b.rows, append([]string(nil), cells...))
}

func (b *Builder) style(row, col int) lipgloss.Style {
	if row == table.HeaderRow && b.opts.Color {
		return lipgloss.NewStyle().Faint(true)
	}
	return lipgloss.NewStyle()
}

func (b *Builder) pick(cells []string, live []int) []string {
	out := make([]string, len(live))
	for i, c := range live {
		out[i] = cells[c]
	}
	return out
}

// naturalWidth sums the widest visible cell of each live column, plus the
// border overhead (three padding spaces per column plus the two edges).
func (b *Builder) naturalWidth(live []int) int {
	total := 0
	for _, c := range live {
		w := ansi.StringWidth(b.headers[c])
		for _, r := range b.rows {
			if cw := ansi.StringWidth(r[c]); cw > w {
				w = cw
			}
		}
		total += w
	}
	if len(live) > 0 {
		for range live {
			total += 3 // one border char + one space of padding each side
		}
		total++ // outer edge
	}
	return total
}

// Render measures, drops columns in Drop order while the natural width
// exceeds Width (never dropping the last surviving column), and writes the
// table to the builder's writer. With Width 0 nothing is ever dropped.
func (b *Builder) Render() error {
	live := make([]int, len(b.headers))
	for i := range live {
		live[i] = i
	}

	if b.opts.Width > 0 {
		drop := make([]int, 0, len(b.opts.Drop))
		for _, c := range b.opts.Drop {
			if c >= 0 && c < len(b.headers) {
				drop = append(drop, c)
			}
		}
		for _, c := range drop {
			if len(live) <= 1 || b.naturalWidth(live) <= b.opts.Width {
				break
			}
			for j, l := range live {
				if l == c {
					live = append(live[:j], live[j+1:]...)
					break
				}
			}
		}
	}

	t := table.New()
	t.Border(lipgloss.NormalBorder())
	t.StyleFunc(b.style)
	t.Headers(b.pick(b.headers, live)...)
	for _, r := range b.rows {
		t.Row(b.pick(r, live)...)
	}
	if b.opts.Width > 0 {
		t.Width(b.opts.Width)
	}

	_, err := io.WriteString(b.w, t.Render())
	return err
}
