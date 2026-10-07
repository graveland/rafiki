// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// mcpChildRequest builds a request carrying a per-child credential the way
// UserTokenAuth.Middleware leaves one: on the context, not in a header.
func mcpChildRequest(ownerUserID, childID string, admin bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	return r.WithContext(server.WithIdentity(r.Context(), &server.Identity{
		UserID: ownerUserID, IsAdmin: admin, ChildID: childID, Via: server.ProvenanceChildToken,
	}))
}

// TestMCPSandboxToolsAppearWhenStoreExists pins that a daemon with a sandbox
// table exposes the three sandbox tools on this face, for a user credential and
// a per-child credential alike.
func TestMCPSandboxToolsAppearWhenStoreExists(t *testing.T) {
	c := assert.NewCollecting(t)
	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = newFakeSandboxStore()

	requests := map[string]*http.Request{
		"user":  mcpRequestFor("u-alice"),
		"child": mcpChildRequest("u-alice", "c-child", false),
	}
	for provenance, req := range requests {
		names := mcpToolNames(t, mcpConnect(t, face.getServer(req)))
		for _, name := range []string{"sandbox_create", "sandbox_list", "sandbox_remove"} {
			c.Contains(names, name, "%s request is missing", provenance)
		}
	}
}

// TestMCPSandboxToolsAbsentWithoutStore pins the decline: a nil sandbox store
// (a DB-less daemon) materializes none of the three, the same nil-means-decline
// rule the preset and pymodule blueprints follow.
func TestMCPSandboxToolsAbsentWithoutStore(t *testing.T) {
	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = nil

	names := mcpToolNames(t, mcpConnect(t, face.getServer(mcpRequestFor("u-alice"))))
	for _, name := range []string{"sandbox_create", "sandbox_list", "sandbox_remove"} {
		assert.NewCollecting(t).NotContains(names, name, "nil sandbox store unexpectedly exposes")
	}
}

// TestMCPSandboxMatchesConnectChildPolicy pins CLAUDE.md's "change both or
// neither": a per-child credential may call the sandbox verbs on the Connect
// plane (CreateSandbox/RemoveSandbox childScoped, ListSandboxes ownerScoped —
// all admitted to a per-child token), and this face must expose exactly those
// three to a per-child credential. Widening or narrowing one plane alone
// diverges the two credential surfaces.
func TestMCPSandboxMatchesConnectChildPolicy(t *testing.T) {
	c := assert.NewAborting(t)

	// The Connect gate's classification, and that a per-child token is admitted.
	child := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u-owner", ChildID: "c-child", Via: server.ProvenanceChildToken,
	})
	for _, proc := range []string{
		"/rafiki.v1.Control/CreateSandbox",
		"/rafiki.v1.Control/RemoveSandbox",
	} {
		c.Eq(policyChildScoped, policyFor(proc), "policy for %s", proc)
		c.NoError(authorizeControlProcedure(child, proc), "a child credential must be admitted to %s", proc)
	}
	c.Eq(policyOwnerScoped, policyFor("/rafiki.v1.Control/ListSandboxes"), "policy for ListSandboxes")
	c.NoError(authorizeControlProcedure(child, "/rafiki.v1.Control/ListSandboxes"), "a child credential must be admitted to ListSandboxes")

	// The MCP face grants the same three to the same credential.
	face, _ := mcpFaceFixture(t)
	face.controller().sandboxStore = newFakeSandboxStore()
	names := mcpToolNames(t, mcpConnect(t, face.getServer(mcpChildRequest("u-owner", "c-child", false))))
	for _, name := range []string{"sandbox_create", "sandbox_list", "sandbox_remove"} {
		c.Contains(names, name, "the MCP child credential is missing a verb the Connect plane admits")
	}
}

// TestMCPSandboxManagerListScopesToOwner drives sandbox_list through the real
// face with a per-child credential whose owner is an admin: the child sees only
// its OWNER's sandboxes, never another user's and never an admin-wide fleet.
// The wire carries no owner, so this pins the credential→owner resolution path
// on this face (the twin of TestConnectSandboxListScopesToTheOwner).
func TestMCPSandboxManagerListScopesToOwner(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	face, _ := mcpFaceFixture(t)
	st := newFakeSandboxStore()
	c.NoError(st.Insert(ctx, sandbox.Row{ID: "sbx_admin", OwnerUserID: "u-admin", Name: "admin-box", State: sandboxStateReady}), "seed admin row")
	c.NoError(st.Insert(ctx, sandbox.Row{ID: "sbx_other", OwnerUserID: "u-other", Name: "other-box", State: sandboxStateReady}), "seed other row")
	face.controller().sandboxStore = st

	cs := mcpConnect(t, face.getServer(mcpChildRequest("u-admin", "c-child", true)))
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sandbox_list"})
	c.NoError(err, "sandbox_list over MCP")
	text := mcpCallText(t, res)
	c.StrContains(text, "sbx_admin", "sandbox_list did not surface the owner's sandbox")
	c.NotStrContains(text, "sbx_other", "sandbox_list leaked another user's sandbox")
}

