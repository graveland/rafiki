// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// closableLifecycle adds the exited-descendant lister to treeLifecycle.
type closableLifecycle struct {
	treeLifecycle
	closable []string
}

func (l *closableLifecycle) ClosableDescendantIDs(string) []string { return l.closable }

func TestKillDescendantsOnlyLeavesTheTargetRunning(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_3", "c_2"}}
	resp, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{
		ChildId: "c_1", Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY,
	}))
	c.Require().NoError(err, "Kill")
	c.ElementsMatch([]string{"kill:c_3", "kill:c_2"}, l.order, "only the descendants are killed")
	c.EqDeep([]string{"c_3", "c_2"}, resp.Msg.GetDescendantIds(), "reported descendants")
	c.Eq("c_1", resp.Msg.GetChildId(), "child id")
	c.Eq("", resp.Msg.GetSignal(), "no exit data for a target that was not killed")
	c.True(resp.Msg.ExitCode == nil, "exit code unset")
}

func TestKillExplicitScopesMatchTheLegacyFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		scope rafikiv1.DescendantScope
		want  []string
	}{
		"target only": {rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY, []string{"kill:c_1"}},
		"target and descendants": {
			rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_AND_DESCENDANTS,
			[]string{"kill:c_2", "kill:c_1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			l := &treeLifecycle{descendants: []string{"c_2"}}
			_, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(
				&rafikiv1.KillRequest{ChildId: "c_1", Scope: tc.scope}))
			c.Require().NoError(err, "Kill")
			c.EqDeep(tc.want, l.order, "kill order")
		})
	}
}

func TestKillRefusesAScopeThatContradictsIncludeDescendants(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_2"}}
	_, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{
		ChildId: "c_1", IncludeDescendants: true,
		Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY,
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq(0, len(l.order), "nothing is killed on a refused request")
}

func TestKillRefusesExitedDescendantsOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_2"}}
	_, err := cascadeServer(l).Kill(context.Background(), connect.NewRequest(&rafikiv1.KillRequest{
		ChildId: "c_1", Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY,
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq(0, len(l.order), "nothing is killed on a refused request")
}

func TestCloseDescendantsOnlyLeavesTheTargetOpen(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_3", "c_2"}}
	resp, err := cascadeServer(l).Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: "c_1", Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_DESCENDANTS_ONLY,
	}))
	c.Require().NoError(err, "Close")
	c.EqDeep([]string{"close:c_3", "close:c_2"}, l.order, "only the descendants are closed, in order")
	c.EqDeep([]string{"c_3", "c_2"}, resp.Msg.GetDescendantIds(), "reported descendants")
}

func TestCloseRefusesAScopeThatContradictsIncludeDescendants(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &treeLifecycle{descendants: []string{"c_2"}}
	_, err := cascadeServer(l).Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: "c_1", IncludeDescendants: true,
		Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY,
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq(0, len(l.order), "nothing is closed on a refused request")
}

// The closable list, not the full descendant list, decides what an
// exited-only close touches; a row that went live since the snapshot is
// skipped without failing the sweep or reaching the target.
func TestCloseExitedDescendantsOnlySkipsRowsThatWentLive(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &closableLifecycle{
		treeLifecycle: treeLifecycle{
			descendants: []string{"c_4", "c_3", "c_2"},
			errFor: map[string]error{
				"c_3": &connectapi.ControllerError{Code: protocol.ErrNotExited, Message: "child is still running"},
			},
		},
		closable: []string{"c_3", "c_2"},
	}
	resp, err := cascadeServer(l).Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: "c_1", Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY,
	}))
	c.Require().NoError(err, "Close")
	c.EqDeep([]string{"close:c_3", "close:c_2"}, l.order, "only closable rows are attempted, target untouched")
	c.EqDeep([]string{"c_2"}, resp.Msg.GetDescendantIds(), "a skipped row is not reported as closed")
}

func TestCloseExitedDescendantsOnlyStillFailsOnARealError(t *testing.T) {
	c := assert.NewCollecting(t)
	l := &closableLifecycle{
		treeLifecycle: treeLifecycle{errFor: map[string]error{
			"c_2": &connectapi.ControllerError{Code: protocol.ErrInternal, Message: "disk on fire"},
		}},
		closable: []string{"c_2"},
	}
	_, err := cascadeServer(l).Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: "c_1", Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY,
	}))
	c.Require().Error(err, "Close")
	c.StrContains(err.Error(), "c_2")
}

func TestCloseExitedDescendantsOnlyNeedsTheClosableLister(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := cascadeServer(&treeLifecycle{}).Close(context.Background(), connect.NewRequest(&rafikiv1.CloseRequest{
		ChildId: "c_1", Scope: rafikiv1.DescendantScope_DESCENDANT_SCOPE_EXITED_DESCENDANTS_ONLY,
	}))
	c.Eq(connect.CodeUnimplemented, connect.CodeOf(err), "code")
}
