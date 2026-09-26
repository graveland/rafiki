// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"math"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// SetBudget changes a child's MaxCost with operator authority. See
// SetBudgetRequest's comment in control.proto for what that means relative
// to the agent-facing agent_set_budget tool.
//
// Errors go through ConnectErr: the code the daemon attached at the source IS
// the classification, so the negative-cap rejection in cmd/rafikid/limits.go
// (ErrInvalidArgs) reads as InvalidArgument and the authored message rides
// along.
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
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}
	if err := (*p).SetBudget(ctx, childID, maxCost); err != nil {
		return nil, ConnectErr(err)
	}
	return connect.NewResponse(&rafikiv1.SetBudgetResponse{
		ChildId: childID,
		MaxCost: maxCost,
	}), nil
}
