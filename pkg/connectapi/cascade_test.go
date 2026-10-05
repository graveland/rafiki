// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// treeLifecycle records the order children are ended in and lets a test fail
// individual ids. It lists descendants like the daemon does: deepest first.
type treeLifecycle struct {
	fakeLifecycle
	descendants []string
	order       []string
	errFor      map[string]error
}

func (l *treeLifecycle) DescendantIDs(string) []string { return l.descendants }

func (l *treeLifecycle) Kill(_ context.Context, id string, _, _ time.Duration) (connectapi.KillOutcome, error) {
	l.order = append(l.order, "kill:"+id)
	return connectapi.KillOutcome{}, l.errFor[id]
}

func (l *treeLifecycle) Close(_ context.Context, id string) error {
	l.order = append(l.order, "close:"+id)
	return l.errFor[id]
}

func cascadeServer(l connectapi.ChildLifecycle) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetChildLifecycle(l)
	return s
}

func TestKillIncludeDescendantsEndsDeepestFirstThenParent(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_3", "c_2"}}
	resp, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(
		&rafikiv1.KillRequest{ChildId: "c_1", IncludeDescendants: true}))
	c.Require().NoError(err, "Kill")
	c.EqDeep([]string{"kill:c_3", "kill:c_2", "kill:c_1"}, l.order, "kill order")
	c.EqDeep([]string{"c_3", "c_2"}, resp.Msg.GetDescendantIds(), "reported descendants")
}

func TestKillWithoutIncludeDescendantsLeavesTheSubtreeAlone(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_2"}}
	resp, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(
		&rafikiv1.KillRequest{ChildId: "c_1"}))
	c.Require().NoError(err, "Kill")
	c.EqDeep([]string{"kill:c_1"}, l.order, "kill order")
	c.Eq(0, len(resp.Msg.GetDescendantIds()), "reported descendants")
}

func TestCascadeSkipsDescendantsThatAreAlreadyGone(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{
		descendants: []string{"c_3", "c_2"},
		errFor: map[string]error{
			"c_3": &connectapi.ControllerError{Code: protocol.ErrChildExited, Message: "child has already exited"},
		},
	}
	resp, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(
		&rafikiv1.KillRequest{ChildId: "c_1", IncludeDescendants: true}))
	c.Require().NoError(err, "Kill")
	c.EqDeep([]string{"c_2"}, resp.Msg.GetDescendantIds(), "only the descendant this call ended is reported")
	c.EqDeep([]string{"kill:c_3", "kill:c_2", "kill:c_1"}, l.order, "kill order")
}

// A descendant that cannot be ended must not leave the parent dead and the
// child orphaned and running: the rest of the sweep still runs, the parent does
// not, and the error names the failure.
func TestCascadeFailureLeavesTheParentAlone(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{
		descendants: []string{"c_3", "c_2"},
		errFor: map[string]error{
			"c_3": &connectapi.ControllerError{Code: protocol.ErrNotExited, Message: "child is still running"},
		},
	}
	_, err := cascadeServer(l).Close(context.Background(), connect.NewRequest(
		&rafikiv1.CloseRequest{ChildId: "c_1", IncludeDescendants: true}))
	c.Require().Error(err, "Close")
	c.StrContains(err.Error(), "c_3")
	c.EqDeep([]string{"close:c_3", "close:c_2"}, l.order, "close order: the sweep finishes, the parent is not closed")
}

func TestIncludeDescendantsNeedsALister(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := cascadeServer(&fakeLifecycle{}).Kill(context.Background(), connect.NewRequest(
		&rafikiv1.KillRequest{ChildId: "c_1", IncludeDescendants: true}))
	c.Eq(connect.CodeUnimplemented, connect.CodeOf(err), "a lifecycle that cannot list descendants must refuse, not kill the parent alone")
}
