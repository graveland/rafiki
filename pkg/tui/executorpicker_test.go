// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestExecutorPickerSelectedReturnsMachineWhenPresent(t *testing.T) {
	all := []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
	}
	p := newExecutorPicker("claude", all, true, "")
	if got := p.selected(); got != "greyshift" {
		t.Fatalf("want greyshift, got %q", got)
	}
}

func TestExecutorPickerSelectedFallsBackToIDWhenNoMachineLabel(t *testing.T) {
	all := []*rafikiv1.ExecutorRow{
		{Id: "sess-01ABC", Eligible: true},
	}
	p := newExecutorPicker("claude", all, true, "")
	if got := p.selected(); got != "sess-01ABC" {
		t.Fatalf("want sess-01ABC, got %q", got)
	}
}

func TestExecutorPickerSelectedEmptyWhenNoRows(t *testing.T) {
	p := newExecutorPicker("claude", nil, true, "")
	if got := p.selected(); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestExecutorPickerMoveClampsToBounds(t *testing.T) {
	all := []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "a"},
		{Id: "exec-2", Machine: "b"},
	}
	p := newExecutorPicker("claude", all, true, "")
	p.move(-5, 10)
	if p.cursor != 0 {
		t.Fatalf("want clamped to 0, got %d", p.cursor)
	}
	p.move(+5, 10)
	if p.cursor != 1 {
		t.Fatalf("want clamped to 1, got %d", p.cursor)
	}
}

// The commit-back behavior handleExecutorPickerKey must reproduce: the picked
// ref lands in the form's executor field, the picker is dismissed, and focus
// advances past the row it just filled. This drives the pieces directly because
// the real key path needs the full Update plumbing (see handlePickerKey's own
// tests for the same cut).
func TestExecutorPickerCommitSetsFormFieldAndAdvancesFocus(t *testing.T) {
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
	if c.form.inputs[fieldExecutor].Value() != "greyshift" {
		t.Fatalf("want greyshift in the executor field, got %q", c.form.inputs[fieldExecutor].Value())
	}
	if c.execPicker != nil {
		t.Fatal("picker should be dismissed")
	}
	if c.form.focus == before {
		t.Fatal("focus should have advanced")
	}
}

// ctrl+e must open the picker through the REAL key path, not just exist as a
// case in a switch: bubbletea spells keys in ways a case can silently miss
// ("space" vs " " shipped exactly that way once).
func TestCtrlEOpensTheExecutorPickerFromTheForm(t *testing.T) {
	c := formCockpit(t)

	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	if c.execPicker == nil {
		t.Fatal("^E did not open the executor picker")
	}
	if c.execPicker.kind != c.form.kind() {
		t.Errorf("picker kind = %q, want the form's current kind %q", c.execPicker.kind, c.form.kind())
	}
	if !c.executorsBusy[c.form.kind()] {
		t.Error("no fetch was issued for the picker's kind")
	}
}

// The picker stacks OVER the form: esc returns to it rather than dismissing
// both, because the other fields are still half filled in.
func TestExecutorPickerEscReturnsToTheForm(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	c.execPicker.rows = []*rafikiv1.ExecutorRow{{Id: "exec-1", Machine: "greyshift", Eligible: true}}

	c.handleKey(keyMsg("esc"))

	if c.execPicker != nil {
		t.Fatal("esc did not close the picker")
	}
	if c.form == nil {
		t.Fatal("esc dismissed the form under the picker; it should have returned to it")
	}
}

// enter commits the highlighted ref into the executor field and advances past
// the row it just filled, through the real handleExecutorPickerKey path.
func TestExecutorPickerEnterCommitsThroughTheKeyPath(t *testing.T) {
	c := formCockpit(t)
	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	c.applyExecutorsLoaded(executorsLoadedMsg{kind: c.form.kind(), rows: []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
		{Id: "exec-2", Machine: "otherbox", Eligible: false},
	}})
	c.execPicker.move(+1, 10) // highlight the second row
	before := c.form.focus

	c.handleKey(keyMsg("enter"))

	if c.execPicker != nil {
		t.Fatal("enter did not close the picker")
	}
	if got := c.form.inputs[fieldExecutor].Value(); got != "otherbox" {
		t.Fatalf("executor field = %q, want otherbox (the highlighted row)", got)
	}
	if c.form.focus == before {
		t.Fatal("focus did not advance past the executor row")
	}
}

// An ineligible row is still offered, with its reason on the detail line: the
// reason is what tells you why the executor you want reads ✗.
func TestExecutorPickerShowsTheIneligibleReason(t *testing.T) {
	p := newExecutorPicker("claude", []*rafikiv1.ExecutorRow{
		{Id: "exec-1", Machine: "greyshift", Eligible: true},
		{Id: "exec-2", Machine: "oldbox", Eligible: false, Reason: "does not support launching \"claude\""},
	}, true, "")
	p.move(+1, 10) // the reason renders for the HIGHLIGHTED row
	out := ansi.Strip(p.view(80, 20))
	if !strings.Contains(out, "oldbox") {
		t.Error("the ineligible row was dropped from the list")
	}
	if !strings.Contains(out, `does not support launching "claude"`) {
		t.Error("the highlighted row's reason is not shown")
	}
}

// A modal takes the whole panel, and the executor picker is a modal.
func TestExecutorPickerOwnsTheBodyPane(t *testing.T) {
	c := railWith(t, "c_1", "c_2")
	c.handleKey(keyMsg("n"))
	c.width, c.height, c.ready = 100, 30, true

	c.handleKey(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	if c.railCols() != 0 {
		t.Error("the rail is still drawn behind the executor picker")
	}
	out := ansi.Strip(c.View().Content)
	if !strings.Contains(out, "executor") {
		t.Error("the executor picker did not render")
	}
	if strings.Contains(out, "c_2") {
		t.Error("a rail row rendered behind the modal")
	}
}

// The form's hint line names ^E: a binding nobody can discover is a binding
// that does not exist.
func TestFormHintsNameTheExecutorPicker(t *testing.T) {
	c := formCockpit(t)
	out := ansi.Strip(c.form.view(90, 24, c.modelView, nil, nil))
	if !strings.Contains(out, "^E executor") {
		t.Errorf("the hint line does not name ^E:\n%s", out)
	}
}
