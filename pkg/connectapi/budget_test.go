// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"math"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestSetBudgetPassesFieldsThrough(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{
		ChildId: "c_target",
		MaxCost: 12.50,
	}))
	assert.NewAborting(t).NoError(err, "SetBudget")
	if f.budgetChildID != "c_target" || f.budgetMaxCost != 12.50 {
		t.Fatalf("lifecycle got childID=%q maxCost=%v, want c_target/12.50", f.budgetChildID, f.budgetMaxCost)
	}
	if resp.Msg.GetChildId() != "c_target" || resp.Msg.GetMaxCost() != 12.50 {
		t.Fatalf("response = %+v, want echoed child_id/max_cost", resp.Msg)
	}
}

func TestSetBudgetZeroAccepted(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{
		ChildId: "c_target",
		MaxCost: 0,
	}))
	assert.NewAborting(t).NoError(err, "SetBudget(0)")
	if f.budgetChildID != "c_target" || f.budgetMaxCost != 0 {
		t.Fatalf("lifecycle got childID=%q maxCost=%v, want c_target/0", f.budgetChildID, f.budgetMaxCost)
	}
	if resp.Msg.GetChildId() != "c_target" || resp.Msg.GetMaxCost() != 0 {
		t.Fatalf("response = %+v, want echoed child_id/0", resp.Msg)
	}
}

func TestSetBudgetRequiresChildID(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{MaxCost: 5}))
	assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestSetBudgetWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestSetBudgetNegativeBecomesInvalidArgument(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	for _, badCost := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: badCost}))
		assert.NewAborting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "badCost %v: code = %v, want InvalidArgument", badCost, connect.CodeOf(err))
	}
}

func TestSetBudgetNotFoundBecomesNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	authoredMsg := "agent c_x is not registered"
	f := &fakeLifecycle{budgetErr: &connectapi.ControllerError{Code: protocol.ErrNotFound, Message: authoredMsg}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), authoredMsg, "err.Error()")
}

func TestSetBudgetInvalidArgumentFromLifecycleBecomesInvalidArgument(t *testing.T) {
	c := assert.NewAborting(t)
	authoredMsg := "limit exceeded or invalid"
	f := &fakeLifecycle{budgetErr: &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: authoredMsg}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), authoredMsg, "err.Error()")
}

func TestSetBudgetErrorBecomesInternal(t *testing.T) {
	f := &fakeLifecycle{budgetErr: &connectapi.ControllerError{Code: protocol.ErrInternal, Message: "boom"}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	assert.NewAborting(t).Eq(connect.CodeInternal, connect.CodeOf(err), "code")
}
