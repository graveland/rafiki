// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// queryDialog composes the filter and the ordering in one table.
//
// VERTICAL, one row per field. The first version laid the cells out in two
// horizontal bands and needed sixteen of them side by side, which overflowed
// any terminal under ~140 columns and would have needed wrapping logic whose
// row count changed as values were cycled -- resizing the list mid-edit.
// Turning the table on its side removes the problem rather than managing it:
// height is what terminals have to spare, a field's whole state reads on one
// line, and the filter/sort split stops being two modes to switch between.
//
// It still sits OVER the list, so every keystroke re-filters the rows visible
// above it. Choosing a query blind and then discovering what it matched is the
// interaction this exists to replace.
type queryDialog struct {
	row int
	col int
	// off is the first row drawn: the table is taller than a short terminal
	// can spare, so it windows like every other list here.
	off int
}

// The three columns. A row carries a value in each only where one is
// meaningful -- a boolean capability has no maximum, and the model id has no
// numeric bound at all.
const (
	colMinCell = iota
	colMaxCell
	colSortCell
	queryColCount
)

// filterFlag names the boolean capability filters, which have no threshold to
// cycle and so occupy a single cell.
type filterFlag int

const (
	flagNone filterFlag = iota
	flagTools
	flagVision
	flagThinking
)

// queryRow is one line of the table: either a capability toggle or a field.
type queryRow struct {
	flag  filterFlag
	field modelField
}

func (r queryRow) label() string {
	switch r.flag {
	case flagTools:
		return "tools"
	case flagVision:
		return "vision"
	case flagThinking:
		return "thinking"
	}
	return r.field.String()
}

// queryRows lists the table in display order: the three capability toggles
// first, because they are the coarsest cut and the ones most often changed.
func queryRowsList() []queryRow {
	out := []queryRow{{flag: flagTools}, {flag: flagVision}, {flag: flagThinking}}
	for f := modelField(0); f < modelFieldCount; f++ {
		out = append(out, queryRow{field: f})
	}
	return out
}

// available reports whether a cell can hold anything. Navigation skips the
// ones that cannot, so ←/→ never parks on a cell where space does nothing.
func (r queryRow) available(col int) bool {
	if r.flag != flagNone {
		return col == colMinCell // a toggle occupies the first column only
	}
	switch col {
	case colMinCell:
		// The stop lists are the single source for whether a field can be
		// bounded: colModel and colAge declare none, so no separate "is this
		// numeric" predicate is needed.
		return len(minStops(r.field)) > 1
	case colMaxCell:
		return len(maxStops(r.field)) > 1
	case colSortCell:
		return true // every field can be sorted, including the id
	}
	return false
}

func (d *queryDialog) moveRow(delta, window int) {
	rows := queryRowsList()
	d.row = min(max(d.row+delta, 0), len(rows)-1)
	if !rows[d.row].available(d.col) {
		d.col = firstAvailable(rows[d.row], d.col)
	}
	if d.row < d.off {
		d.off = d.row
	}
	if window > 0 && d.row >= d.off+window {
		d.off = d.row - window + 1
	}
}

func (d *queryDialog) moveCol(delta int) {
	r := queryRowsList()[d.row]
	for i := 0; i < queryColCount; i++ {
		next := (d.col + delta*(i+1) + queryColCount*len(queryRowsList())) % queryColCount
		if r.available(next) {
			d.col = next
			return
		}
	}
}

// firstAvailable finds a usable column near the one the cursor was in, so a
// vertical move lands somewhere sensible rather than always snapping left.
func firstAvailable(r queryRow, want int) int {
	if r.available(want) {
		return want
	}
	for c := 0; c < queryColCount; c++ {
		if r.available(c) {
			return c
		}
	}
	return colMinCell
}

