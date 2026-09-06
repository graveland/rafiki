// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// executorsLoadedMsg carries the daemon's ListExecutors answer back to the
// picker, mirroring modelsLoadedMsg exactly.
type executorsLoadedMsg struct {
	kind string
	rows []*rafikiv1.ExecutorRow
	err  error
}

// fetchExecutorsCmd asks the daemon what executors it has for this kind,
// unless that answer is already cached or already in flight. See
// fetchModelsCmd — same shape, same reasoning. It returns nil when there is
// nothing to do, so callers can issue it on every event that might need the
// rows (form open, kind change, ctrl+e) without tracking state themselves.
func (c *Cockpit) fetchExecutorsCmd(kind string) tea.Cmd {
	if c.executors == nil {
		c.executors = map[string][]*rafikiv1.ExecutorRow{}
		c.executorsErr = map[string]string{}
		c.executorsBusy = map[string]bool{}
	}
	if _, ok := c.executors[kind]; ok {
		return nil
	}
	if c.executorsBusy[kind] {
		return nil
	}
	c.executorsBusy[kind] = true
	delete(c.executorsErr, kind)

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		resp, err := c.client.ListExecutors(ctx,
			connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: kind}))
		if err != nil {
			return executorsLoadedMsg{kind: kind, err: err}
		}
		return executorsLoadedMsg{kind: kind, rows: resp.Msg.GetRows()}
	}
}

// executorsFor returns the cached rows for a kind, plus whether the answer
// has arrived. See modelsFor: absent rows and an empty list are different
// states, and the pre-submit ambiguity check only consults rows that have
// actually arrived — an absent answer is NOT "no executors".
func (c *Cockpit) executorsFor(kind string) ([]*rafikiv1.ExecutorRow, bool) {
	rows, ok := c.executors[kind]
	return rows, ok
}

// applyExecutorsLoaded caches the daemon's answer and refreshes the picker if
// one is open for this kind. See applyModelsLoaded.
func (c *Cockpit) applyExecutorsLoaded(m executorsLoadedMsg) {
	if c.executors == nil {
		c.executors = map[string][]*rafikiv1.ExecutorRow{}
		c.executorsErr = map[string]string{}
		c.executorsBusy = map[string]bool{}
	}
	delete(c.executorsBusy, m.kind)
	if m.err != nil {
		c.executorsErr[m.kind] = trimRPCError(m.err)
	} else {
		c.executors[m.kind] = m.rows
	}
	if c.execPicker != nil && c.execPicker.kind == m.kind {
		c.execPicker.loading = false
		c.execPicker.err = c.executorsErr[m.kind]
		c.execPicker.rows = c.executors[m.kind]
		c.execPicker.clampCursor()
	}
}

// executorRef is the ref a picked row commits into the form: the machine
// label when present, else the raw id. Same rule as the CLI's refFor —
// machine labels are unique per owner, so the label is the ref a person
// typed last time and the id is what an unlabeled fleet executor falls
// back to.
func executorRef(r *rafikiv1.ExecutorRow) string {
	if m := r.GetMachine(); m != "" {
		return m
	}
	return r.GetId()
}

// executorPicker browses what the daemon's ListExecutors RPC reports for a
// kind. Deliberately much smaller than modelPicker: no sort/filter dialog, no
// multi-column bounds — executors are a small, low-cardinality set, and that
// machinery would be premature (see the design doc's non-goals).
type executorPicker struct {
	kind string

	// rows is the whole answer, unfiltered — unlike modelPicker's all/rows
	// split, there is no filter box here (see the design doc's non-goals), so
	// a second staging field would be a distinction with no difference.
	rows []*rafikiv1.ExecutorRow

	cursor int
	offset int

	loading bool
	err     string
}

func newExecutorPicker(kind string, rows []*rafikiv1.ExecutorRow, loaded bool, err string) *executorPicker {
	// A known failure is NOT a loading state, matching newModelPicker: a
	// picker opened after a failed fetch must say so rather than show
	// "asking the daemon…" forever.
	p := &executorPicker{kind: kind, rows: rows, loading: !loaded && err == "", err: err}
	p.clampCursor()
	return p
}

