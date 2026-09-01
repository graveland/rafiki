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

// ListTasks answers the task ledger for one conversation.
//
// A daemon with no ledger answers an EMPTY list rather than an error: the
// cockpit hides the box when there is nothing to show, and an error there
// would render as a failure on every DB-less daemon.
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
