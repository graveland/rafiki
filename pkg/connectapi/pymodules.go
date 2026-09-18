// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// PymoduleRow is one saved pymodule as the manager returns it. Code is
// populated only by Get and Put.
type PymoduleRow struct {
	Version     int64
	Name        string
	Description string
	CreatedAt   string
	Code        string
}

// PymoduleManager is the narrow slice of the daemon needed to manage the
// caller's own pymodules. The owner is resolved INSIDE the implementation
// from the request context -- no method takes one, the same rule as
// tools.PyModuleStore. A zero owner (unix socket) means the shared
// unattributed bucket, which is what the caller's own children use.
type PymoduleManager interface {
	ListPymodules(ctx context.Context) ([]PymoduleRow, error)
	GetPymodule(ctx context.Context, name string) (PymoduleRow, error)
	PutPymodule(ctx context.Context, name, code, description string) (PymoduleRow, error)
	DeletePymodule(ctx context.Context, name string) error
}

// SetPymoduleManager attaches the pymodules backend. Post-construction
// setter, same reason as SetSkillManager: the Controller is built after
// this Server. A nil manager is refused rather than stored: storing &m for
// a nil interface would defeat pymoduleManager's Unavailable path and nil-
// panic the first handler call instead.
func (s *Server) SetPymoduleManager(m PymoduleManager) {
	if m == nil {
		return
	}
	s.pymodules.Store(&m)
}

func (s *Server) pymoduleManager() (PymoduleManager, error) {
	p := s.pymodules.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("pymodules backend not yet wired"))
	}
	return *p, nil
}

func pymoduleError(err error) error {
	if errors.Is(err, pymodules.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func toProtoPymodule(r PymoduleRow) *rafikiv1.PymoduleRow {
	return &rafikiv1.PymoduleRow{
		Version:     r.Version,
		Name:        r.Name,
		Description: r.Description,
		CreatedAt:   r.CreatedAt,
		Code:        r.Code,
	}
}

func (s *Server) ListPymodules(
	ctx context.Context, req *connect.Request[rafikiv1.ListPymodulesRequest],
) (*connect.Response[rafikiv1.ListPymodulesResponse], error) {
	m, err := s.pymoduleManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListPymodules(ctx)
	if err != nil {
		return nil, pymoduleError(err)
	}
	out := make([]*rafikiv1.PymoduleRow, 0, len(rows))
	for _, r := range rows {
		r.Code = "" // an inventory is not a document
		out = append(out, toProtoPymodule(r))
	}
	return connect.NewResponse(&rafikiv1.ListPymodulesResponse{Rows: out}), nil
}

func (s *Server) GetPymodule(
	ctx context.Context, req *connect.Request[rafikiv1.GetPymoduleRequest],
) (*connect.Response[rafikiv1.GetPymoduleResponse], error) {
	m, err := s.pymoduleManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if err := pymodules.ValidName(req.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	row, err := m.GetPymodule(ctx, req.Msg.GetName())
	if err != nil {
		return nil, pymoduleError(err)
	}
	return connect.NewResponse(&rafikiv1.GetPymoduleResponse{Row: toProtoPymodule(row)}), nil
}

func (s *Server) PutPymodule(
	ctx context.Context, req *connect.Request[rafikiv1.PutPymoduleRequest],
) (*connect.Response[rafikiv1.PutPymoduleResponse], error) {
	m, err := s.pymoduleManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if req.Msg.GetCode() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("code is required"))
	}
	if err := pymodules.ValidName(req.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	row, err := m.PutPymodule(ctx, req.Msg.GetName(), req.Msg.GetCode(), req.Msg.GetDescription())
	if err != nil {
		return nil, pymoduleError(err)
	}
	return connect.NewResponse(&rafikiv1.PutPymoduleResponse{Row: toProtoPymodule(row)}), nil
}

func (s *Server) DeletePymodule(
	ctx context.Context, req *connect.Request[rafikiv1.DeletePymoduleRequest],
) (*connect.Response[rafikiv1.DeletePymoduleResponse], error) {
	m, err := s.pymoduleManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if err := pymodules.ValidName(req.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := m.DeletePymodule(ctx, req.Msg.GetName()); err != nil {
		return nil, pymoduleError(err)
	}
	return connect.NewResponse(&rafikiv1.DeletePymoduleResponse{}), nil
}
