// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestSetBudgetPassesFieldsThrough(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	resp, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{
		ChildId: "c_target",
		MaxCost: 12.50,
	}))
	if err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
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
	if err != nil {
		t.Fatalf("SetBudget(0): %v", err)
	}
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
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestSetBudgetWithoutLifecycleFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable", connect.CodeOf(err))
	}
}

func TestSetBudgetNegativeBecomesInvalidArgument(t *testing.T) {
	f := &fakeLifecycle{}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	for _, badCost := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: badCost}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("badCost %v: code = %v, want InvalidArgument", badCost, connect.CodeOf(err))
		}
	}
}

func TestSetBudgetNotFoundBecomesNotFound(t *testing.T) {
	authoredMsg := "agent c_x is not registered"
	f := &fakeLifecycle{budgetErr: &control.ControllerError{Code: protocol.ErrNotFound, Message: authoredMsg}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), authoredMsg) {
		t.Fatalf("err.Error() = %q, want containing authored message %q", err.Error(), authoredMsg)
	}
}

func TestSetBudgetInvalidArgumentFromLifecycleBecomesInvalidArgument(t *testing.T) {
	authoredMsg := "limit exceeded or invalid"
	f := &fakeLifecycle{budgetErr: &control.ControllerError{Code: protocol.ErrInvalidArgs, Message: authoredMsg}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), authoredMsg) {
		t.Fatalf("err.Error() = %q, want containing authored message %q", err.Error(), authoredMsg)
	}
}

func TestSetBudgetErrorBecomesInternal(t *testing.T) {
	f := &fakeLifecycle{budgetErr: &control.ControllerError{Code: protocol.ErrInternal, Message: "boom"}}
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(f)

	_, err := s.SetBudget(context.Background(), connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_x", MaxCost: 5}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal", connect.CodeOf(err))
	}
}
