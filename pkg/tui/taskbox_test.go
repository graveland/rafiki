// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func row(handle, content, status string) *rafikiv1.TaskRow {
	return &rafikiv1.TaskRow{Handle: handle, Content: content, Status: status}
}

// The box is a readout that costs transcript height, so it appears only when
// there is live work to report.
func TestTaskBoxHiddenWithNoLiveTasks(t *testing.T) {
	if got := renderTaskBox(nil, 40); len(got) != 0 {
		t.Errorf("box rendered with no tasks: %v", got)
	}
	done := []*rafikiv1.TaskRow{
		row("1", "done", "completed"),
		row("2", "also done", "failed"),
		row("3", "abandoned", "dropped"),
	}
	if got := renderTaskBox(done, 40); len(got) != 0 {
		t.Errorf("box rendered with only terminal tasks: %v", got)
	}
}

func TestTaskBoxShowsLiveWork(t *testing.T) {
	rows := []*rafikiv1.TaskRow{
		row("1", "read the design doc", "completed"),
		row("2", "wire the cost rollup", "in_progress"),
		row("3", "add the task pane", "pending"),
		row("4", "needs migration", "blocked"),
	}
	out := strings.Join(renderTaskBox(rows, 40), "\n")
	if !strings.Contains(out, "wire the cost rollup") {
		t.Errorf("in-progress task missing:\n%s", out)
	}
	if !strings.Contains(out, "⊘") {
		t.Errorf("blocked task not marked with ⊘:\n%s", out)
	}
}

// Capped, with the remainder named. An agent with forty tasks must not take
// the whole screen.
func TestTaskBoxCapsRowsAndNamesTheRemainder(t *testing.T) {
	var rows []*rafikiv1.TaskRow
	for i := 1; i <= 12; i++ {
		rows = append(rows, row(itoa(int64(i)), "task", "pending"))
	}
	got := renderTaskBox(rows, 40)
	if len(got) > taskBoxMaxRows+3 {
		t.Errorf("box is %d lines, cap is %d rows plus a border and a more-line",
			len(got), taskBoxMaxRows)
	}
	if !strings.Contains(strings.Join(got, "\n"), "more") {
		t.Errorf("the elided remainder is not named:\n%s", strings.Join(got, "\n"))
	}
}

// Rows must never exceed the pane. clip counts display columns, not runes.
func TestTaskBoxRespectsWidth(t *testing.T) {
	rows := []*rafikiv1.TaskRow{
		row("1", strings.Repeat("very long task content ", 20), "in_progress"),
	}
	for _, line := range renderTaskBox(rows, 30) {
		if w := ansi.StringWidth(line); w > 30 {
			t.Errorf("line is %d columns, budget is 30: %q", w, line)
		}
	}
}
