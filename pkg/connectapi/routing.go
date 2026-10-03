// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// SetRouting merges a routing-spec delta over a child's stored routing spec,
// effective on its next OpenRouter request. See SetRoutingRequest's comment in
// control.proto for who may set what.
//
// Errors go through ConnectErr, so the code the daemon attached at the source
// IS the classification: a parse failure (ErrInvalidArgs) reads as
// InvalidArgument, an unknown child (ErrNotFound) as NotFound, and a child's
// attempt to set or change only= (ErrPermissionDenied) as PermissionDenied. An
// error that is NOT a ControllerError is infrastructure text and is redacted by
// ConnectErr; its cause is logged here so it is not lost.
func (s *Server) SetRouting(
	ctx context.Context,
	req *connect.Request[rafikiv1.SetRoutingRequest],
) (*connect.Response[rafikiv1.SetRoutingResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	delta := req.Msg.GetDelta()
	if delta == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("delta is required"))
	}
	// childScoped: the subtree boundary runs first; the lifecycle then applies
	// the child rule (prefer/sort/quant only, never only=).
	if sc := s.childScope(ctx); sc != nil {
		if err := sc.Authorize(childID); err != nil {
			return nil, err
		}
	}
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}
	routing, err := (*p).SetRouting(ctx, childID, delta)
	if err != nil {
		var ce *ControllerError
		if !errors.As(err, &ce) {
			// ConnectErr redacts this below; log the cause here or lose it.
			slog.Error("connect: set_routing failed", "child_id", childID, "error", err)
		}
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.SetRoutingResponse{
		ChildId: childID,
		Routing: routing,
	}), nil
}
