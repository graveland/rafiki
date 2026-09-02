// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// queryDialog composes the filter and the ordering in one place.
//
// It is a BAND over the bottom of whichever list is showing, not a full-screen
// modal, so every keystroke re-sorts and re-filters the rows still visible
// above it. Choosing a query blind and then discovering what it matched is the
// interaction this replaces.
type queryDialog struct {
	band queryBand
	cell int // index within the current band
}

type queryBand int

const (
	bandFilter queryBand = iota
	bandSort
	queryBandCount
)

// filterCells are the filter band's columns: the three capability toggles,
// then a min and a max cell for every numeric field.
//
// Min and max are SEPARATE cells because a single control cannot express the
// query that motivated this: "paid" and "≤$2" are both constraints on price,
// and 7 free models pass a bare "≤$2" -- so excluding free needs the low side
// while capping spend needs the high side, at the same time.
type filterCell struct {
	field modelField
	isMax bool
	flag  filterFlag
}

type filterFlag int

const (
	flagNone filterFlag = iota
	flagTools
	flagVision
	flagThinking
)

func filterCells() []filterCell {
	out := []filterCell{
		{flag: flagTools}, {flag: flagVision}, {flag: flagThinking},
	}
	for f := modelField(0); f < modelFieldCount; f++ {
		if !f.numeric() {
			continue
		}
		if len(minStops(f)) > 1 {
			out = append(out, filterCell{field: f})
		}
		if len(maxStops(f)) > 1 {
			out = append(out, filterCell{field: f, isMax: true})
		}
	}
	return out
}

// sortCells is every field, in column order.
func sortCells() []modelField {
	out := make([]modelField, 0, modelFieldCount)
	for f := modelField(0); f < modelFieldCount; f++ {
		out = append(out, f)
	}
	return out
}

func (d *queryDialog) cellCount() int {
	if d.band == bandFilter {
		return len(filterCells())
	}
	return len(sortCells())
}

func (d *queryDialog) move(delta int) {
	n := d.cellCount()
	if n == 0 {
		return
	}
	d.cell = (d.cell + delta + n) % n
}

func (d *queryDialog) switchBand() {
	d.band = (d.band + 1) % queryBandCount
	d.cell = 0
}

// cycle advances the selected cell's value.
//
// In the filter band that is the next threshold stop; in the sort band it is
// the three-way off -> ascending -> descending, which is what makes one key do
// both "include" and "which way" without a second key.
func (d *queryDialog) cycle(v *modelView) {
	if d.band == bandFilter {
		d.cycleFilter(v)
		return
	}
	d.cycleSortCell(v)
}