// cycle advances the selected cell.
//
// On a bound that is the next threshold stop; on the sort column it is the
// three-way off -> ascending -> descending, which is what lets one key mean
// both "include this" and "which way".
func (d *queryDialog) cycle(v *modelView) {
	r := queryRowsList()[d.row]
	switch r.flag {
	case flagTools:
		v.toggleTools()
		return
	case flagVision:
		v.toggleVision()
		return
	case flagThinking:
		v.thinkingOnly = !v.thinkingOnly
		return
	}
	switch d.col {
	case colSortCell:
		d.cycleSortCell(v, r.field)
	case colMinCell:
		b := v.boundFor(r.field)
		b.minIx = (b.minIx + 1) % len(minStops(r.field))
		v.setBound(r.field, b)
	case colMaxCell:
		b := v.boundFor(r.field)
		b.maxIx = (b.maxIx + 1) % len(maxStops(r.field))
		v.setBound(r.field, b)
	}
}

// cycleSortCell walks one field through off -> asc -> desc -> off.
//
// Turning a key ON appends it, so it starts LAST in priority and cannot
// silently displace the ordering already chosen. Priority moves deliberately,
// with ⇧↑/⇧↓, rather than being a side effect of the order things were
// toggled in.
func (d *queryDialog) cycleSortCell(v *modelView, f modelField) {
	for i, k := range v.keys {
		if k.field != f {
			continue
		}
		if !k.desc {
			v.keys[i].desc = true
			return
		}
		v.keys = append(v.keys[:i], v.keys[i+1:]...)
		return
	}
	v.keys = append(v.keys, sortKey{field: f})
}

// reprioritize moves the selected field's sort key up or down the list.
func (d *queryDialog) reprioritize(v *modelView, delta int) {
	r := queryRowsList()[d.row]
	if r.flag != flagNone {
		return
	}
	for i, k := range v.keys {
		if k.field != r.field {
			continue
		}
		j := i + delta
		if j < 0 || j >= len(v.keys) {
			return
		}
		v.keys[i], v.keys[j] = v.keys[j], v.keys[i]
		return
	}
}

// ── rendering ────────────────────────────────────────────────────────────────

// queryChrome is the rows the panel spends on things that are not fields:
// a rule, a header, and a hints line.
const queryChrome = 3

// maxQueryRows caps how much of the panel the table takes, so the list it is
// filtering never disappears behind it. The table windows when it does not fit.
const maxQueryRows = 13

// queryHeight is the panel's total height for a body pane of this height.
func queryHeight(q *queryDialog, bodyHeight int) int {
	if q == nil {
		return 0
	}
	return queryChrome + queryWindow(bodyHeight)
}

// queryWindow is how many table rows are drawn. It leaves at least a few rows
// of the list visible: a filter you cannot see the effect of is the thing this
// design exists to avoid.
func queryWindow(bodyHeight int) int {
	rows := len(queryRowsList())
	if rows > maxQueryRows {
		rows = maxQueryRows
	}
	if spare := bodyHeight - queryChrome - 4; spare < rows {
		rows = spare
	}
	return max(1, rows)
}

func (d *queryDialog) view(width, bodyHeight int, v modelView) string {
	window := queryWindow(bodyHeight)
	rows := queryRowsList()

	var b strings.Builder
	b.WriteString(styleMeta.Render(strings.Repeat("─", max(1, width))))
	b.WriteString("\n")
	b.WriteString(styleMeta.Render(
		"  " + padTo("FIELD", 12) + padTo("MIN", 12) + padTo("MAX", 12) + "SORT"))
	b.WriteString("\n")

	end := min(len(rows), d.off+window)
	for i := d.off; i < end; i++ {
		r := rows[i]
		lead := "  "
		if i == d.row {
			lead = styleFocusEdge.Render("▌ ")
		}
		b.WriteString(lead)
		b.WriteString(padTo(styleMeta.Render(r.label()), 12))
		for c := 0; c < queryColCount; c++ {
			b.WriteString(padTo(d.cellText(r, c, v, i == d.row && c == d.col), 12))
		}
		b.WriteString("\n")
	}

	hint := " ↑/↓ field   ←/→ column   space cycle   esc done"
	if d.col == colSortCell {
		hint = " ↑/↓ field   ←/→ column   space cycle   ⇧↑/⇧↓ priority   esc done"
	}
	pos := ""
	if len(rows) > window {
		pos = fmt.Sprintf("   %d-%d of %d", d.off+1, end, len(rows))
	}
	b.WriteString(styleMeta.Render(hint + pos))
	return b.String()
}

