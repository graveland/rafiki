// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// spawnKinds are the child kinds the cockpit can create, in cycle order.
//
// This is a CHOICE field rather than a text input because the set is closed and
// two characters wrong ("fundy") produces a spawn the daemon rejects after the
// form has already been dismissed. A free-text field for a two-value enum is a
// typo waiting to cost a round trip.
var spawnKinds = []string{protocol.KindFundi, protocol.KindClaude}

// spawnField indexes the form's rows. The order is the tab order, and it runs
// most-edited to least: a name is what you always set and a cwd is what you
// almost never do.
type spawnField int

const (
	fieldName spawnField = iota
	fieldKind
	fieldModel
	fieldCwd
	spawnFieldCount
)

func (f spawnField) label() string {
	switch f {
	case fieldName:
		return "name"
	case fieldKind:
		return "kind"
	case fieldModel:
		return "model"
	case fieldCwd:
		return "cwd"
	}
	return "?"
}

// spawnParams is what the form produces. A separate type from the wire request
// so the form owes nothing to the generated package.
type spawnParams struct {
	name  string
	kind  string
	model string
	cwd   string
}

// spawnForm is the modal shown by `n` on the agents pane.
//
// It is deliberately four fields. The Connect SpawnRequest carries labels, an
// executor selector and three budgets as well, and its own comment says not to
// grow it without a consumer for each field -- the same reasoning applies to
// the form: those are `rafiki create` flags, set once in a script, not things
// anyone fills in by hand between two keystrokes.
type spawnForm struct {
	inputs [spawnFieldCount]textinput.Model
	kindIx int
	focus  spawnField

	// err is the daemon's refusal, kept so the form can stay open with the
	// values still in it. Dismissing a form on failure throws away exactly the
	// input the user needs to correct.
	err string
	// busy is set while a spawn is in flight, so a second Enter cannot submit
	// the same form twice.
	busy bool

	// suggest is the live typeahead under the model row, recomputed on every
	// keystroke from the cockpit's cached catalog. Filtering locally is what
	// makes it live: asking the daemon per character would put a round trip
	// between the key and the list.
	suggest []*rafikiv1.ModelRow
	// suggestCur is the highlighted suggestion, or -1 for none. The distinction
	// is load-bearing: with nothing highlighted ⏎ SUBMITS, exactly as it does
	// on every other row, so the key never means two things at once.
	suggestCur int
}

// maxSuggestions bounds the typeahead. Six is enough to recognise the one you
// meant without the list pushing the cwd row off a short terminal; the full
// picker is where you go to see all of them.
const maxSuggestions = 6

// newSpawnForm builds the modal with sensible prefills.
//
// The cwd prefill is the CLIENT's working directory, which is right for a local
// daemon and a guess for a remote one -- the path may not exist on the daemon's
// machine at all. It is still the best default (the common case is local), and
// a wrong one surfaces as the daemon's own error in the form rather than as a
// silent misplacement, because Spawn refuses a cwd it cannot use.
func newSpawnForm() *spawnForm {
	f := &spawnForm{}
	for i := range f.inputs {
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 0
		f.inputs[i] = in
	}
	f.inputs[fieldName].Placeholder = "(auto)"
	f.inputs[fieldModel].Placeholder = "(daemon default)"
	if wd, err := os.Getwd(); err == nil {
		f.inputs[fieldCwd].SetValue(wd)
	}
	f.inputs[fieldName].Focus()
	f.suggestCur = -1
	return f
}

// refreshSuggestions recomputes the typeahead from the catalog and whatever is
// typed in the model row.
//
// An empty query still lists: ↓ into an untouched field is a legitimate way to
// browse, and an empty box that answers nothing looks broken.
func (f *spawnForm) refreshSuggestions(all []*rafikiv1.ModelRow) {
	q := strings.ToLower(strings.TrimSpace(f.inputs[fieldModel].Value()))
	f.suggest = f.suggest[:0]
	for _, r := range all {
		if len(f.suggest) == maxSuggestions {
			break
		}
		if q != "" && !strings.Contains(strings.ToLower(r.GetId()), q) &&
			!strings.Contains(strings.ToLower(r.GetName()), q) {
			continue
		}
		f.suggest = append(f.suggest, r)
	}
	if f.suggestCur >= len(f.suggest) {
		f.suggestCur = len(f.suggest) - 1
	}
}

// showSuggestions reports whether the typeahead is on screen. It follows FOCUS,
// so tabbing off the model row hides the list rather than leaving it floating
// under a field nobody is editing.
func (f *spawnForm) showSuggestions() bool {
	return f.focus == fieldModel && len(f.suggest) > 0
}

// acceptSuggestion fills the field from the highlighted row. false means
// nothing was highlighted, which is the caller's cue that ⏎ meant submit.
func (f *spawnForm) acceptSuggestion() bool {
	if f.suggestCur < 0 || f.suggestCur >= len(f.suggest) {
		return false
	}
	f.inputs[fieldModel].SetValue(f.suggest[f.suggestCur].GetId())
	f.suggestCur = -1
	return true
}

