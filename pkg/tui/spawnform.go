// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	return f
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
	b.WriteString(styleMeta.Render("⇥ field   ←/→ kind   ⏎ create   esc cancel"))
	return lipgloss.NewStyle().MaxWidth(width).Render(b.String())
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
	case "tab", "down":
		f.moveFocus(+1)
		return c, nil
	case "shift+tab", "up":
		f.moveFocus(-1)
		return c, nil
	case "left":
		if f.focus == fieldKind {
			f.cycleKind(-1)
			return c, nil
		}
	case "right":
		if f.focus == fieldKind {
			f.cycleKind(+1)
			return c, nil
		}
	case " ":
		if f.focus == fieldKind {
			f.cycleKind(+1)
			return c, nil
		}
	case "enter":
		if f.busy {
			return c, nil // a second ⏎ must not submit the same form twice
		}
		p, problem := f.params()
		if problem != "" {
			f.err = problem
			return c, nil
		}
		f.busy, f.err = true, ""
		return c, c.spawnCmd(p)
	}
	return c, f.update(msg)
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
