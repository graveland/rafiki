// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/multigres/testkit/assert"
)

// A bracketed paste is a tea.PasteMsg, not a keystroke, so it never reaches
// handleKey: without a modal branch in Update every paste fell through to the
// prompt box BEHIND the open modal, and with the pane focus on the rail
// handlePaste's own focusInput guard dropped it instead. ⌘V into the create
// form's name row did nothing at all, which is exactly the shape of "this field
// refuses to paste".
func TestPasteFillsTheSpawnFormNameRow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	ck.Require().NotNil(c.form, "create form did not open")

	c.Update(tea.PasteMsg{Content: "beta-agent"})

	ck.Eq("beta-agent", c.form.inputs[fieldName].Value(), "name row after paste")
	ck.Eq("", c.ta.Value(), "the paste leaked into the prompt box behind the modal")
}

// The kind row is a chooser, not a text field: a paste there must be dropped
// rather than land in an input nobody can see.
func TestPasteIntoTheSpawnFormKindRowIsDropped(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	c.handleKey(keyMsg("down"))
	ck.Require().Eq(fieldKind, c.form.focus, "focus is not on the kind row")
	before := c.form.kind()

	c.Update(tea.PasteMsg{Content: "claude"})

	ck.Eq(before, c.form.kind(), "kind changed on a paste")
	ck.Eq("", c.ta.Value(), "the paste leaked into the prompt box behind the modal")
}

// Pasting a model id is a common way to fill the model row, and the typeahead
// must re-filter: a list still filtered by the previous value would hide the
// model just pasted.
func TestPasteIntoTheModelRowRefiltersTheTypeahead(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	seedModels(c, c.form.kind(), modelRows())
	focusModelRow(c)

	c.Update(tea.PasteMsg{Content: "deepseek"})

	ck.Eq("deepseek", c.form.inputs[fieldModel].Value(), "model row after paste")
	ck.Len(c.form.suggest, 1, "typeahead rows = %d, want the one match", len(c.form.suggest))
	if len(c.form.suggest) == 1 {
		ck.Eq("deepseek/chat", c.form.suggest[0].GetId(), "typeahead row")
	}
}

// The browser's filter box is a text field too, and a changed filter restarts
// the list: a cursor left where it was selects whatever lands under it.
func TestPasteReachesTheModelPickerFilter(t *testing.T) {
	ck := assert.NewCollecting(t)
	c, p := loadedPicker(t)
	p.move(1, 10)
	ck.Require().Eq(1, p.cursor, "cursor did not move, the reset below proves nothing")

	c.Update(tea.PasteMsg{Content: "deep"})

	ck.Eq("deep", p.filter.Value(), "picker filter after paste")
	ck.Len(p.rows, 1, "filtered rows = %d, want just deepseek/chat", len(p.rows))
	ck.Eq(0, p.cursor, "cursor not reset by a pasted filter")
}

func TestPasteReachesTheRenameForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1")
	c.handleKey(keyMsg("R"))
	ck.Require().NotNil(c.renameForm, "R on the rail did not open the rename form")
	// The field opens prefilled with the current name; the paste inserts at the
	// cursor, so clear it to assert about the paste rather than the prefill.
	c.renameForm.input.SetValue("")

	c.Update(tea.PasteMsg{Content: "renamed-agent"})

	ck.Eq("renamed-agent", c.renameForm.input.Value(), "rename field after paste")
	ck.Eq("", c.ta.Value(), "the paste leaked into the prompt box behind the modal")
}

// A modal with no text field owns the paste the way it owns every key: the text
// must not land in the prompt box hidden behind it.
func TestPasteInAModalWithoutATextFieldIsSwallowed(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)
	// `rafiki create` opens the cockpit with the form up and the pane focus
	// still on the input, which is the case where a stray paste used to land in
	// the prompt box instead of being dropped.
	c.focus = focusInput
	c.form.focus = fieldName
	c.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	ck.Require().NotNil(c.query, "^S did not open the sort band")

	c.Update(tea.PasteMsg{Content: "should not land anywhere"})

	ck.Eq("", c.ta.Value(), "the paste leaked into the prompt box behind the sort band")
	ck.Eq("", c.form.inputs[fieldName].Value(), "the paste leaked into the form behind the sort band")
	ck.NotNil(c.query, "the sort band was dismissed by a paste")
}

// With no modal up the paste keeps its ordinary path: folding, image paths and
// the token dance all live in handlePaste.
func TestPasteWithNoModalStillReachesThePrompt(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestCockpit("")
	c.focus = focusInput

	c.Update(tea.PasteMsg{Content: "one\ntwo"})

	ck.Eq("one\ntwo", c.ta.Value(), "prompt box after paste")
}