func (f *spawnForm) kind() string { return spawnKinds[f.kindIx] }

// params returns what to spawn, or an error message if the form is incomplete.
func (f *spawnForm) params() (spawnParams, string) {
	cwd := strings.TrimSpace(f.inputs[fieldCwd].Value())
	if cwd == "" {
		return spawnParams{}, "cwd is required"
	}
	return spawnParams{
		name:  strings.TrimSpace(f.inputs[fieldName].Value()),
		kind:  f.kind(),
		model: strings.TrimSpace(f.inputs[fieldModel].Value()),
		cwd:   cwd,
	}, ""
}

// moveFocus cycles the focused row, wrapping in both directions. Only text
// rows take the textinput focus flag; the kind row has no cursor.
func (f *spawnForm) moveFocus(delta int) {
	f.inputs[f.focus].Blur()
	next := (int(f.focus) + delta + int(spawnFieldCount)) % int(spawnFieldCount)
	f.focus = spawnField(next)
	if f.focus != fieldKind {
		f.inputs[f.focus].Focus()
	}
}

func (f *spawnForm) cycleKind(delta int) {
	f.kindIx = (f.kindIx + delta + len(spawnKinds)) % len(spawnKinds)
}

// update routes a keystroke into the focused row. Returns the command the
// textinput asked for, if any.
func (f *spawnForm) update(msg tea.KeyPressMsg) tea.Cmd {
	if f.focus == fieldKind {
		// The kind row consumes nothing else: a stray letter here must not
		// vanish into an input with no visible cursor.
		return nil
	}
	var cmd tea.Cmd
	f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
	return cmd
}

