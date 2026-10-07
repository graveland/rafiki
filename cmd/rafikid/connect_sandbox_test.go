// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// TestSandboxOwnerIdentityDemotesAChildOfAnAdmin pins the adapter's recallOwner
// rule: a user (or the zero UDS identity, modelled here by callerChild == ""
// with no admin bit) keeps its own admin bit, while a child-provenance caller
// (callerChild != "") is bound to its OWNER's NON-admin identity — a child of
// an admin can never reach daemon-wide sandbox state.
func TestSandboxOwnerIdentityDemotesAChildOfAnAdmin(t *testing.T) {
	c := assert.NewCollecting(t)
	admin := users.Identity{UserID: "u-admin", Username: "root", IsAdmin: true}

	user := sandboxOwnerIdentity(admin, "")
	c.Eq("u-admin", user.UserID, "owner user id preserved")
	c.True(user.IsAdmin, "an operator keeps its admin bit")

	child := sandboxOwnerIdentity(admin, "c_child")
	c.Eq("u-admin", child.UserID, "a child resolves its owner's user id")
	c.False(child.IsAdmin, "a child of an admin is demoted to non-admin (recallOwner)")
}

// TestConnectSandboxListScopesToTheOwner drives ListSandboxes through the real
// handler with a per-child credential whose owner is an admin: the child must
// see only its OWNER's sandboxes, never another user's and never an
// admin-wide fleet. The wire carries no owner, so this pins the whole
// credential→owner resolution path.
func TestConnectSandboxListScopesToTheOwner(t *testing.T) {
	c := assert.NewCollecting(t)
	st := newFakeSandboxStore()
	ctx := context.Background()
	c.Require().NoError(st.Insert(ctx, sandbox.Row{ID: "sbx_admin", OwnerUserID: "u-admin", Name: "admin-box", State: sandboxStateReady}), "seed admin row")
	c.Require().NoError(st.Insert(ctx, sandbox.Row{ID: "sbx_other", OwnerUserID: "u-other", Name: "other-box", State: sandboxStateReady}), "seed other row")

	srv := &connectapi.Server{}
	srv.SetSandboxManager(connectSandbox{c: &Controller{sandboxStore: st}})

	// A per-child credential held by a child of the admin. pkg/server builds
	// this identity with the owner's user id and NO admin bit.
	childCtx := server.WithIdentity(ctx,
		&server.Identity{UserID: "u-admin", ChildID: "c_child", Via: server.ProvenanceChildToken})
	resp, err := srv.ListSandboxes(childCtx, connect.NewRequest(&rafikiv1.ListSandboxesRequest{}))
	c.Require().NoError(err, "ListSandboxes")

	ids := make([]string, 0, len(resp.Msg.GetSandboxes()))
	for _, s := range resp.Msg.GetSandboxes() {
		ids = append(ids, s.GetId())
	}
	c.EqDeep([]string{"sbx_admin"}, ids, "a child of an admin sees only its owner's sandboxes, never the other user's")
}
