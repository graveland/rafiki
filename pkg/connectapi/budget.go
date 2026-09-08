// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"math"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// setBudgetCode maps a Controller.SetChildBudgetAsOperator error onto its
// Connect code, mirroring close.go's closeCode: the code the daemon attached
// at the source IS the classification. The one addition over closeCode's
// switch is ErrInvalidArgs -> CodeInvalidArgument, since limitError (the
// negative-cap rejection in cmd/rafikid/limits.go) returns that code.
func setBudgetCode(err error) connect.Code {
	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		return connect.CodeInternal
	}
	switch ce.Code {
	case protocol.ErrNotFound:
		return connect.CodeNotFound
	case protocol.ErrInvalidArgs:
		return connect.CodeInvalidArgument
	default:
		return connect.CodeInternal
	}
}

// SetBudget changes a child's MaxCost with operator authority. See
// SetBudgetRequest's comment in control.proto for what that means relative
// to the agent-facing agent_set_budget tool.
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
		return nil, connect.NewError(setBudgetCode(err), err)
	}
	return connect.NewResponse(&rafikiv1.SetBudgetResponse{
		ChildId: childID,
		MaxCost: maxCost,
	}), nil
}
