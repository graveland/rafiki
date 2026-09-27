// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ExecutorAdmin is the operator-side slice of the daemon behind the
// executor enroll / create / label / disable / enable / delete RPCs —
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

// executorAdminErr is the seam's one error exit, shared by every handler in
// this file and by both of ListExecutors' paths. Errors go through
// ConnectErr: the code the daemon attached at the source IS the
// classification, so translateExecutorErr's ControllerErrors — "enrollment
// token already consumed", the machine-name collision — keep their authored
// messages on this face too. An already-coded *connect.Error returns before
// any of that, untouched (the same early return mapChildOpsErr carries), so
// a coded error from the seam is neither logged nor re-wrapped Internal. An
// error that is neither is infrastructure text and is redacted by ConnectErr;
// its cause is logged here
// so it is not lost (the same discipline close.go and budget.go follow
// inline, shared because this seam has seven error exits and seven inline
// copies is seven ways for the log line to drift).
func executorAdminErr(op string, err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	var ce *ControllerError
	if !errors.As(err, &ce) {
		// ConnectErr redacts this below; log the cause here or lose it.
		slog.Error("connect: executor admin "+op+" failed", "error", err)
	}
	return ConnectErr(err)
}

// EnrollExecutor serves the EnrollExecutor RPC: mint a one-time
// enrollment token. ttl_seconds must be positive — the handler
// refuses a non-positive one rather than silently minting the Controller's
// 72h default, and this face serves the same verb.
func (s *Server) EnrollExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.EnrollExecutorRequest],
) (*connect.Response[rafikiv1.EnrollExecutorResponse], error) {
	if req.Msg.GetTtlSeconds() <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("ttl_seconds must be positive"))
	}
	p := s.execAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor admin not yet wired"))
	}
	resp, err := (*p).Enroll(ctx, req.Msg)
	if err != nil {
		return nil, executorAdminErr("enroll_executor", err)
	}
	return connect.NewResponse(resp), nil
}

// CreateExecutor serves the CreateExecutor RPC: mint an
// executor row and its durable credential in one step.
func (s *Server) CreateExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.CreateExecutorRequest],
) (*connect.Response[rafikiv1.CreateExecutorResponse], error) {
	p := s.execAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor admin not yet wired"))
	}
	resp, err := (*p).Create(ctx, req.Msg)
	if err != nil {
		return nil, executorAdminErr("create_executor", err)
	}
	return connect.NewResponse(resp), nil
}

// LabelExecutor serves the LabelExecutor RPC. executor_id and at
// least one of set/remove are required — the handler refuses an
// empty change rather than paying a store round trip to change nothing, and
// this face serves the same verb.
func (s *Server) LabelExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.LabelExecutorRequest],
) (*connect.Response[rafikiv1.LabelExecutorResponse], error) {
	if req.Msg.GetExecutorId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("executor_id is required"))
	}
	if len(req.Msg.GetSet()) == 0 && len(req.Msg.GetRemove()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("at least one of set or remove is required"))
	}
	p := s.execAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor admin not yet wired"))
	}
	row, err := (*p).Label(ctx, req.Msg)
	if err != nil {
		return nil, executorAdminErr("label_executor", err)
	}
	return connect.NewResponse(&rafikiv1.LabelExecutorResponse{Executor: toProtoExecutor(row)}), nil
}

// DisableExecutor serves the DisableExecutor RPC.
func (s *Server) DisableExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.DisableExecutorRequest],
) (*connect.Response[rafikiv1.DisableExecutorResponse], error) {
	if req.Msg.GetExecutorId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("executor_id is required"))
	}
	p := s.execAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor admin not yet wired"))
	}
	if err := (*p).Disable(ctx, req.Msg.GetExecutorId()); err != nil {
		return nil, executorAdminErr("disable_executor", err)
	}
	return connect.NewResponse(&rafikiv1.DisableExecutorResponse{}), nil
}

// EnableExecutor serves the EnableExecutor RPC.
func (s *Server) EnableExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.EnableExecutorRequest],
) (*connect.Response[rafikiv1.EnableExecutorResponse], error) {
	if req.Msg.GetExecutorId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("executor_id is required"))
	}
	p := s.execAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor admin not yet wired"))
	}
	if err := (*p).Enable(ctx, req.Msg.GetExecutorId()); err != nil {
		return nil, executorAdminErr("enable_executor", err)
	}
	return connect.NewResponse(&rafikiv1.EnableExecutorResponse{}), nil
}

// DeleteExecutor serves the DeleteExecutor RPC.
func (s *Server) DeleteExecutor(
	ctx context.Context,
	req *connect.Request[rafikiv1.DeleteExecutorRequest],
) (*connect.Response[rafikiv1.DeleteExecutorResponse], error) {
	if req.Msg.GetExecutorId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("executor_id is required"))
	}
	p := s.execAdmin.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor admin not yet wired"))
	}
	if err := (*p).Delete(ctx, req.Msg.GetExecutorId()); err != nil {
		return nil, executorAdminErr("delete_executor", err)
	}
	return connect.NewResponse(&rafikiv1.DeleteExecutorResponse{}), nil
}