// TestMCPSandboxManagerRemoveUsesChildId pins that the binding passes the
// caller's child id to the Controller: a row created by a non-descendant is
// refused, which can only happen when callerChild != "" reached SandboxRemove.
func TestMCPSandboxManagerRemoveUsesChildId(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	face, _ := mcpFaceFixture(t)
	st := newFakeSandboxStore()
	c.NoError(st.Insert(ctx, sandbox.Row{ID: "sbx_x", OwnerUserID: "u-admin", Name: "other-box", CreatedBy: "c-other", State: sandboxStateReady}), "seed row")
	face.controller().sandboxStore = st

	cs := mcpConnect(t, face.getServer(mcpChildRequest("u-admin", "c-child", false)))
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sandbox_remove",
		Arguments: map[string]any{"ref": "other-box"},
	})
	c.NoError(err, "sandbox_remove over MCP")
	c.True(res.IsError, "a child must not remove a row a non-descendant created")
	c.StrContains(mcpToolResultText(t, res), "not created by this child", "refusal reason")
}

// TestMCPSandboxOwnerDemotesAChildOfAnAdmin is the MCP-side twin of the Connect
// test TestSandboxOwnerIdentityDemotesAChildOfAnAdmin: the owner the sandbox
// binding receives carries the admin bit forward ONLY for a genuine user
// credential. connectapi's sandboxCaller applies the same rule, so a
// non-user credential that somehow presents the admin bit (a hand-built
// identity; a real child token carries none) is demoted on both faces. The
// empty-ChildID child-token shape is the one the review flagged: it has no
// child id, so the manager cannot demote it via callerChild and the admin bit
// must already be gone here.
func TestMCPSandboxOwnerDemotesAChildOfAnAdmin(t *testing.T) {
	c := assert.NewCollecting(t)

	user := mcpSandboxOwner(&server.Identity{UserID: "u-admin", Username: "root", IsAdmin: true, Via: server.ProvenanceUser})
	c.Eq("u-admin", user.UserID, "owner user id preserved")
	c.True(user.IsAdmin, "a real user credential keeps its admin bit")

	child := mcpSandboxOwner(&server.Identity{UserID: "u-admin", IsAdmin: true, ChildID: "c-child", Via: server.ProvenanceChildToken})
	c.Eq("u-admin", child.UserID, "a child resolves its owner's user id")
	c.False(child.IsAdmin, "a child-token credential must never keep the admin bit")

	empty := mcpSandboxOwner(&server.Identity{UserID: "u-admin", IsAdmin: true, Via: server.ProvenanceChildToken})
	c.False(empty.IsAdmin, "an empty-ChildID child token must never keep the admin bit")
}

// TestMCPSandboxManagerBindingsAreIsolated pins that two managers built for two
// different children never see each other's child id: each binding carries the
// id it was constructed with into Controller.SandboxRemove, so a row one child
// created is removable by that child's binding and refused by the other's. The
// child id is closed over at construction and never arrives as an argument, so
// there is no way for one binding to act as the other.
func TestMCPSandboxManagerBindingsAreIsolated(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	store := newFakeSandboxStore()
	ctrl := &Controller{st: childstore.New(), sandboxStore: store}
	owner := users.Identity{UserID: "u-owner"}

	seed := func(id string) {
		c.NoError(store.Insert(ctx, sandbox.Row{
			ID: id, OwnerUserID: "u-owner", Name: id, CreatedBy: "c-a", State: sandboxStateReady,
		}), "seed %s", id)
	}

	a := newMCPSandboxes(ctrl, owner, "c-a")
	b := newMCPSandboxes(ctrl, owner, "c-b")

	// Child A may remove its own row.
	seed("sbx_one")
	c.NoError(a.Remove(ctx, "sbx_one"), "child A removing its own row")

	// Child B's binding carries c-b, not c-a, so A's row is refused.
	seed("sbx_two")
	err := b.Remove(ctx, "sbx_two")
	c.Error(err, "child B removing A's row must be refused")
	c.StrContains(err.Error(), "not created by this child", "refusal reason")

	// A still carries c-a on a later call.
	seed("sbx_three")
	c.NoError(a.Remove(ctx, "sbx_three"), "child A removing another of its own rows")
}