// clampCursor keeps the cursor in bounds after rows changes size — called on
// construction and whenever applyExecutorsLoaded replaces rows with a fresh
// answer.
func (p *executorPicker) clampCursor() {
	if p.cursor >= len(p.rows) {
		p.cursor = max(0, len(p.rows)-1)
	}
}

func (p *executorPicker) move(delta, window int) {
	if len(p.rows) == 0 {
		return
	}
	p.cursor = min(max(p.cursor+delta, 0), len(p.rows)-1)
	// Keep the cursor inside the drawn window.
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if window > 0 && p.cursor >= p.offset+window {
		p.offset = p.cursor - window + 1
	}
}

// selected returns the highlighted row's ref, or "" when the list is empty.
func (p *executorPicker) selected() string {
	if p.cursor < 0 || p.cursor >= len(p.rows) {
		return ""
	}
	return executorRef(p.rows[p.cursor])
}

// handleExecutorPickerKey routes a keystroke while the executor picker is up.
// Mirrors handlePickerKey (the model picker's) at the smaller scale this
// picker has: no filter box, no sort/filter dialog.
func (c *Cockpit) handleExecutorPickerKey(msg tea.KeyPressMsg, window int) (tea.Model, tea.Cmd) {
	p := c.execPicker
	switch msg.String() {
	case "esc", "ctrl+c":
		c.execPicker = nil
		return c, nil
	case "enter":
		if ref := p.selected(); ref != "" {
			c.form.inputs[fieldExecutor].SetValue(ref)
		}
		c.execPicker = nil
		// Advance past the executor row, matching the model picker: picking an
		// executor is almost always the last thing set before submitting.
		c.form.moveFocus(+1)
		return c, nil
	case "up":
		p.move(-1, window)
		return c, nil
	case "down":
		p.move(+1, window)
		return c, nil
	case "pgup":
		p.move(-window, window)
		return c, nil
	case "pgdown":
		p.move(+window, window)
		return c, nil
	case "home":
		p.cursor, p.offset = 0, 0
		return c, nil
	}
	return c, nil
}

// executorPickerChrome is the rows the picker spends on things that are not
// executors: title, blank, a detail/reason line, and two rows of slack. The
// reason line is conditional, so the count is deliberately conservative — a
// window one row short of maximum is cheap, a view that overflows the pane is
// not.
const executorPickerChrome = 5

func (p *executorPicker) view(width, height int) string {
	var b strings.Builder
	b.WriteString(styleRailFocused.Render("executor"))
	b.WriteString(styleMeta.Render("  " + p.kind))
	b.WriteString("\n\n")

	switch {
	// The error is checked FIRST, matching the model picker: a picker that
	// knows why it has no rows must say so rather than claim to still be
	// waiting.
	case p.err != "":
		b.WriteString(styleError.Render("✗ " + p.err))
		return b.String()
	case p.loading:
		b.WriteString(stylePending.Render("⏳ asking the daemon…"))
		return b.String()
	case len(p.rows) == 0:
		b.WriteString(styleMeta.Render("no executors reported"))
		return b.String()
	}

	window := max(1, height-executorPickerChrome)
	end := min(len(p.rows), p.offset+window)
	for i := p.offset; i < end; i++ {
		r := p.rows[i]
		lead := "  "
		ref := executorRef(r)
		if i == p.cursor {
			lead = styleFocusEdge.Render("▌ ")
			ref = styleRailFocused.Render(ref)
		}
		glyph := "✗"
		if r.GetEligible() {
			glyph = "✓"
		}
		b.WriteString(lead)
		b.WriteString(padTo(ref, 24))
		b.WriteString("  ")
		b.WriteString(glyph)
		b.WriteString("\n")
	}
	// One detail line for the highlighted row — an executor has far fewer
	// facts worth surfacing than a model, and the reason a row is NOT
	// eligible is the one that matters most when every visible row reads ✗.
	if p.cursor >= 0 && p.cursor < len(p.rows) {
		sel := p.rows[p.cursor]
		if !sel.GetEligible() && sel.GetReason() != "" {
			b.WriteString(styleMeta.Render(sel.GetReason()))
			b.WriteString("\n")
		}
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(b.String())
}
