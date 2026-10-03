// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/routing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ErrRouteNotFound is what a RouteManager returns when Delete names a model
// line with no live routing-policy row — either it was never set, or its
// newest row is already a tombstone. Package-local, like ErrProviderNotBanned,
// so this interface doesn't force implementers onto routepolicy's sentinel
// (routepolicy links pgx and must never reach the client, which links this
// package).
var ErrRouteNotFound = errors.New("route has no live row for model line")

// RouteRow is one live routing-policy row: the model line it governs, the
// routing spec (routing.ParseSpec's grammar, e.g. "sort=price,quant=fp8+"),
// and when it was written.
type RouteRow struct {
	ModelLine string
	Spec      string
	CreatedAt time.Time
}

// RouteManager is the narrow slice of the daemon needed to list and manage the
// routing-policy rows. Implementations enforce who may write: a mutation from
// an identity that may not is refused with a connect PermissionDenied error,
// which the handlers pass through unchanged.
type RouteManager interface {
	// ListRoutes returns every live row, ordered by model line.
	ListRoutes(ctx context.Context) ([]RouteRow, error)
	// SetRoute appends a live row for line, superseding any previous one.
	// The spec is validated with routing.ParseSpec before the handler calls
	// this, and the store validates again.
	SetRoute(ctx context.Context, line, spec string) (RouteRow, error)
	// DeleteRoute appends the tombstone for line. ErrRouteNotFound when the
	// line has no live row.
	DeleteRoute(ctx context.Context, line string) error
}

// SetRouteManager attaches the route-policy backend. A nil manager is refused
// rather than stored, the same rule as SetProviderBanManager: storing &m for a
// nil interface would defeat routeManager's Unavailable path and nil-panic the
// first handler call.
func (s *Server) SetRouteManager(m RouteManager) {
	if m == nil {
		return
	}
	s.routes.Store(&m)
}

func (s *Server) routeManager() (RouteManager, error) {
	p := s.routes.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("routes backend not yet wired"))
	}
	return *p, nil
}

func routeError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, ErrRouteNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// toProtoRouteRow renders created_at as RFC3339 UTC: the wire field is a
// string, and a stable timestamp format is what a client re-parses.
func toProtoRouteRow(r RouteRow) *rafikiv1.RouteRow {
	return &rafikiv1.RouteRow{
		ModelLine: r.ModelLine,
		Spec:      r.Spec,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) ListRoutes(
	ctx context.Context, _ *connect.Request[rafikiv1.ListRoutesRequest],
) (*connect.Response[rafikiv1.ListRoutesResponse], error) {
	m, err := s.routeManager()
	if err != nil {
		return nil, err
	}
	rows, err := m.ListRoutes(ctx)
	if err != nil {
		return nil, routeError(err)
	}
	out := make([]*rafikiv1.RouteRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProtoRouteRow(r))
	}
	return connect.NewResponse(&rafikiv1.ListRoutesResponse{Rows: out}), nil
}

func (s *Server) SetRoute(
	ctx context.Context, req *connect.Request[rafikiv1.SetRouteRequest],
) (*connect.Response[rafikiv1.SetRouteResponse], error) {
	m, err := s.routeManager()
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(req.Msg.GetModelLine())
	if line == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model_line is required"))
	}
	// The empty spec is the zero spec ("no opinion"), the same value a policy
	// row stores to mean that; anything else must parse, and the refusal
	// carries the parser's message so the caller learns which item was wrong.
	spec := strings.TrimSpace(req.Msg.GetSpec())
	if _, err := routing.ParseSpec(spec); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	row, err := m.SetRoute(ctx, line, spec)
	if err != nil {
		return nil, routeError(err)
	}
	return connect.NewResponse(&rafikiv1.SetRouteResponse{Row: toProtoRouteRow(row)}), nil
}

func (s *Server) DeleteRoute(
	ctx context.Context, req *connect.Request[rafikiv1.DeleteRouteRequest],
) (*connect.Response[rafikiv1.DeleteRouteResponse], error) {
	m, err := s.routeManager()
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(req.Msg.GetModelLine())
	if line == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("model_line is required"))
	}
	if err := m.DeleteRoute(ctx, line); err != nil {
		return nil, routeError(err)
	}
	return connect.NewResponse(&rafikiv1.DeleteRouteResponse{}), nil
}

// RouteExplainer is the daemon-side backend behind ModelRoutes: it parses a
// model line's routing bracket, joins the model's hosting endpoints to the
// guard's bans/ejections and OpenRouter's measured stats, and predicts the
// try order. Deliberately one method and no scoping: the answer names
// providers, prices and stats, nothing owned by a user — the same non-owned
// surface ListRoutes and ModelInfo expose.
type RouteExplainer interface {
	ExplainModelRoutes(ctx context.Context, model string) (*rafikiv1.ModelRoutesResponse, error)
}

// SetRouteExplainer attaches the ModelRoutes backend. A nil explainer is
// refused rather than stored, the same rule as SetRouteManager: storing &m for
// a nil interface would defeat routeExplainer's Unavailable path and nil-panic
// the first handler call.
func (s *Server) SetRouteExplainer(m RouteExplainer) {
	if m == nil {
		return
	}
	s.routeExplainer.Store(&m)
}

func (s *Server) routeExplain() (RouteExplainer, error) {
	p := s.routeExplainer.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("route explainer not wired"))
	}
	return *p, nil
}

// ModelRoutes answers where a request for a model would go under a routing
// spec. The model argument carries the routing bracket; the explainer owns
// parsing it, so a malformed line surfaces through it as InvalidArgument (see
// modelRoutesError).
//
// The empty-model check runs BEFORE the unwired check so a blank request is
// refused as an argument error even on a daemon that has no explainer.
func (s *Server) ModelRoutes(
	ctx context.Context, req *connect.Request[rafikiv1.ModelRoutesRequest],
) (*connect.Response[rafikiv1.ModelRoutesResponse], error) {
	model := strings.TrimSpace(req.Msg.GetModel())
	if model == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("model is required"))
	}
	m, err := s.routeExplain()
	if err != nil {
		return nil, err
	}
	resp, err := m.ExplainModelRoutes(ctx, model)
	if err != nil {
		return nil, modelRoutesError(err, model)
	}
	return connect.NewResponse(resp), nil
}

// modelRoutesError classifies an explainer failure. A *ControllerError is the
// code the daemon attached at the source (the adapter classifies a malformed
// model line ErrInvalidArgs), so it goes through ConnectErr unchanged. A
// *connect.Error the explainer already shaped (the endpoint catalog being
// unavailable) is passed through as-is. Anything else is infrastructure text
// this package cannot author, so its cause is logged here and ConnectErr
// redacts it to a generic Internal.
func modelRoutesError(err error, model string) error {
	var ce *ControllerError
	if errors.As(err, &ce) {
		return ConnectErr(err)
	}
	var connErr *connect.Error
	if errors.As(err, &connErr) {
		return connErr
	}
	slog.Error("connect: model_routes failed", "model", model, "error", err)
	return ConnectErr(err)
}
