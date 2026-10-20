// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// A TasksChanged line names the kind and how many rows the change touched, so
// `rafiki tail` shows ledger movement without dumping every row's content.
func TestTasksChangedRendersKindAndCount(t *testing.T) {
	c := assert.NewCollecting(t)
	id := "c_owner"
	r := newEventRenderer()
	_ = r.observe(spawnedFor(id, "impl", ""), atClock(9, 0, 0.0))

	ev := withTS(&rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_TasksChanged{
		TasksChanged: &rafikiv1.TasksChanged{Kind: "added", Tasks: []*rafikiv1.TaskChange{
			{Handle: "1", Content: "write the store", ActiveForm: "writing the store", Status: "pending"},
			{Handle: "2", Content: "wire the event", ActiveForm: "wiring the event", Status: "pending"},
		}},
	}}, atClock(9, 0, 5.0))

	got := r.observe(ev, atClock(9, 0, 5.0))
	c.NotStrContains(got, "\n", "tasks_changed line wrapped")
	for _, want := range []string{"tasks_changed", "tasks added (2)"} {
		c.True(strings.Contains(got, want), "tasks_changed line = %q, want it to contain %q", got, want)
	}
}