// view renders the modal. width is the body pane's width.
func (f *spawnForm) view(width int) string {
	var b strings.Builder
	b.WriteString(styleRailFocused.Render("new agent"))
	b.WriteString("\n\n")

	for i := spawnField(0); i < spawnFieldCount; i++ {
		marker := "  "
		if i == f.focus {
			marker = styleFocusEdge.Render("▌ ")
		}
		b.WriteString(marker)
		// padTo measures with ansi.StringWidth, so the style codes cost no columns.
		b.WriteString(padTo(styleMeta.Render(i.label()), 8))

		if i == fieldKind {
			b.WriteString(f.kindView())
		} else {
			b.WriteString(f.inputs[i].View())
		}
		b.WriteString("\n")

		if i == fieldModel && f.showSuggestions() {
			b.WriteString(f.suggestView(width))
		}
	}

	if f.err != "" {
		b.WriteString("\n")
		b.WriteString(styleError.Render("✗ " + f.err))
		b.WriteString("\n")
	}
	if f.busy {
		b.WriteString("\n")
		b.WriteString(stylePending.Render("⏳ creating…"))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	// The ⏎ hint is contextual because ⏎ genuinely does two things, and a
	// static footer would be wrong on one row out of four.
	hints := "⇥ field   ←/→ kind   ⏎ create   esc cancel"
	if f.focus == fieldModel {
		// The model row has its own vocabulary and it is worth the line: ↑/↓
		// mean the list here rather than the field ring, and ^F is the only
		// route to the sort and vision filters.
		hints = "↑/↓ pick   ⏎ take   ^F all models   ⇥ field   esc cancel"
	}
	b.WriteString(styleMeta.Render(hints))
	return lipgloss.NewStyle().MaxWidth(width).Render(b.String())
}

// suggestView renders the live typeahead under the model row.
//
// Each row carries the facts that decide a choice -- context, price, whether it
// sees images -- because "which of these three opus ids" is not answerable from
// the id alone. The columns are the picker's, narrowed.
func (f *spawnForm) suggestView(width int) string {
	var b strings.Builder
	for i, r := range f.suggest {
		lead := "    "
		id := r.GetId()
		if i == f.suggestCur {
			lead = "  " + styleFocusEdge.Render("▌ ")
			id = styleRailFocused.Render(id)
		}
		b.WriteString(lead)
		b.WriteString(padTo(id, max(20, width-32)))
		b.WriteString("  ")
		b.WriteString(styleMeta.Render(padTo(ctxCell(r), 9)))
		b.WriteString(styleMeta.Render(padTo(priceCell(r.PromptUsd), 8)))
		b.WriteString(styleMeta.Render(visionCellGlyph(r)))
		b.WriteString("\n")
	}
	return b.String()
}

// kindView renders the choice row as its options, the selected one marked.
// Showing both is the point: a single value gives no hint that it can change.
func (f *spawnForm) kindView() string {
	parts := make([]string, 0, len(spawnKinds))
	for i, k := range spawnKinds {
		if i == f.kindIx {
			parts = append(parts, styleRailFocused.Render("("+k+")"))
			continue
		}
		parts = append(parts, styleMeta.Render(" "+k+" "))
	}
	return strings.Join(parts, " ")
}

// handleFormKey routes a keystroke while the create modal is up.
//
// The modal is checked before the cockpit's globals, so everything it does not
// claim is swallowed rather than falling through -- a ⇥ that reached cyclePane
// would move focus to a pane hidden behind the modal.
func (c *Cockpit) handleFormKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	f := c.form
	switch msg.String() {
	case "esc", "ctrl+c":
		// ^C cancels the modal rather than arming quit: in a modal that is
		// what it means everywhere, and esc is right beside it.
		c.form = nil
		return c, nil
	case "tab":
		f.suggestCur = -1
		f.moveFocus(+1)
		return c, c.fetchModelsCmd(f.kind())
	case "shift+tab":
		f.suggestCur = -1
		f.moveFocus(-1)
		return c, c.fetchModelsCmd(f.kind())
	case "down":
		// On the model row ↓ walks INTO the typeahead rather than to the next
		// field. Leaving the row is still ⇥, which is the key that always
		// means that; ↓ next to a visible list can only sensibly mean the list.
		if f.showSuggestions() && f.suggestCur < len(f.suggest)-1 {
			f.suggestCur++
			return c, nil
		}
		if f.showSuggestions() && f.suggestCur >= 0 {
			return c, nil // already at the bottom; do not fall out of the list
		}
		f.suggestCur = -1
		f.moveFocus(+1)
		return c, c.fetchModelsCmd(f.kind())
	case "up":
		// ↑ off the top returns to the text, not to the previous field: the
		// way out of a typeahead is back to what you were typing.
		if f.showSuggestions() && f.suggestCur >= 0 {
			f.suggestCur--
			return c, nil
		}
		f.suggestCur = -1
		f.moveFocus(-1)
		return c, c.fetchModelsCmd(f.kind())
	case "left":
		if f.focus == fieldKind {
			f.cycleKind(-1)
			return c, c.kindChanged()
		}
	case "right":
		if f.focus == fieldKind {
			f.cycleKind(+1)
			return c, c.kindChanged()
		}
	case " ":
		if f.focus == fieldKind {
			f.cycleKind(+1)
			return c, c.kindChanged()
		}
	case "ctrl+f":
		// The full browser: sorting by price, the vision filter, and every row
		// rather than the top six. The typeahead answers "which one was it
		// called"; this answers "what is the cheapest one that sees images".
		rows, loaded := c.modelsFor(f.kind())
		c.picker = newModelPicker(f.kind(), strings.TrimSpace(f.inputs[fieldModel].Value()),
			rows, loaded, c.modelsErr[f.kind()])
		return c, tea.Batch(c.fetchModelsCmd(f.kind()), textinput.Blink)
	case "enter":
		if f.busy {
			return c, nil // a second ⏎ must not submit the same form twice
		}
		// A highlighted suggestion is what ⏎ takes. With nothing highlighted
		// it submits, exactly as on every other row -- so ⏎ never means two
		// things at the same moment, only at different ones.
		if f.acceptSuggestion() {
			f.refreshSuggestions(c.models[f.kind()])
			return c, nil
		}
		p, problem := f.params()
		if problem != "" {
			f.err = problem
			return c, nil
		}
		f.busy, f.err = true, ""
		return c, c.spawnCmd(p)
	}

	before := f.inputs[fieldModel].Value()
	cmd := f.update(msg)
	if f.focus == fieldModel && f.inputs[fieldModel].Value() != before {
		// Live: every keystroke re-filters. The highlight drops back to the
		// text, because a cursor left on row 3 of the OLD list selects
		// whatever now happens to sit there.
		f.suggestCur = -1
		f.refreshSuggestions(c.models[f.kind()])
	}
	return c, cmd
}

// kindChanged reacts to the kind row cycling: the two kinds have different
// model universes, so the typeahead must be rebuilt from the other catalog and
// that catalog may not be fetched yet.
func (c *Cockpit) kindChanged() tea.Cmd {
	kind := c.form.kind()
	c.form.suggestCur = -1
	c.form.refreshSuggestions(c.models[kind])
	return c.fetchModelsCmd(kind)
}

// applySpawned handles the daemon's answer to a create.
//
// A failure keeps the modal OPEN with its values intact: the daemon's refusal
// is usually about one field (a cwd that does not exist on its machine is the
// common one for a remote daemon), and dismissing the form would throw away
// exactly the input needed to fix it.
func (c *Cockpit) applySpawned(m spawnedMsg) tea.Cmd {
	if c.form != nil {
		c.form.busy = false
	}
	if m.err != nil {
		if c.form != nil {
			c.form.err = trimRPCError(m.err)
		}
		return nil
	}
	c.form = nil
	// Land on the new agent. Its rail row arrives on the event stream's
	// child_spawned, but hopping now means the pane is already open when the
	// first token lands rather than after the user goes looking for it.
	cmd := c.hop(m.childID)
	c.setNotice("created " + m.childID)
	return tea.Batch(cmd, c.leaveRail())
}
