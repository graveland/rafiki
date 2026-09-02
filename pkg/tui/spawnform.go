// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"go.graveland.dev/rafiki/pkg/clientstate"
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
	// viewSummary names the active sort and vision filter, set by view from the
	// cockpit's shared modelView so the hint line and the list cannot disagree.
	viewSummary string
	// suggestOff is the first row drawn. The list holds every match and the
	// panel shows a window onto it, so a filter matching 200 models is
	// navigable rather than truncated at whatever happened to fit.
	suggestOff int
}

// fieldLabelWidth is the column the row labels occupy.
const fieldLabelWidth = 8

// formChrome is the rows the form spends on things that are not suggestions:
// title, blank, four field rows, blank, hints, and the detail block.
const formChrome = 8 + detailHeight

// suggestWindow is how many suggestion rows fit in a body pane of this height.
//
// It accounts for the optional error and busy lines because they push the list
// down: a window computed as if they were absent scrolls one screenful past
// where the panel actually ends.
func (f *spawnForm) suggestWindow(height int, q *queryDialog) int {
	chrome := formChrome
	if f.err != "" {
		chrome += 2
	}
	if f.busy {
		chrome += 2
	}
	return max(1, height-chrome-queryHeight(q, height))
}

// moveSuggest walks the highlight and drags the window with it.
func (f *spawnForm) moveSuggest(delta, window int) {
	if len(f.suggest) == 0 {
		return
	}
	f.suggestCur = min(max(f.suggestCur+delta, 0), len(f.suggest)-1)
	if f.suggestCur < f.suggestOff {
		f.suggestOff = f.suggestCur
	}
	if window > 0 && f.suggestCur >= f.suggestOff+window {
		f.suggestOff = f.suggestCur - window + 1
	}
}

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
func (f *spawnForm) refreshSuggestions(all []*rafikiv1.ModelRow, v modelView) {
	f.suggest = selectModels(all, f.inputs[fieldModel].Value(), v)
	if f.suggestCur >= len(f.suggest) {
		f.suggestCur = len(f.suggest) - 1
	}
	// A new filter restarts the window as well as the highlight: scrolled 40
	// rows into the OLD list, the new one would open somewhere arbitrary.
	f.suggestOff = 0
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

// prefill seeds the form from the caller's defaults, leaving anything empty at
// the form's own default.
func (f *spawnForm) prefill(d SpawnDefaults) {
	if d.Name != "" {
		f.inputs[fieldName].SetValue(d.Name)
	}
	if d.Model != "" {
		f.inputs[fieldModel].SetValue(d.Model)
	}
	if d.Cwd != "" {
		f.inputs[fieldCwd].SetValue(d.Cwd)
	}
	for i, k := range spawnKinds {
		if k == d.Kind {
			f.kindIx = i
		}
	}
}

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
func (f *spawnForm) view(width, height int, v modelView, q *queryDialog) string {
	f.viewSummary = v.summary()
	var b strings.Builder
	b.WriteString(styleRailFocused.Render("new agent"))
	b.WriteString("\n\n")

	// bubbles renders a placeholder of exactly ONE character when Width is
	// unset: placeholderView sizes its buffer to Width()+1, copies the
	// placeholder into it, and then early-returns having emitted only p[:1].
	// "(auto)" came out as "(". Sizing the inputs to the panel fixes the
	// placeholder and lets long model ids scroll inside their own box.
	inputW := max(20, width-fieldLabelWidth-6)
	for i := spawnField(0); i < spawnFieldCount; i++ {
		if i != fieldKind {
			f.inputs[i].SetWidth(inputW)
		}
	}

	for i := spawnField(0); i < spawnFieldCount; i++ {
		marker := "  "
		if i == f.focus {
			marker = styleFocusEdge.Render("▌ ")
		}
		b.WriteString(marker)
		// padTo measures with ansi.StringWidth, so the style codes cost no columns.
		b.WriteString(padTo(styleMeta.Render(i.label()), fieldLabelWidth))

		if i == fieldKind {
			b.WriteString(f.kindView())
		} else {
			b.WriteString(f.inputs[i].View())
		}
		b.WriteString("\n")

		if i == fieldModel && f.showSuggestions() {
			b.WriteString(f.suggestView(width, f.suggestWindow(height, q), v))
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
		//
		// The count is not decoration now that the list scrolls: without it a
		// window showing 20 of 300 matches looks exactly like all 20 matches.
		pos := ""
		if n := len(f.suggest); n > 0 {
			shown := min(n, f.suggestOff+f.suggestWindow(height, q))
			if n > shown-f.suggestOff {
				pos = fmt.Sprintf("   %d-%d of %d", f.suggestOff+1, shown, n)
			}
		}
		hints = "↑/↓ pick   ⏎ take   ^S/^V/^T " + f.viewSummary + "   ^F all   ⇥" + pos
	}
	b.WriteString(styleMeta.Render(hints))
	if q != nil {
		b.WriteString("\n")
		b.WriteString(q.view(width, height, v))
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(b.String())
}

// suggestView renders the live typeahead under the model row.
//
// Each row carries the facts that decide a choice -- context, price, whether it
// sees images -- because "which of these three opus ids" is not answerable from
// the id alone. The columns are the picker's, narrowed.
func (f *spawnForm) suggestView(width, window int, v modelView) string {
	var b strings.Builder
	// The inline list has no header row, so the hint line is what names the
	// sort; the columns themselves just appear.
	extras := extraColumns(v.keys)
	extraW := 0
	for _, f := range extras {
		_, w := f.header()
		extraW += w
	}
	idW := max(20, width-32-extraW)

	now := time.Now()
	end := min(len(f.suggest), f.suggestOff+window)
	for i := f.suggestOff; i < end; i++ {
		r := f.suggest[i]
		lead := "    "
		id := r.GetId()
		if i == f.suggestCur {
			lead = "  " + styleFocusEdge.Render("▌ ")
			id = styleRailFocused.Render(id)
		}
		b.WriteString(lead)
		b.WriteString(padTo(id, idW))
		b.WriteString("  ")
		b.WriteString(styleMeta.Render(padTo(ctxCell(r), 9)))
		b.WriteString(styleMeta.Render(padTo(priceCell(r.PromptUsd), 8)))
		for _, f := range extras {
			_, w := f.header()
			b.WriteString(styleMeta.Render(padTo(cellFor(r, f, now), w)))
		}
		b.WriteString(styleMeta.Render(visionCellGlyph(r)))
		b.WriteString("\n")
	}
	var sel *rafikiv1.ModelRow
	if f.suggestCur >= 0 && f.suggestCur < len(f.suggest) {
		sel = f.suggest[f.suggestCur]
	}
	for _, line := range modelDetail(sel, now, width) {
		b.WriteString(line)
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
func (c *Cockpit) handleFormKey(msg tea.KeyPressMsg, window int) (tea.Model, tea.Cmd) {
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
			f.moveSuggest(+1, window)
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
		if f.showSuggestions() && f.suggestCur == 0 {
			// Off the top row: back to the text, and the window with it.
			f.suggestCur, f.suggestOff = -1, 0
			return c, nil
		}
		if f.showSuggestions() && f.suggestCur > 0 {
			f.moveSuggest(-1, window)
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
	case "space":
		// "space", not " ": bubbletea's KeyPressMsg.String() spells it out, so
		// a case of " " matches nothing and the binding is silently dead. This
		// shipped that way once.
		if f.focus == fieldKind {
			f.cycleKind(+1)
			return c, c.kindChanged()
		}
	case "ctrl+s":
		// The band opens over the typeahead, so the suggestions re-order under
		// it as the query is composed.
		c.query = &queryDialog{}
		return c, nil
	case "ctrl+r":
		// The one-key cycle survives for the common case: one obvious
		// ordering should not need a panel.
		c.modelView.cycleSort()
		f.suggestCur = -1
		f.refreshSuggestions(c.models[f.kind()], c.modelView)
		return c, nil
	case "ctrl+v":
		c.modelView.toggleVision()
		f.suggestCur = -1
		f.refreshSuggestions(c.models[f.kind()], c.modelView)
		return c, nil
	case "ctrl+t":
		c.modelView.toggleTools()
		f.suggestCur = -1
		f.refreshSuggestions(c.models[f.kind()], c.modelView)
		return c, nil
	case "ctrl+f":
		// The full browser: sorting by price, the vision filter, and every row
		// rather than the top six. The typeahead answers "which one was it
		// called"; this answers "what is the cheapest one that sees images".
		rows, loaded := c.modelsFor(f.kind())
		c.picker = newModelPicker(f.kind(), strings.TrimSpace(f.inputs[fieldModel].Value()),
			rows, loaded, c.modelsErr[f.kind()], c.modelView)
		return c, tea.Batch(c.fetchModelsCmd(f.kind()), textinput.Blink)
	case "enter":
		if f.busy {
			return c, nil // a second ⏎ must not submit the same form twice
		}
		// A highlighted suggestion is what ⏎ takes. With nothing highlighted
		// it submits, exactly as on every other row -- so ⏎ never means two
		// things at the same moment, only at different ones.
		if f.acceptSuggestion() {
			f.refreshSuggestions(c.models[f.kind()], c.modelView)
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
		f.refreshSuggestions(c.models[f.kind()], c.modelView)
	}
	return c, cmd
}

// kindChanged reacts to the kind row cycling: the two kinds have different
// model universes, so the typeahead must be rebuilt from the other catalog and
// that catalog may not be fetched yet.
func (c *Cockpit) kindChanged() tea.Cmd {
	kind := c.form.kind()
	// The two kinds have different model universes, so a model carried across
	// a kind change is very likely one the new kind cannot resolve. Swap in
	// that kind's own remembered model instead of leaving a stale id behind.
	c.form.inputs[fieldModel].SetValue(clientstate.LastModelFor(kind))
	c.form.suggestCur = -1
	c.form.refreshSuggestions(c.models[kind], c.modelView)
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
	// Remember the model, so the next bare `rafiki create` opens on it. Keyed
	// by kind, because the two kinds resolve different id universes.
	if c.form != nil {
		clientstate.RememberModel(c.form.kind(), strings.TrimSpace(c.form.inputs[fieldModel].Value()))
	}
	c.form = nil
	// Land on the new agent. Its rail row arrives on the event stream's
	// child_spawned, but hopping now means the pane is already open when the
	// first token lands rather than after the user goes looking for it.
	cmd := c.hop(m.childID)
	c.setNotice("created " + m.childID)
	return tea.Batch(cmd, c.leaveRail())
}
