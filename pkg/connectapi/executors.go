// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	// ConnectedAt is the current connection's join time; the zero time means
	// not connected. LastSeen is the row's last pool sighting; the zero time
	// means never seen. Both pass through whatever the source set — a row built
	// without them carries the zero time.
	ConnectedAt time.Time
	LastSeen    time.Time
	LaunchKinds []string
	Eligible    bool
	Reason      string
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
//
// kind selects WHICH listing this is. A non-empty kind asks the kind-scoped
// question — "which executors could serve a spawn of this kind right now" —
// and goes to the ExecutorLister, whose rows are live executors with
// eligibility evaluated exactly as chooseExecutor vs chooseLaunchExecutor
// would. An EMPTY kind is the management listing: the executor admin's plain rows over the durable table merged with
// the live pool, selector and default-limit semantics included, with
// eligibility UNEVALUATED — eligible/reason unset, per ListExecutorsRequest's
// comment in control.proto.
//
// Both paths return errors through executorAdminErr, which logs an uncoded
// cause before ConnectErr redacts it: the kind path's lister errors are not
// all the daemon's own (ListExecutorRows can surface raw store text, which
// names the database host, user and database on a connection failure), so
// wrapping one as-is would leak it to the peer.
func (s *Server) ListExecutors(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListExecutorsRequest],
) (*connect.Response[rafikiv1.ListExecutorsResponse], error) {
	if req.Msg.GetKind() == "" {
		p := s.execAdmin.Load()
		if p == nil {
			return nil, connect.NewError(connect.CodeUnavailable,
				errors.New("executor admin not yet wired"))
		}
		rows, err := (*p).List(ctx, req.Msg.GetSelector(), req.Msg.GetLimit())
		if err != nil {
			return nil, executorAdminErr("list_executors", err)
		}
		out := make([]*rafikiv1.ExecutorRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, toProtoExecutor(r))
		}
		return connect.NewResponse(&rafikiv1.ListExecutorsResponse{Rows: out}), nil
	}
	p := s.execLister.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("executor lister not yet wired"))
	}
	rows, err := (*p).ListExecutors(ctx, req.Msg.GetKind())
	if err != nil {
		return nil, executorAdminErr("list_executors", err)
	}
	out := make([]*rafikiv1.ExecutorRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoExecutor(r))
	}
	return connect.NewResponse(&rafikiv1.ListExecutorsResponse{Rows: out}), nil
}

func toProtoExecutor(r ExecutorRow) *rafikiv1.ExecutorRow {
	out := &rafikiv1.ExecutorRow{
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
	if !r.ConnectedAt.IsZero() {
		out.ConnectedAt = timestamppb.New(r.ConnectedAt)
	}
	if !r.LastSeen.IsZero() {
		out.LastSeen = timestamppb.New(r.LastSeen)
	}
	return out
}
