// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"
	"math"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// SetBudget changes a child's MaxCost: with operator authority for a user
// credential, with agent_set_budget's rule for a per-child credential. See
// SetBudgetRequest's comment in control.proto.
//
// Errors go through ConnectErr: the code the daemon attached at the source IS
// the classification, so the negative-cap rejection in cmd/rafikid/limits.go
// (ErrInvalidArgs) reads as InvalidArgument and the authored message rides
// along. An error that is NOT a ControllerError is infrastructure text and is
// redacted by ConnectErr; its cause is logged here so it is not lost.
func (s *Server) SetBudget(
	ctx context.Context,
	req *connect.Request[rafikiv1.SetBudgetRequest],
) (*connect.Response[rafikiv1.SetBudgetResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	maxCost := req.Msg.GetMaxCost()
	if maxCost < 0 || math.IsNaN(maxCost) || math.IsInf(maxCost, 0) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("max_cost cannot be negative, NaN, or infinite"))
	}
	// childScoped: the subtree boundary runs first; the lifecycle then
	// applies the child rule (direct parentage, remaining grant).
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
	if err := (*p).SetBudget(ctx, childID, maxCost); err != nil {
		var ce *ControllerError
		if !errors.As(err, &ce) {
			// ConnectErr redacts this below; log the cause here or lose it.
			slog.Error("connect: set_budget failed", "child_id", childID, "error", err)
		}
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.SetBudgetResponse{
		ChildId: childID,
		MaxCost: maxCost,
	}), nil
}