func (d *queryDialog) cycleFilter(v *modelView) {
	cells := filterCells()
	if d.cell >= len(cells) {
		return
	}
	c := cells[d.cell]
	switch c.flag {
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
	b := v.boundFor(c.field)
	if c.isMax {
		b.maxIx = (b.maxIx + 1) % len(maxStops(c.field))
	} else {
		b.minIx = (b.minIx + 1) % len(minStops(c.field))
	}
	v.setBound(c.field, b)
}

// cycleSortCell walks one field through off -> asc -> desc -> off.
//
// Turning a key ON appends it, so it starts as the LAST priority and cannot
// silently displace the ordering already chosen. Priority is moved with the
// arrow keys, deliberately, rather than being a side effect of the order you
// happened to toggle things in.
func (d *queryDialog) cycleSortCell(v *modelView) {
	f := sortCells()[d.cell]
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

// reprioritize moves the selected sort key up or down the priority list.
func (d *queryDialog) reprioritize(v *modelView, delta int) {
	if d.band != bandSort {
		return
	}
	f := sortCells()[d.cell]
	for i, k := range v.keys {
		if k.field != f {
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

// adjustBound is the filter band's up/down: it steps a threshold rather than
// cycling it, so a long stop list is reachable in both directions.
func (d *queryDialog) adjustBound(v *modelView, delta int) {
	if d.band != bandFilter {
		return
	}
	cells := filterCells()
	if d.cell >= len(cells) {
		return
	}
	c := cells[d.cell]
	if c.flag != flagNone {
		return
	}
	b := v.boundFor(c.field)
	if c.isMax {
		b.maxIx = clampIx(b.maxIx+delta, len(maxStops(c.field)))
	} else {
		b.minIx = clampIx(b.minIx+delta, len(minStops(c.field)))
	}
	v.setBound(c.field, b)
}

func clampIx(i, n int) int {
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// ── rendering ────────────────────────────────────────────────────────────────

// queryDialogHeight is the rows the band always occupies: rule, filter row,
// sort row, hints. Fixed so the list above does not resize as cells change.
const queryDialogHeight = 4

func (d *queryDialog) view(width int, v modelView) string {
	var b strings.Builder
	b.WriteString(styleMeta.Render(strings.Repeat("─", max(1, width))))
	b.WriteString("\n")

	b.WriteString(d.bandLabel("FILTER", bandFilter))
	for i, c := range filterCells() {
		b.WriteString(" ")
		b.WriteString(d.cellText(c.label(v), d.band == bandFilter && d.cell == i, c.active(v)))
	}
	b.WriteString("\n")

	b.WriteString(d.bandLabel("SORT", bandSort))
	for i, f := range sortCells() {
		b.WriteString(" ")
		lbl, on := sortCellLabel(f, v)
		b.WriteString(d.cellText(lbl, d.band == bandSort && d.cell == i, on))
	}
	b.WriteString("\n")

	adjust := "↑/↓ threshold"
	if d.band == bandSort {
		adjust = "↑/↓ priority"
	}
	b.WriteString(styleMeta.Render(
		" ←/→ move   space cycle   " + adjust + "   ⇥ band   esc done"))
	return b.String()
}

func (d *queryDialog) bandLabel(name string, band queryBand) string {
	if d.band == band {
		return styleFocusBadge.Render(" " + name + " ")
	}
	return styleMeta.Render(" " + name + " ")
}

// cellText marks the SELECTED cell and, separately, whether it is ACTIVE.
// Those are different questions -- where the cursor is, and what the query
// currently constrains -- and one style cannot answer both.
func (d *queryDialog) cellText(s string, selected, active bool) string {
	switch {
	case selected:
		return styleFocusBadge.Render("[" + s + "]")
	case active:
		return styleRailFocused.Render(" " + s + " ")
	}
	return styleMeta.Render(" " + s + " ")
}

func (c filterCell) label(v modelView) string {
	switch c.flag {
	case flagTools:
		return "tools " + onOff(v.toolsOnly)
	case flagVision:
		return "vision " + onOff(v.visionOnly)
	case flagThinking:
		return "thinking " + onOff(v.thinkingOnly)
	}
	b := v.boundFor(c.field)
	stops, ix, op := minStops(c.field), b.minIx, "≥"
	if c.isMax {
		stops, ix, op = maxStops(c.field), b.maxIx, "≤"
	}
	if ix >= len(stops) {
		ix = 0
	}
	// The operator rides the CELL, not the stop, so the two cells for one
	// field are distinguishable when both are unset -- "ctx —  ctx —" does
	// not say which is the floor.
	return c.field.String() + " " + stopText(stops[ix], op)
}

func (c filterCell) active(v modelView) bool {
	switch c.flag {
	case flagTools:
		return v.toolsOnly
	case flagVision:
		return v.visionOnly
	case flagThinking:
		return v.thinkingOnly
	}
	b := v.boundFor(c.field)
	if c.isMax {
		return b.maxIx > 0
	}
	return b.minIx > 0
}

// sortCellLabel shows the field, its direction and its PRIORITY NUMBER. The
// number is the point of multi-key: without it "ctx↓ in$↑" does not say which
// one wins.
func sortCellLabel(f modelField, v modelView) (string, bool) {
	for i, k := range v.keys {
		if k.field != f {
			continue
		}
		arrow := "↑"
		if k.desc {
			arrow = "↓"
		}
		return f.String() + arrow + priorityDigit(i+1), true
	}
	return f.String(), false
}

func onOff(b bool) string {
	if b {
		return "●"
	}
	return "○"
}

// priorityDigit renders a 1-based priority compactly. Beyond nine keys the
// exact number stops mattering; "+" says "and more" without widening the cell.
func priorityDigit(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "+"
}

// ── keys ─────────────────────────────────────────────────────────────────────

// handleQueryKey routes a keystroke while the filter+sort band is open.
//
// It is checked before the picker and the form, and swallows everything it
// does not claim: the band owns the arrows, and letting them reach the list
// underneath would scroll a list whose ordering you are in the middle of
// changing.
func (c *Cockpit) handleQueryKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	d := c.query
	switch msg.String() {
	case "esc", "enter", "ctrl+s", "ctrl+c":
		c.query = nil
	case "left":
		d.move(-1)
	case "right":
		d.move(+1)
	case "tab", "shift+tab":
		d.switchBand()
	case "space":
		d.cycle(&c.modelView)
	case "up":
		d.reprioritize(&c.modelView, -1)
		d.adjustBound(&c.modelView, +1)
	case "down":
		d.reprioritize(&c.modelView, +1)
		d.adjustBound(&c.modelView, -1)
	default:
		return c, nil
	}
	c.reapplyModelQuery()
	return c, nil
}

// reapplyModelQuery re-runs the query against whichever list is open, so the
// rows above the band track every keystroke.
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

// queryRows is the height the band costs the list above it, zero when closed.
func queryRows(q *queryDialog) int {
	if q == nil {
		return 0
	}
	return queryDialogHeight
}
