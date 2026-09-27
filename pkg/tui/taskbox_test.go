// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func row(handle, content, status string) *rafikiv1.TaskRow {
	return &rafikiv1.TaskRow{Handle: handle, Content: content, Status: status}
}

// The box is a readout that costs transcript height, so it appears only when
// there is live work to report.
func TestTaskBoxHiddenWithNoLiveTasks(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Empty(renderTaskBox(nil, 40), "box rendered with no tasks")
	done := []*rafikiv1.TaskRow{
		row("1", "done", "completed"),
		row("2", "also done", "failed"),
		row("3", "abandoned", "dropped"),
	}
	c.Empty(renderTaskBox(done, 40), "box rendered with only terminal tasks")
}

func TestTaskBoxShowsLiveWork(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []*rafikiv1.TaskRow{
		row("1", "read the design doc", "completed"),
		row("2", "wire the cost rollup", "in_progress"),
		row("3", "add the task pane", "pending"),
		row("4", "needs migration", "blocked"),
	}
	out := strings.Join(renderTaskBox(rows, 40), "\n")
	c.StrContains(out, "wire the cost rollup", "in-progress task missing:\n")
	c.StrContains(out, "⊘", "blocked task not marked with ⊘:\n")
}

// Capped, with the remainder named. An agent with forty tasks must not take
// the whole screen.
func TestTaskBoxCapsRowsAndNamesTheRemainder(t *testing.T) {
	c := assert.NewCollecting(t)
	var rows []*rafikiv1.TaskRow
	for i := 1; i <= 12; i++ {
		rows = append(rows, row(itoa(int64(i)), "task", "pending"))
	}
	got := renderTaskBox(rows, 40)
	c.LessOrEqual(taskBoxMaxRows+3, len(got), "box is %d lines, cap is %d rows plus a border and a more-line", len(got), taskBoxMaxRows)
	c.StrContains(strings.Join(got, "\n"), "more", "the elided remainder is not named:\n")
}

// Rows must never exceed the pane. clip counts display columns, not runes.
func TestTaskBoxRespectsWidth(t *testing.T) {
	rows := []*rafikiv1.TaskRow{
		row("1", strings.Repeat("very long task content ", 20), "in_progress"),
	}
	for _, line := range renderTaskBox(rows, 30) {
		w := ansi.StringWidth(line)
		assert.NewCollecting(t).LessOrEqual(30, w, "line is %d columns, budget is 30: %q", w, line)
	}
}

// View draws a divider ABOVE the box, so the box costs len(box)+1 screen rows.
// Subtracting only its own lines made the view one row too tall whenever the
// box was visible, pushing the footer off the bottom of the alt screen.
func TestTaskBoxRowsCountsItsDivider(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq(0, taskBoxRows(nil), "taskBoxRows(nil)")
	box := renderTaskBox([]*rafikiv1.TaskRow{row("1", "work", "in_progress")}, 40)
	c.Require().NotEmpty(box, "box did not render")
	got, want := taskBoxRows(box), len(box)+1
	c.Eq(want, got, "taskBoxRows")
}
