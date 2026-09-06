// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

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
