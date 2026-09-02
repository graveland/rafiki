// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case " ":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func formCockpit(t *testing.T) *Cockpit {
	t.Helper()
	c := railWith(t, "c_1")
	c.handleKey(keyMsg("n"))
	if c.form == nil {
		t.Fatal("n on the agents pane did not open the create form")
	}
	return c
}

func TestNOpensTheCreateForm(t *testing.T) {
	formCockpit(t)
}

// n must do nothing from the input pane: it is a letter, and typing "now" into
// a prompt must not spawn an agent.
func TestNInTheInputPaneIsJustALetter(t *testing.T) {
	c := newTestCockpit("c_1")
	c.focus = focusInput
	c.handleKey(keyMsg("n"))

	if c.form != nil {
		t.Fatal("n opened the create form while typing")
	}
	if !strings.Contains(c.ta.Value(), "n") {
		t.Errorf("textarea = %q, want the letter to have been typed", c.ta.Value())
	}
}

// The modal is checked BEFORE the globals, so tab must move between fields
// rather than reaching cyclePane.
func TestTabCyclesFieldsRatherThanPanes(t *testing.T) {
	c := formCockpit(t)
	before := c.focus

	c.handleKey(keyMsg("tab"))

	if c.form.focus != fieldKind {
		t.Errorf("form focus = %v, want fieldKind", c.form.focus)
	}
	if c.focus != before {
		t.Error("tab reached the pane ring; a modal must own that key")
	}
}

func TestFieldFocusWrapsBothWays(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("shift+tab"))
	if c.form.focus != spawnFieldCount-1 {
		t.Errorf("focus = %v, want the last field after wrapping back", c.form.focus)
	}
	c.handleKey(keyMsg("tab"))
	if c.form.focus != fieldName {
		t.Errorf("focus = %v, want fieldName after wrapping forward", c.form.focus)
	}
}

func TestKindCyclesAndIsNeverFreeText(t *testing.T) {
	c := formCockpit(t)
	c.form.focus = fieldKind

	first := c.form.kind()
	c.handleKey(keyMsg("right"))
	if c.form.kind() == first {
		t.Fatal("→ did not change the kind")
	}
	c.handleKey(keyMsg("left"))
	if c.form.kind() != first {
		t.Error("← did not restore the kind; the cycle must be symmetric")
	}

	// A letter on the kind row must be swallowed, not typed anywhere.
	c.handleKey(keyMsg("z"))
	for _, k := range spawnKinds {
		if c.form.kind() == k {
			return
		}
	}
	t.Errorf("kind = %q, which is not one of %v", c.form.kind(), spawnKinds)
}

func TestEscapeCancelsTheForm(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("esc"))
	if c.form != nil {
		t.Error("esc did not close the form")
	}
}

// ^C in a modal cancels the modal. It must NOT arm the cockpit's quit.
func TestCtrlCCancelsTheFormWithoutArmingQuit(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	if c.form != nil {
		t.Error("^C did not close the form")
	}
	if !c.quitArmed.IsZero() {
		t.Error("^C armed quit from inside a modal")
	}
}

// cwd is required by the server. Catching it here keeps the values on screen
// instead of spending a round trip to be told.
func TestEmptyCwdIsRefusedLocally(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldCwd].SetValue("")

	_, cmd := c.handleKey(keyMsg("enter"))

	if cmd != nil {
		t.Error("submitted with no cwd; the server would refuse it")
	}
	if c.form == nil {
		t.Fatal("form closed on a validation failure")
	}
	if !strings.Contains(c.form.err, "cwd") {
		t.Errorf("err = %q, want it to name cwd", c.form.err)
	}
}

func TestCwdIsPrefilled(t *testing.T) {
	c := formCockpit(t)
	if c.form.inputs[fieldCwd].Value() == "" {
		t.Error("cwd was not prefilled; every create would need it typed by hand")
	}
}

// A refused spawn keeps the form and its values: the daemon's complaint is
// usually about one field, and dismissing throws away what needs correcting.
func TestSpawnFailureKeepsTheFormAndItsValues(t *testing.T) {
	c := formCockpit(t)
	c.form.inputs[fieldName].SetValue("scout")
	c.form.busy = true

	c.applySpawned(spawnedMsg{err: errors.New("internal: no such directory")})

	if c.form == nil {
		t.Fatal("a failed spawn dismissed the form")
	}
	if c.form.inputs[fieldName].Value() != "scout" {
		t.Error("the typed name was lost")
	}
	if c.form.busy {
		t.Error("form still busy after a failure; a retry would be impossible")
	}
	if !strings.Contains(c.form.err, "no such directory") {
		t.Errorf("err = %q, want the daemon's reason", c.form.err)
	}
}

func TestSpawnSuccessClosesTheForm(t *testing.T) {
	c := formCockpit(t)
	c.form.busy = true

	c.applySpawned(spawnedMsg{childID: "c_new"})

	if c.form != nil {
		t.Error("form stayed open after a successful create")
	}
}

// A second Enter while the first is in flight must not create two agents.
func TestBusyFormRefusesASecondSubmit(t *testing.T) {
	c := formCockpit(t)
	c.form.busy = true

	if _, cmd := c.handleKey(keyMsg("enter")); cmd != nil {
		t.Error("a busy form submitted again")
	}
}

// The modal must render instead of the transcript, and ahead of the help sheet.
func TestFormOwnsTheBodyPane(t *testing.T) {
	c := formCockpit(t)
	c.showHelp = true
	c.width, c.height, c.ready = 100, 30, true

	out := ansi.Strip(c.View().Content)
	if !strings.Contains(out, "new agent") {
		t.Error("the form did not render")
	}
	if strings.Contains(out, "closes this") {
		t.Error("the help sheet rendered over the modal")
	}
}

func TestFormShowsEveryFieldAndBothKinds(t *testing.T) {
	c := formCockpit(t)
	out := c.form.view(80, 24, c.modelView)
	for _, want := range []string{"name", "kind", "model", "cwd"} {
		if !strings.Contains(out, want) {
			t.Errorf("form view is missing the %q row", want)
		}
	}
	// Both kinds are shown, not just the selected one: a single value gives no
	// hint that the row can change.
	for _, k := range spawnKinds {
		if !strings.Contains(out, k) {
			t.Errorf("form view does not offer kind %q", k)
		}
	}
}

// A bubbles input is constructed BLURRED and its Update returns immediately
// while it is, swallowing every printable key with no error anywhere. The
// cockpit shipped exactly that bug once — the whole three-pane UI could not be
// typed into — so the form gets the same guard the textarea has: drive a rune
// through the real key path and assert it landed.
func TestTypingReachesTheFocusedFormField(t *testing.T) {
	c := formCockpit(t)

	for _, r := range "scout" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if got := c.form.inputs[fieldName].Value(); got != "scout" {
		t.Fatalf("name field = %q, want scout — the input is not focused", got)
	}
}

// Focus must MOVE with the tab order, not stay on the first field. Blurring the
// old row and focusing the new one are two separate calls, and getting only the
// first right yields a form where every row after the first is dead.
func TestTypingFollowsTheFocusedField(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(keyMsg("tab")) // kind
	c.handleKey(keyMsg("tab")) // model

	for _, r := range "opus" {
		c.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	if got := c.form.inputs[fieldModel].Value(); got != "opus" {
		t.Errorf("model field = %q, want opus", got)
	}
	if got := c.form.inputs[fieldName].Value(); got != "" {
		t.Errorf("name field = %q, want empty — the old row kept focus", got)
	}
}
