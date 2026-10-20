// SPDX-License-Identifier: Apache-2.0

package eventlog_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TasksChanged is a ledger mutation a resumable consumer must not miss, so it
// is durable and carries the wire name of its oneof field. Both halves are
// pinned together: TierOf classifies through TypeName, so a dropped
// durableTypes entry makes a task change invisible to every replay.
func TestTasksChangedIsDurableAndNamed(t *testing.T) {
	c := assert.NewCollecting(t)
	ev := &rafikiv1.Event{ChildId: "c_owner", Payload: &rafikiv1.Event_TasksChanged{
		TasksChanged: &rafikiv1.TasksChanged{Kind: "added", Tasks: []*rafikiv1.TaskChange{
			{Handle: "1", Content: "write the store", Status: "pending"},
			{Handle: "2", Content: "wire the event", Status: "pending"},
		}},
	}}

	c.Eq("tasks_changed", eventlog.TypeName(ev), "TypeName(TasksChanged)")
	c.Eq(eventlog.TierDurable, eventlog.TierOf(ev), "TierOf(TasksChanged)")
}