// cellText renders one cell, marking SELECTED and ACTIVE separately. Those are
// different questions -- where the cursor is, and what the query constrains --
// and one style cannot answer both.
func (d *queryDialog) cellText(r queryRow, col int, v modelView, selected bool) string {
	text, active := d.cellValue(r, col, v)
	if text == "" {
		return "" // nothing to show; navigation never lands here
	}
	switch {
	case selected:
		return styleFocusBadge.Render("[" + text + "]")
	case active:
		return styleRailFocused.Render(" " + text + " ")
	}
	return styleMeta.Render(" " + text + " ")
}

func (d *queryDialog) cellValue(r queryRow, col int, v modelView) (string, bool) {
	if !r.available(col) {
		return "", false
	}
	if r.flag != flagNone {
		switch r.flag {
		case flagTools:
			return onOffWord(v.toolsOnly), v.toolsOnly
		case flagVision:
			return onOffWord(v.visionOnly), v.visionOnly
		case flagThinking:
			return onOffWord(v.thinkingOnly), v.thinkingOnly
		}
	}
	b := v.boundFor(r.field)
	switch col {
	case colMinCell:
		stops := minStops(r.field)
		ix := min(b.minIx, len(stops)-1)
		return stopText(stops[ix], "≥"), ix > 0
	case colMaxCell:
		stops := maxStops(r.field)
		ix := min(b.maxIx, len(stops)-1)
		return stopText(stops[ix], "≤"), ix > 0
	case colSortCell:
		for i, k := range v.keys {
			if k.field != r.field {
				continue
			}
			arrow := "↑"
			if k.desc {
				arrow = "↓"
			}
			// The priority NUMBER is the point of multi-key: without it
			// "ctx↓ in$↑" does not say which one wins.
			return arrow + " " + priorityDigit(i+1), true
		}
		return "—", false
	}
	return "", false
}

func onOffWord(b bool) string {
	if b {
		return "● on"
	}
	return "○ off"
}

// priorityDigit renders a 1-based priority compactly. Past nine keys the exact
// number stops mattering; "+" says "and more" without widening the cell.
func priorityDigit(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "+"
}

// ── keys ─────────────────────────────────────────────────────────────────────

// handleQueryKey routes a keystroke while the filter+sort panel is open.
//
// It is checked before the picker and the form, and swallows everything it does
// not claim: the panel owns the arrows, and letting them through would scroll a
// list whose ordering is mid-edit.
func (c *Cockpit) handleQueryKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	d := c.query
	window := queryWindow(c.bodyHeight())
	switch msg.String() {
	case "esc", "enter", "ctrl+s", "ctrl+c":
		c.query = nil
	case "up":
		d.moveRow(-1, window)
	case "down":
		d.moveRow(+1, window)
	case "left":
		d.moveCol(-1)
	case "right", "tab":
		d.moveCol(+1)
	case "space":
		// "space", not " ": KeyPressMsg.String() spells it out, and a case of
		// " " is a silently dead binding. This repo has shipped that once.
		d.cycle(&c.modelView)
	case "shift+up":
		d.reprioritize(&c.modelView, -1)
	case "shift+down":
		d.reprioritize(&c.modelView, +1)
	default:
		return c, nil
	}
	c.reapplyModelQuery()
	return c, nil
}

// reapplyModelQuery re-runs the query against whichever list is open, so the
// rows above the panel track every keystroke.
func (c *Cockpit) reapplyModelQuery() {
	if c.picker != nil {
		c.picker.cursor, c.picker.offset = 0, 0
		c.picker.apply(c.modelView)
	}
	if c.form != nil {
		c.form.suggestCur = -1
		c.form.refreshSuggestions(c.models[c.form.kind()], c.modelView)
	}
}
