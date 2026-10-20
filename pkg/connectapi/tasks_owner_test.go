// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tasks"

	"github.com/multigres/testkit/assert"
)

// ownerTaskLister records the framed TaskListRequest ListTasks forwards, and
// how many times it was reached: a refused read must not consult the ledger at
// all, so the call count is as load-bearing as the fields.
type ownerTaskLister struct {
	got   protocol.TaskListRequest
	calls int
	rows  []tasks.Task
}

func (f *ownerTaskLister) TaskList(_ context.Context, r protocol.TaskListRequest) ([]tasks.Task, error) {
	f.got = r
	f.calls++
	return f.rows, nil
}

// ownerScope is a ChildScope with a fixed id and a fixed descendant set. Its
// Authorize refuses the caller ITSELF, as the daemon's scope does (a child is
// not a descendant of its own id) — which is what makes ListTasks' own-id arm
// necessary rather than redundant, and what this file pins.
type ownerScope struct {
	id          string
	descendants map[string]bool
}

func (s ownerScope) ChildID() string { return s.id }

func (s ownerScope) Authorize(target string) error {
	if s.descendants[target] {
		return nil
	}
	return errTestNotDescendant(target)
}

func (s ownerScope) Subtree([]string) []protocol.ChildSummary { return nil }

func (s ownerScope) ConversationInScope(string) bool { return false }

// ownerServer wires a Server with the recording lister, and with sc as the
// caller's child scope when it is non-nil (nil means an operator caller).
func ownerServer(sc connectapi.ChildScope) (*connectapi.Server, *ownerTaskLister) {
	lister := &ownerTaskLister{}
	s := connectapi.NewServer(nil)
	s.SetTaskLister(lister)
	if sc != nil {
		s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return sc })
	}
	return s, lister
}

// TestListTasksOwnerChildSelfAndDescendantAllowed pins both arms of the child
// caller's owner_child_id guard: the caller's OWN ledger is allowed by the
// ChildID comparison (Authorize refuses the caller itself, so the arm is not
// redundant), and a descendant's is allowed by Authorize. Deleting either arm
// refuses a read the caller is entitled to.
func TestListTasksOwnerChildSelfAndDescendantAllowed(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f := ownerServer(ownerScope{id: "c_caller", descendants: map[string]bool{"c_kid": true}})

	resp, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{OwnerChildId: "c_caller"}))
	c.Require().NoError(err, "the caller's own ledger is in scope")
	c.Eq(1, f.calls, "the own-id arm must reach the ledger")
	c.Eq("c_caller", f.got.OwnerChildID, "OwnerChildID forwarded")
	c.Eq("", f.got.ConversationID, "the handler must not resolve an owner id into a conversation id")
	c.Empty(resp.Msg.GetTasks(), "an empty ledger answers no rows")

	resp, err = s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{OwnerChildId: "c_kid"}))
	c.Require().NoError(err, "a descendant's ledger is in scope")
	c.Eq(2, f.calls, "the descendant arm must reach the ledger")
	c.Eq("c_kid", f.got.OwnerChildID, "OwnerChildID forwarded")
	c.Empty(resp.Msg.GetTasks(), "an empty ledger answers no rows")
}

// TestListTasksOwnerChildSiblingRefused pins the guard itself: another child's
// ledger is a PermissionDenied, never a scoped empty list (which reads exactly
// like "you have no tasks"), and the ledger is never consulted.
func TestListTasksOwnerChildSiblingRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f := ownerServer(ownerScope{id: "c_caller", descendants: map[string]bool{"c_kid": true}})

	resp, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{OwnerChildId: "c_sibling"}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "a sibling's ledger = %v, want", err)
	c.Nil(resp, "a refused ledger read produced a response")
	c.Eq(0, f.calls, "a refused read must not consult the ledger")
}

// TestListTasksOwnerAndConversationTogetherRefused pins the mutual exclusion
// for every caller: the two fields name two different ledgers, and the
// refusal runs before the caller's scope is even asked, so a child caller
// that happens to own the named child still gets InvalidArgument.
func TestListTasksOwnerAndConversationTogetherRefused(t *testing.T) {
	cases := []struct {
		name string
		sc   connectapi.ChildScope
	}{
		{"operator", nil},
		{"child naming its own id", ownerScope{id: "c_caller"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			s, f := ownerServer(tc.sc)
			resp, err := s.ListTasks(context.Background(), connect.NewRequest(&rafikiv1.ListTasksRequest{
				ConversationId: "conv-1", OwnerChildId: "c_caller",
			}))
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "both scopes set = %v, want", err)
			c.Nil(resp, "a refused request produced a response")
			c.Eq(0, f.calls, "a refused request must not consult the ledger")
		})
	}
}

// TestListTasksOwnerOperatorUnknownChildIsEmpty pins the operator path: the
// handler forwards the owner id untouched, and an unknown child (or one with
// no ledger yet) answers an empty list rather than an error — Controller.
// TaskList's lookup answers nil rows for it.
func TestListTasksOwnerOperatorUnknownChildIsEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f := ownerServer(nil)

	resp, err := s.ListTasks(context.Background(),
		connect.NewRequest(&rafikiv1.ListTasksRequest{OwnerChildId: "c_nobody"}))
	c.Require().NoError(err, "an unknown child is an empty ledger, not a failure")
	c.Eq(1, f.calls, "the operator path reaches the ledger")
	c.Eq("c_nobody", f.got.OwnerChildID, "OwnerChildID forwarded")
	c.Empty(resp.Msg.GetTasks(), "an unknown child answers no rows")
}

// TestListTasksOwnerChildCallerWithNeitherFieldStillRefused pins that the new
// arm widened nothing: a child caller naming no ledger at all is still
// refused, because that reads as "every conversation".
func TestListTasksOwnerChildCallerWithNeitherFieldStillRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	s, f := ownerServer(ownerScope{id: "c_caller", descendants: map[string]bool{"c_kid": true}})

	resp, err := s.ListTasks(context.Background(), connect.NewRequest(&rafikiv1.ListTasksRequest{}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "no scope named = %v, want", err)
	c.Nil(resp, "a refused request produced a response")
	c.Eq(0, f.calls, "a refused read must not consult the ledger")
}
