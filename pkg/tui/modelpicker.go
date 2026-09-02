// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// modelsLoadedMsg carries the daemon's model catalog back to the picker.
type modelsLoadedMsg struct {
	kind string
	rows []*rafikiv1.ModelRow
	err  error
}

// modelSort is the column the picker orders by.
type modelSort int

const (
	sortID modelSort = iota
	sortCost
	sortContext
	modelSortCount
)

func (s modelSort) String() string {
	switch s {
	case sortID:
		return "name"
	case sortCost:
		return "cheapest"
	case sortContext:
		return "biggest context"
	}
	return "?"
}

// modelPicker browses what the daemon says it can run.
//
// The rows come from the Connect ListModels RPC, which is scoped by KIND: a
// claude child resolves only Anthropic ids, and offering it an OpenRouter id
// produces a child that spawns, attaches and never answers. The picker never
// applies that rule itself -- it asks with the form's current kind and renders
// the answer.
type modelPicker struct {
	kind string

	all    []*rafikiv1.ModelRow
	rows   []*rafikiv1.ModelRow // filtered + sorted view of all
	filter textinput.Model

	cursor int
	offset int
	sort   modelSort
	// visionOnly drops models KNOWN to be text-only. Models of unknown
	// capability are kept -- see visionKind.
	visionOnly bool

	loading bool
	err     string
}

func newModelPicker(kind, seed string) *modelPicker {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	in.Placeholder = "filter…"
	in.SetValue(seed)
	in.Focus()
	return &modelPicker{kind: kind, filter: in, loading: true}
}

// fetchModelsCmd asks the daemon what it can run for this kind.
func (c *Cockpit) fetchModelsCmd(kind string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()

		resp, err := c.client.ListModels(ctx,
			connect.NewRequest(&rafikiv1.ListModelsRequest{Kind: kind}))
		if err != nil {
			return modelsLoadedMsg{kind: kind, err: err}
		}
		return modelsLoadedMsg{kind: kind, rows: resp.Msg.GetModels()}
	}
}

// visionState is what the catalog claims about a model's inputs.
type visionState int

const (
	visionUnknown visionState = iota // no catalog entry at all
	visionNo
	visionYes
)

// visionKind reads the claim, and UNKNOWN is a real answer.
//
// Empty modalities means the daemon has no catalog entry for this id -- which
// is every locally-served model (ollama, LM Studio, a custom provider) -- and
// is NOT the same as "no vision". Treating empty as no is how a vision filter
// silently hides the entire local fleet.
func visionKind(r *rafikiv1.ModelRow) visionState {
	mods := r.GetInputModalities()
	if len(mods) == 0 {
		return visionUnknown
	}
	for _, m := range mods {
		if m == "image" {
			return visionYes
		}
	}
	return visionNo
}

// apply recomputes the visible rows from the filter, the vision toggle and the
// sort. Called on every keystroke; the catalog is a few hundred rows, so a
// full re-filter costs less than the bookkeeping to avoid it.
func (p *modelPicker) apply() {
	q := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	p.rows = p.rows[:0]
	for _, r := range p.all {
		if p.visionOnly && visionKind(r) == visionNo {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.GetId()), q) &&
			!strings.Contains(strings.ToLower(r.GetName()), q) {
			continue
		}
		p.rows = append(p.rows, r)
	}
	p.sortRows()
	if p.cursor >= len(p.rows) {
		p.cursor = max(0, len(p.rows)-1)
	}
}

// sortRows orders the visible rows.
//
// An ABSENT price or context sorts LAST in both orders, never as zero. A model
// the catalog does not know is not the cheapest thing available, and sorting it
// there is the exact failure the optional wire fields exist to prevent.
func (p *modelPicker) sortRows() {
	switch p.sort {
	case sortCost:
		sort.SliceStable(p.rows, func(i, j int) bool {
			a, b := p.rows[i].PromptUsd, p.rows[j].PromptUsd
			if a == nil || b == nil {
				return a != nil // known prices first
			}
			if *a != *b {
				return *a < *b
			}
			return p.rows[i].GetId() < p.rows[j].GetId()
		})
	case sortContext:
		sort.SliceStable(p.rows, func(i, j int) bool {
			a, b := p.rows[i].ContextWindow, p.rows[j].ContextWindow
			if a == nil || b == nil {
				return a != nil
			}
			if *a != *b {
				return *a > *b
			}
			return p.rows[i].GetId() < p.rows[j].GetId()
		})
	default:
		sort.SliceStable(p.rows, func(i, j int) bool {
			return p.rows[i].GetId() < p.rows[j].GetId()
		})
	}
}

