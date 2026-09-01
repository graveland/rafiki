// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"
)

// TaskLister is the narrow slice of the daemon's Controller this package needs
// to answer ListTasks. It mirrors ChildLister: the Controller already serves
// this shape for the ctrl_task_list frame verb, so there is one implementation
// behind both faces rather than two that can disagree.
type TaskLister interface {
	TaskList(ctx context.Context, req protocol.TaskListRequest) ([]tasks.Task, error)
}

// SetTaskLister attaches the ledger source. Post-construction setter for the
// same reason as SetChildLister: the Controller is built after this Server.
func (s *Server) SetTaskLister(l TaskLister) { s.taskLister.Store(&l) }

// taskListMaxRows mirrors pkg/control/dispatch.go's clamp.
//
// ListTasksRequest has no limit field and tasks.ListFilter.Limit == 0 means
// UNLIMITED, so without this one call with an empty conversation_id -- which
// means every conversation -- materialises the whole ledger into memory and
// onto the wire, past protocol.MaxFrameBytes' worth of rows. The frame verb
// has carried this clamp all along; the Connect path shipped without it.
const taskListMaxRows = 2000

// ListTasks answers the task ledger for one conversation.
//
// A Server with no lister attached answers an EMPTY list: that is a wiring
// state, not a runtime one, and it keeps the zero value usable in tests.
// A STORE error is returned AS an error -- rafiki requires a database, so a
// ledger that cannot answer is a real failure and must not be disguised as an
// empty list. The cockpit chooses to hide the box rather than surface it,
// which is the caller's decision and not this handler's.
func (s *Server) ListTasks(
	ctx context.Context, req *connect.Request[rafikiv1.ListTasksRequest],
) (*connect.Response[rafikiv1.ListTasksResponse], error) {
	out := &rafikiv1.ListTasksResponse{}
	lp := s.taskLister.Load()
	if lp == nil || *lp == nil {
		return connect.NewResponse(out), nil
	}
	rows, err := (*lp).TaskList(ctx, protocol.TaskListRequest{
		ConversationID: req.Msg.GetConversationId(),
		All:            req.Msg.GetIncludeDropped(),
		Limit:          taskListMaxRows,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, t := range rows {
		out.Tasks = append(out.Tasks, &rafikiv1.TaskRow{
			Handle:     t.Handle,
			Content:    t.Content,
			ActiveForm: t.ActiveForm,
			Status:     string(t.Status),
			Assignee:   t.Assignee,
			DropReason: t.DropReason,
		})
	}
	return connect.NewResponse(out), nil
}
