// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
)

type fakeTaskLister struct {
	got  protocol.TaskListRequest
	rows []tasks.Task
	err  error
}

func (f *fakeTaskLister) TaskList(_ context.Context, r protocol.TaskListRequest) ([]tasks.Task, error) {
	f.got = r
	return f.rows, f.err
}

func TestListTasksMapsRowsOntoTheWire(t *testing.T) {
	f := &fakeTaskLister{rows: []tasks.Task{
		{Handle: "1", Content: "read the design", Status: tasks.StatusCompleted},
		{Handle: "2.1", Content: "wire the rollup", ActiveForm: "wiring the rollup",
			Status: tasks.StatusInProgress, Assignee: "c9"},
	}}
	s := NewServer(nil)
	s.SetTaskLister(f)

	resp, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{ConversationId: "conv-1"}))
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if f.got.ConversationID != "conv-1" {
		t.Errorf("conversation id not forwarded: %q", f.got.ConversationID)
	}
	rows := resp.Msg.GetTasks()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[1].GetHandle() != "2.1" || rows[1].GetAssignee() != "c9" {
		t.Errorf("row 1 mapped wrong: %+v", rows[1])
	}
	if rows[0].GetStatus() != string(tasks.StatusCompleted) {
		t.Errorf("status not carried: %q", rows[0].GetStatus())
	}
}

// No lister configured is not an error the cockpit should render as a failure:
// a daemon with no database has no ledger and the box simply stays hidden.
func TestListTasksWithNoListerIsEmpty(t *testing.T) {
	s := NewServer(nil)
	resp, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{}))
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(resp.Msg.GetTasks()) != 0 {
		t.Errorf("got rows from a server with no lister")
	}
}
