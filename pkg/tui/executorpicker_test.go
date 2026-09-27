// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestExecutorPickerSelectedReturnsMachineWhenPresent(t *testing.T) {
	all := []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
	}
	p := newExecutorPicker("claude", all, true, "")
	assert.NewAborting(t).Eq("greyshift", p.selected(), "want greyshift, got")
}

func TestExecutorPickerSelectedFallsBackToIDWhenNoMachineLabel(t *testing.T) {
	all := []*rafikiv1.ExecutorRow{
		{Id: "sess-01ABC", Eligible: true},
	}
	p := newExecutorPicker("claude", all, true, "")
	assert.NewAborting(t).Eq("sess-01ABC", p.selected(), "want sess-01ABC, got")
}

func TestExecutorPickerSelectedEmptyWhenNoRows(t *testing.T) {
	p := newExecutorPicker("claude", nil, true, "")
	assert.NewAborting(t).Eq("", p.selected(), "want empty, got")
}

func TestExecutorPickerMoveClampsToBounds(t *testing.T) {
	c := assert.NewAborting(t)
	all := []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "a"},
		{Id: "exec-2", Machine: "b"},
	}
	p := newExecutorPicker("claude", all, true, "")
	p.move(-5, 10)
	c.Eq(0, p.cursor, "want clamped to 0, got")
	p.move(+5, 10)
	c.Eq(1, p.cursor, "want clamped to 1, got")
}

// The commit-back behavior handleExecutorPickerKey must reproduce: the picked
// ref lands in the form's executor field, the picker is dismissed, and focus
// advances past the row it just filled. This drives the pieces directly because
// the real key path needs the full Update plumbing (see handlePickerKey's own
// tests for the same cut).
func TestExecutorPickerCommitSetsFormFieldAndAdvancesFocus(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Cockpit{form: newSpawnForm()}
	c.execPicker = newExecutorPicker("claude", []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
	}, true, "")
	before := c.form.focus
	if ref := c.execPicker.selected(); ref != "" {
		c.form.inputs[fieldExecutor].SetValue(ref)
	}
	c.execPicker = nil
	c.form.moveFocus(+1)
	ck.Eq("greyshift", c.form.inputs[fieldExecutor].Value(), "want greyshift in the executor field, got")
	ck.Nil(c.execPicker, "picker should be dismissed")
	ck.NotEq(before, c.form.focus, "focus should have advanced")
}

// ctrl+e must open the picker through the REAL key path, not just exist as a
// case in a switch: bubbletea spells keys in ways a case can silently miss
// ("space" vs " " shipped exactly that way once).
func TestCtrlEOpensTheExecutorPickerFromTheForm(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := formCockpit(t)

	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	ck.Require().NotNil(c.execPicker, "^E did not open the executor picker")
	ck.Eq(c.form.kind(), c.execPicker.kind, "picker kind")
	ck.False(!c.executorsBusy[c.form.kind()], "no fetch was issued for the picker's kind")
}

// The picker stacks OVER the form: esc returns to it rather than dismissing
// both, because the other fields are still half filled in.
func TestExecutorPickerEscReturnsToTheForm(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	c.execPicker.rows = []*rafikiv1.ExecutorRow{{Id: "exec-1", Machine: "greyshift", Eligible: true}}

	c.handleKey(keyMsg("esc"))

	ck.Nil(c.execPicker, "esc did not close the picker")
	ck.NotNil(c.form, "esc dismissed the form under the picker; it should have returned to it")
}

// enter commits the highlighted ref into the executor field and advances past
// the row it just filled, through the real handleExecutorPickerKey path.
func TestExecutorPickerEnterCommitsThroughTheKeyPath(t *testing.T) {
	ck := assert.NewAborting(t)
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	c.applyExecutorsLoaded(executorsLoadedMsg{kind: c.form.kind(), rows: []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
		{Id: "exec-2", Machine: "otherbox", Eligible: false},
	}})
	c.execPicker.move(+1, 10) // highlight the second row
	before := c.form.focus

	c.handleKey(keyMsg("enter"))

	ck.Nil(c.execPicker, "enter did not close the picker")
	ck.Eq("otherbox", c.form.inputs[fieldExecutor].Value(), "executor field")
	ck.NotEq(before, c.form.focus, "focus did not advance past the executor row")
}

// An ineligible row is still offered, with its reason on the detail line: the
// reason is what tells you why the executor you want reads ✗.
func TestExecutorPickerShowsTheIneligibleReason(t *testing.T) {
	c := assert.NewCollecting(t)
	p := newExecutorPicker("claude", []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
		{Id: "exec-2", Machine: "oldbox", Eligible: false, Reason: "does not support launching \"claude\""},
	}, true, "")
	p.move(+1, 10) // the reason renders for the HIGHLIGHTED row
	out := ansi.Strip(p.view(80, 20))
	c.StrContains(out, "oldbox", "the ineligible row was dropped from the list")
	c.StrContains(out, `does not support launching "claude"`, "the highlighted row's reason is not shown")
}

// A modal takes the whole panel, and the executor picker is a modal.
func TestExecutorPickerOwnsTheBodyPane(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := railWith(t, "c_1", "c_2")
	c.handleKey(keyMsg("n"))
	c.width, c.height, c.ready = 100, 30, true

	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	ck.Eq(0, c.railCols(), "the rail is still drawn behind the executor picker")
	out := ansi.Strip(c.View().Content)
	ck.StrContains(out, "executor", "the executor picker did not render")
	ck.NotStrContains(out, "c_2", "a rail row rendered behind the modal")
}

// The form's hint line names ^E: a binding nobody can discover is a binding
// that does not exist.
func TestFormHintsNameTheExecutorPicker(t *testing.T) {
	c := formCockpit(t)
	out := ansi.Strip(c.form.view(90, 24, c.modelView, nil, nil))
	assert.NewCollecting(t).StrContains(out, "^E executor", "the hint line does not name ^E:\n")
}