func (p *modelPicker) move(delta, window int) {
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

// selected returns the highlighted model id, or "" when the list is empty.
func (p *modelPicker) selected() string {
	if p.cursor < 0 || p.cursor >= len(p.rows) {
		return ""
	}
	return p.rows[p.cursor].GetId()
}

// ── formatting ───────────────────────────────────────────────────────────────

// ctxCell renders a context window compactly. An absent one is an em dash, not
// a zero.
func ctxCell(r *rafikiv1.ModelRow) string {
	if r.ContextWindow == nil {
		return "—"
	}
	n := *r.ContextWindow
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

// priceCell renders a per-token price the way humans quote them: USD per
// million tokens. Absent stays absent -- an unpriced model must not read free.
func priceCell(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v*1e6)
}

func visionCellGlyph(r *rafikiv1.ModelRow) string {
	switch visionKind(r) {
	case visionYes:
		return "◉"
	case visionNo:
		return "·"
	}
	return "?"
}

// ── rendering ────────────────────────────────────────────────────────────────

// pickerChrome is the rows the picker spends on things that are not models:
// title, filter, blank, header, blank, footer.
const pickerChrome = 6

func (p *modelPicker) view(width, height int) string {
	var b strings.Builder
	b.WriteString(styleRailFocused.Render("model"))
	b.WriteString(styleMeta.Render("  " + p.kind))
	b.WriteString("\n")
	b.WriteString(styleMeta.Render("/ "))
	b.WriteString(p.filter.View())
	b.WriteString("\n\n")

	switch {
	case p.loading:
		b.WriteString(stylePending.Render("⏳ asking the daemon…"))
		return b.String()
	case p.err != "":
		b.WriteString(styleError.Render("✗ " + p.err))
		b.WriteString("\n\n")
		b.WriteString(styleMeta.Render("esc back — you can still type an id by hand"))
		return b.String()
	}

	idW := max(20, width-34)
	b.WriteString(styleMeta.Render(
		padTo("  MODEL", idW+2) + padTo("CONTEXT", 10) +
			padTo("IN $/M", 9) + padTo("OUT $/M", 9) + "VIS"))
	b.WriteString("\n")

	window := max(1, height-pickerChrome)
	if len(p.rows) == 0 {
		b.WriteString(styleMeta.Render("  nothing matches that filter"))
		b.WriteString("\n")
	}
	for i := p.offset; i < len(p.rows) && i < p.offset+window; i++ {
		r := p.rows[i]
		marker := "  "
		id := r.GetId()
		if i == p.cursor {
			marker = styleFocusEdge.Render("▌ ")
			id = styleRailFocused.Render(id)
		}
		b.WriteString(marker)
		b.WriteString(padTo(id, idW))
		b.WriteString("  ")
		b.WriteString(padTo(ctxCell(r), 10))
		b.WriteString(padTo(priceCell(r.PromptUsd), 9))
		b.WriteString(padTo(priceCell(r.CompletionUsd), 9))
		b.WriteString(visionCellGlyph(r))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(styleMeta.Render(p.footer()))
	return b.String()
}

// footer names the state the user cannot otherwise see: which sort is active,
// whether the vision filter is on, and how many rows it is looking at.
//
// The unknown count is not decoration. A vision filter that silently dropped
// unknown-capability models would hide every locally-served model, so they are
// KEPT and counted -- the number is what tells you the "◉" column is not the
// whole answer.
func (p *modelPicker) footer() string {
	parts := []string{
		fmt.Sprintf("%d/%d", len(p.rows), len(p.all)),
		"⇥ sort: " + p.sort.String(),
	}
	if p.visionOnly {
		parts = append(parts, "^V vision on")
	} else {
		parts = append(parts, "^V vision")
	}
	var unknown int
	for _, r := range p.rows {
		if visionKind(r) == visionUnknown {
			unknown++
		}
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown (?)", unknown))
	}
	parts = append(parts, "⏎ pick", "esc back")
	return strings.Join(parts, "   ")
}

// ── keys ─────────────────────────────────────────────────────────────────────

// handlePickerKey routes a keystroke while the model picker is up.
//
// Like the create form, the picker is checked before the cockpit's globals and
// swallows everything it does not claim: the filter is a text input, so every
// printable key belongs to it.
func (c *Cockpit) handlePickerKey(msg tea.KeyPressMsg, window int) (tea.Model, tea.Cmd) {
	p := c.picker
	switch msg.String() {
	case "esc", "ctrl+c":
		c.picker = nil
		return c, nil
	case "enter":
		if id := p.selected(); id != "" {
			c.form.inputs[fieldModel].SetValue(id)
		}
		c.picker = nil
		// Advance past the model row so the next ⏎ submits. Picking a model is
		// almost always the last thing set, and making the user tab off the
		// field they just filled is a keystroke for nothing.
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
	case "tab":
		p.sort = (p.sort + 1) % modelSortCount
		p.apply()
		return c, nil
	case "ctrl+v":
		p.visionOnly = !p.visionOnly
		p.apply()
		return c, nil
	}

	before := p.filter.Value()
	var cmd tea.Cmd
	p.filter, cmd = p.filter.Update(msg)
	if p.filter.Value() != before {
		// A changed filter restarts the list: leaving the cursor where it was
		// selects whatever happens to land under it, which is how you pick the
		// wrong model without noticing.
		p.cursor, p.offset = 0, 0
		p.apply()
	}
	return c, cmd
}

// applyModelsLoaded installs the daemon's answer, if the picker still wants it.
func (c *Cockpit) applyModelsLoaded(m modelsLoadedMsg) {
	if c.picker == nil || c.picker.kind != m.kind {
		return // cancelled, or the kind changed while the fetch was in flight
	}
	c.picker.loading = false
	if m.err != nil {
		c.picker.err = trimRPCError(m.err)
		return
	}
	c.picker.all = m.rows
	c.picker.apply()
}
