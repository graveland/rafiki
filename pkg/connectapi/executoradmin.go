// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ExecutorAdmin is the operator-side slice of the daemon behind the framed
// ctrl_executor_enroll / create / label / disable / enable / delete verbs —
// the management half of the executor plane, as distinct from the read-only
// listing ExecutorLister already serves.
type ExecutorAdmin interface {
	Enroll(ctx context.Context, req *rafikiv1.EnrollExecutorRequest) (*rafikiv1.EnrollExecutorResponse, error)
	Create(ctx context.Context, req *rafikiv1.CreateExecutorRequest) (*rafikiv1.CreateExecutorResponse, error)
	Label(ctx context.Context, req *rafikiv1.LabelExecutorRequest) (ExecutorRow, error)
	Disable(ctx context.Context, executorID string) error
	Enable(ctx context.Context, executorID string) error
	Delete(ctx context.Context, executorID string) error
	// List is the plain listing behind ListExecutors with an empty kind.
	List(ctx context.Context, selector string, limit int32) ([]ExecutorRow, error)
}

// SetExecutorAdmin attaches the executor-admin backend. Post-construction
// setter for the same reason as SetSkillManager: the Controller is built
// after this Server. A nil backend is refused rather than stored, the same
// rule as SetSkillManager: storing &a for a nil interface would defeat the
// handler's Unavailable path and nil-panic the first handler call.
func (s *Server) SetExecutorAdmin(a ExecutorAdmin) {
	if a == nil {
		return
	}
	s.execAdmin.Store(&a)
}

// EnrollExecutor serves the framed ctrl_executor_enroll face: mint a one-time
// enrollment token.
func (s *Server) EnrollExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.EnrollExecutorRequest],
) (*connect.Response[rafikiv1.EnrollExecutorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("EnrollExecutor: not yet implemented"))
}

// CreateExecutor serves the framed ctrl_executor_create face: mint an
// executor row and its durable credential in one step.
func (s *Server) CreateExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.CreateExecutorRequest],
) (*connect.Response[rafikiv1.CreateExecutorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("CreateExecutor: not yet implemented"))
}

// LabelExecutor serves the framed ctrl_executor_label face.
func (s *Server) LabelExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.LabelExecutorRequest],
) (*connect.Response[rafikiv1.LabelExecutorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("LabelExecutor: not yet implemented"))
}

// DisableExecutor serves the framed ctrl_executor_disable face.
func (s *Server) DisableExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.DisableExecutorRequest],
) (*connect.Response[rafikiv1.DisableExecutorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("DisableExecutor: not yet implemented"))
}

// EnableExecutor serves the framed ctrl_executor_enable face.
func (s *Server) EnableExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.EnableExecutorRequest],
) (*connect.Response[rafikiv1.EnableExecutorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("EnableExecutor: not yet implemented"))
}

// DeleteExecutor serves the framed ctrl_executor_delete face.
func (s *Server) DeleteExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.DeleteExecutorRequest],
) (*connect.Response[rafikiv1.DeleteExecutorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("DeleteExecutor: not yet implemented"))
}
