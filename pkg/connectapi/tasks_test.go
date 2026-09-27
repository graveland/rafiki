// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
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
			Status: tasks.StatusInProgress, Assignee: "c9", ConversationID: "conv-9"},
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

// TestTaskRowCarriesConversationID pins the framed row's conversation id on
// the Connect wire. The task row serialized tasks.Task
// untagged, so ConversationID rode every response, and `rafiki tasks`' CHILD
// column rendered it -- the Connect TaskRow dropped it and Task 4.1's CLI
// conversion lost the column as a result. It is a conversation id, not a
// child id (the child working the row is assignee), so it lands under its own
// name rather than a child-shaped one.
func TestTaskRowCarriesConversationID(t *testing.T) {
	f := &fakeTaskLister{rows: []tasks.Task{
		{Handle: "2.1", Content: "wire the rollup", Status: tasks.StatusInProgress,
			Assignee: "c9", ConversationID: "conv-9"},
	}}
	s := NewServer(nil)
	s.SetTaskLister(f)

	resp, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{}))
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	rows := resp.Msg.GetTasks()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := rows[0].GetConversationId(); got != "conv-9" {
		t.Errorf("ConversationId = %q, want conv-9", got)
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

// tasks.ListFilter.Limit == 0 means UNLIMITED, and conversation_id empty means
// every conversation -- so an unclamped call can materialise the whole ledger.
// The frame verb has clamped to 2000 all along.
func TestListTasksClampsRowCount(t *testing.T) {
	f := &fakeTaskLister{}
	s := NewServer(nil)
	s.SetTaskLister(f)

	if _, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{})); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if f.got.Limit != taskListMaxRows {
		t.Errorf("Limit = %d, want %d", f.got.Limit, taskListMaxRows)
	}
}

// TestListTasksForwardsChildIDStatusAndLimit pins the framed TaskListRequest
// parity fields: child_id maps to Assignee (Controller.TaskList's own
// mapping), status forwards verbatim, and an explicit limit under the
// ceiling survives instead of being replaced by taskListMaxRows.
func TestListTasksForwardsChildIDStatusAndLimit(t *testing.T) {
	f := &fakeTaskLister{}
	s := NewServer(nil)
	s.SetTaskLister(f)

	if _, err := s.ListTasks(context.Background(), connect.NewRequest(&rafikiv1.ListTasksRequest{
		ConversationId: "conv-1", ChildId: "c9", Status: "in_progress", Limit: 5,
	})); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if f.got.ChildID != "c9" {
		t.Errorf("ChildID = %q, want c9", f.got.ChildID)
	}
	if f.got.Status != "in_progress" {
		t.Errorf("Status = %q, want in_progress", f.got.Status)
	}
	if f.got.Limit != 5 {
		t.Errorf("Limit = %d, want 5 (an explicit under-ceiling limit must survive)", f.got.Limit)
	}
}

// TestListTasksOverLimitStillClamps mirrors TestListTasksClampsRowCount for an
// explicit, too-large wire limit: the ceiling applies to a caller-supplied
// value too, not only to the unset (zero) case.
func TestListTasksOverLimitStillClamps(t *testing.T) {
	f := &fakeTaskLister{}
	s := NewServer(nil)
	s.SetTaskLister(f)

	if _, err := s.ListTasks(context.Background(), connect.NewRequest(&rafikiv1.ListTasksRequest{
		Limit: taskListMaxRows + 500,
	})); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if f.got.Limit != taskListMaxRows {
		t.Errorf("Limit = %d, want %d (clamped)", f.got.Limit, taskListMaxRows)
	}
}

// TestListTasksAllOrIncludeDroppedBothMeanIncludeDropped pins ruling 3: the
// handler treats include_dropped OR all set as include-dropped, and both
// spellings stay on the wire (control.proto's comment on ListTasksRequest).
func TestListTasksAllOrIncludeDroppedBothMeanIncludeDropped(t *testing.T) {
	cases := []struct {
		name           string
		includeDropped bool
		all            bool
	}{
		{"neither", false, false},
		{"include_dropped only", true, false},
		{"all only", false, true},
		{"both", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTaskLister{}
			s := NewServer(nil)
			s.SetTaskLister(f)

			if _, err := s.ListTasks(context.Background(), connect.NewRequest(&rafikiv1.ListTasksRequest{
				IncludeDropped: tc.includeDropped, All: tc.all,
			})); err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			want := tc.includeDropped || tc.all
			if f.got.All != want {
				t.Errorf("All (include-dropped) = %v, want %v", f.got.All, want)
			}
		})
	}
}

// rafiki requires a database, so a ledger that cannot answer is a real
// failure. Disguising it as an empty list hides a broken daemon behind a
// cockpit that simply shows no tasks.
func TestListTasksSurfacesAStoreError(t *testing.T) {
	f := &fakeTaskLister{err: errors.New("task ledger unavailable")}
	s := NewServer(nil)
	s.SetTaskLister(f)

	if _, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{})); err == nil {
		t.Error("a store error must not be reported as an empty list")
	}
}
