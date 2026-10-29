// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelalias"
)

// ErrModelAliasAuthoring marks a PutModelAlias/DeleteModelAlias refused because
// the caller may read model aliases but not write them (a child credential —
// no child, and no MCP tool, may author an alias). modelAliasError maps it to
// CodePermissionDenied.
var ErrModelAliasAuthoring = errors.New("model alias authoring refused")

// ModelAliasManager is the daemon's model-alias backend. The owner is resolved
// INSIDE the implementation from the request context — no method takes one, the
// same rule PresetManager follows: a user credential reaches its own bucket, the
// anonymous unix-socket caller shares the daemon's unattributed bucket.
type ModelAliasManager interface {
	// ListModelAliases lists the caller's aliases, ordered by name.
	ListModelAliases(ctx context.Context) ([]modelalias.Alias, error)
	// PutModelAlias validates and stores name → target.
	// Validation failures must be wrapped with modelalias.ErrInvalid (mapped to
	// CodeInvalidArgument); store errors must not be.
	PutModelAlias(ctx context.Context, name, target string) (modelalias.Alias, error)
	// DeleteModelAlias removes name's alias, or returns modelalias.ErrNotFound.
	DeleteModelAlias(ctx context.Context, name string) error
}

// SetModelAliasManager attaches the model-alias backend. A nil manager is
// refused rather than stored (see SetPresetManager): storing &m for a nil
// interface would defeat modelAliasManager's Unavailable path and nil-panic the
// first handler call instead.
func (s *Server) SetModelAliasManager(m ModelAliasManager) {
	if m == nil {
		return
	}
	s.modelAliases.Store(&m)
}

func (s *Server) modelAliasManager() (ModelAliasManager, error) {
	m := s.modelAliases.Load()
	if m == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("model aliases backend not yet wired"))
	}
	return *m, nil
}

// modelAliasError maps a manager failure onto a Connect code: an unknown alias
// is CodeNotFound, a name or target that failed validation is a
// CodeInvalidArgument, an authoring refusal by a child credential is
// CodePermissionDenied, and everything else — a store failure — is
// CodeInternal rather than something that reads as the caller's fault.
func modelAliasError(err error) error {
	switch {
	case errors.Is(err, modelalias.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, modelalias.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrModelAliasAuthoring):
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	return unmappedErr(err, "mapper", "modelAliasError")
}

func (s *Server) ListModelAliases(
	ctx context.Context, _ *connect.Request[rafikiv1.ListModelAliasesRequest],
) (*connect.Response[rafikiv1.ListModelAliasesResponse], error) {
	m, err := s.modelAliasManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListModelAliases(ctx)
	if err != nil {
		return nil, modelAliasError(err)
	}
	return connect.NewResponse(&rafikiv1.ListModelAliasesResponse{Rows: modelAliasRows(rows)}), nil
}

func (s *Server) PutModelAlias(
	ctx context.Context, req *connect.Request[rafikiv1.PutModelAliasRequest],
) (*connect.Response[rafikiv1.PutModelAliasResponse], error) {
	m, err := s.modelAliasManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if req.Msg.GetTarget() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("target is required"))
	}
	a, err := m.PutModelAlias(ctx, req.Msg.GetName(), req.Msg.GetTarget())
	if err != nil {
		return nil, modelAliasError(err)
	}
	return connect.NewResponse(&rafikiv1.PutModelAliasResponse{Row: modelAliasRow(a)}), nil
}

func (s *Server) DeleteModelAlias(
	ctx context.Context, req *connect.Request[rafikiv1.DeleteModelAliasRequest],
) (*connect.Response[rafikiv1.DeleteModelAliasResponse], error) {
	m, err := s.modelAliasManager()
	if err != nil {
		return nil, err
	}
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if err := m.DeleteModelAlias(ctx, req.Msg.GetName()); err != nil {
		return nil, modelAliasError(err)
	}
	return connect.NewResponse(&rafikiv1.DeleteModelAliasResponse{}), nil
}

// modelAliasRows converts a manager's aliases for a repeated-ModelAliasRow
// response; never nil, so an empty list serialises as [].
func modelAliasRows(rows []modelalias.Alias) []*rafikiv1.ModelAliasRow {
	out := make([]*rafikiv1.ModelAliasRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, modelAliasRow(r))
	}
	return out
}

// modelAliasRow converts one alias to its wire form.
func modelAliasRow(a modelalias.Alias) *rafikiv1.ModelAliasRow {
	return &rafikiv1.ModelAliasRow{
		Name:      a.Name,
		Target:    a.Target,
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}
