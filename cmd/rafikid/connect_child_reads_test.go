// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"reflect"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

func childTokenCtx(childID string) context.Context {
	return server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", ChildID: childID, Via: server.ProvenanceChildToken})
}

// A per-child credential reads its own subtree — the MCP face's boundary —
// while every other identity keeps scopeFor's answer, refusals included.
func TestChildConversationScope(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "01a0-worker" })

	got, err := childConversationScope(childTokenCtx("c_d0"), c)
	ck.Require().NoError(err, "per-child secret")
	ck.True(reflect.DeepEqual(insights.ScopeSubtree(c.subtreeSelector("c_d0")), got), "per-child secret scope = %+v", got)
	ck.Contains(c.subtreeSelector("c_d0").ConversationIDs, "01a0-worker", "the subtree names the worker's conversation")

	attributed := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed})
	_, err = childConversationScope(attributed, c)
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "per-boot + session: %v", err)

	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser})
	got, err = childConversationScope(user, c)
	ck.Require().NoError(err, "user")
	ck.Eq(insights.ScopeOwner("u1"), got, "user scope")
}

// The three conversation reads reach the backend with the subtree scope, not
// the owner's.
func TestConnectConversationsReadsUseTheChildSubtree(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3)
	fb := &fakeInsightsBackend{}
	c.insights = fb
	a := connectConversations{c: c}
	want := insights.ScopeSubtree(c.subtreeSelector("c_d0"))

	_, err := a.RunQuery(childTokenCtx("c_d0"), "tools", connectapi.CatalogueFilter{})
	ck.Require().NoError(err, "RunQuery")
	ck.True(reflect.DeepEqual(want, fb.gotScope), "RunQuery scope = %+v", fb.gotScope)

	fb.gotScope = insights.Scope{}
	_, err = a.Search(childTokenCtx("c_d0"), connectapi.ConversationSearchFilter{})
	ck.Require().NoError(err, "Search")
	ck.True(reflect.DeepEqual(want, fb.gotScope), "Search scope = %+v", fb.gotScope)

	fb.gotScope = insights.Scope{}
	fb.transcript = &insights.Transcript{ConversationID: "01a0-worker"}
	_, _, err = a.Export(childTokenCtx("c_d0"), "01a0-worker")
	ck.Require().NoError(err, "Export")
	ck.True(reflect.DeepEqual(want, fb.gotScope), "Export scope = %+v", fb.gotScope)
}

// SetBudget from a per-child credential is agent_set_budget's rule — direct
// parentage, bounded by the caller's remaining grant — never the operator's.
func TestConnectLifecycleSetBudgetFromAChildFollowsTheAgentRule(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3, 3) // c_d0 -> c_d1 -> c_d2
	_ = c.st.Update("c_d0", func(s *childstore.Session) { s.MaxCost = 10 })
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.MaxCost = 2 })
	c.coster = fakeCoster{spend: 5} // $5 of c_d0's $10 left
	l := connectLifecycle{c: c}

	ck.NoError(l.SetBudget(childTokenCtx("c_d0"), "c_d1", 6), "raise within the caller's remaining grant")
	ck.Error(l.SetBudget(childTokenCtx("c_d0"), "c_d1", 12), "raise past the caller's remaining grant")
	ck.Error(l.SetBudget(childTokenCtx("c_d0"), "c_d2", 1), "a grandchild is not a direct child")

	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser})
	ck.NoError(l.SetBudget(user, "c_d1", 12), "the operator is not bounded by the parent's grant")
	snap, _ := c.st.Get("c_d1")
	ck.Eq(12.0, snap.MaxCost, "c_d1.MaxCost after the operator raise")
}
