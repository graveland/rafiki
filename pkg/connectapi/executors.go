// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ExecutorRow is one live executor the daemon's pool knows about, plus
// whether it could serve a spawn of a given kind right now.
//
// This is a package-local domain type for the same reason ModelRow is one:
// cmd/rafiki links this package and must not be dragged into pkg/executors'
// pgx-adjacent dependency graph.
type ExecutorRow struct {
	ID            string
	Machine       string
	Labels        map[string]string
	Isolation     string
	WorkspaceMode string
	Roots         []string
	Admits        string
	Enabled       bool
	Connected     bool
	LaunchKinds   []string
	Eligible      bool
	Reason        string
}

// ExecutorLister is the narrow slice of the daemon's Controller needed to
// enumerate executors. kind scopes eligibility exactly as ListModels' kind
// scopes which model sources may answer.
type ExecutorLister interface {
	ListExecutors(ctx context.Context, kind string) ([]ExecutorRow, error)
}

// SetExecutorLister attaches the executor source. Post-construction setter
// for the same reason as SetModelLister: the Controller is built after this
// Server.
func (s *Server) SetExecutorLister(l ExecutorLister) { s.execLister.Store(&l) }

// ListExecutors enumerates the executors the daemon's pool currently knows
// about, for this caller.
func (s *Server) ListExecutors(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListExecutorsRequest],
) (*connect.Response[rafikiv1.ListExecutorsResponse], error) {
	p := s.execLister.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor lister not yet wired"))
	}
	rows, err := (*p).ListExecutors(ctx, req.Msg.GetKind())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*rafikiv1.ExecutorRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoExecutor(r))
	}
	return connect.NewResponse(&rafikiv1.ListExecutorsResponse{Rows: out}), nil
}

func toProtoExecutor(r ExecutorRow) *rafikiv1.ExecutorRow {
	return &rafikiv1.ExecutorRow{
		Id:            r.ID,
		Machine:       r.Machine,
		Labels:        r.Labels,
		Isolation:     r.Isolation,
		WorkspaceMode: r.WorkspaceMode,
		Roots:         r.Roots,
		Admits:        r.Admits,
		Enabled:       r.Enabled,
		Connected:     r.Connected,
		LaunchKinds:   r.LaunchKinds,
		Eligible:      r.Eligible,
		Reason:        r.Reason,
	}
}
