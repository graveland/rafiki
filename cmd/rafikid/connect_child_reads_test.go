// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/agentcli/local"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

func childTokenCtx(childID string) context.Context {
	return childTokenCtxFor(childID, "u1")
}

// childTokenCtxFor is childTokenCtx with an explicit identity UserID, so a test
// can prove a child's conversation scope comes from its stored lineage rather
// than from the owner id its credential happens to carry.
func childTokenCtxFor(childID, userID string) context.Context {
	return server.WithIdentity(context.Background(),
		&server.Identity{UserID: userID, ChildID: childID, Via: server.ProvenanceChildToken})
}

// A per-child credential whose lineage has NO owner reads its own subtree — the
// MCP face's boundary — while every other identity keeps scopeFor's answer,
// refusals included.
func TestChildConversationScope(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1, neither with an owner
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "01a0-worker" })

	got, err := childConversationScope(childTokenCtx("c_d0"), c)
	ck.Require().NoError(err, "per-child secret")
	sel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "subtree selector")
	ck.True(reflect.DeepEqual(insights.ScopeSubtree(sel), got), "per-child secret scope = %+v", got)
	ck.Contains(sel.ConversationIDs, "01a0-worker", "the subtree names the worker's conversation")

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

// A child whose lineage resolves an owner reads that OWNER's corpus. The owner
// comes from the stored parent chain, never from the identity's own UserID: the
// credential here carries "u-identity" and the scope must still be the row's
// owner.
func TestChildConversationScopeIsOwnerWhenLineageHasOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	c := ownerReadFixture(t,
		ownerReadChild{id: "c_a", owner: "u-owner-lineage", started: t0},
		ownerReadChild{id: "c_a1", parent: "c_a", started: t0.Add(time.Minute)},
	)

	got, err := childConversationScope(childTokenCtxFor("c_a1", "u-identity"), c)
	ck.Require().NoError(err, "ownerful child")
	ck.Eq(insights.ScopeOwner("u-owner-lineage"), got,
		"the scope must be the lineage owner's corpus, not the identity's UserID; got %+v", got)
}

// An ownerless lineage keeps the subtree boundary, resolved through the same
// subtreeSelector SubtreeCost prices.
func TestChildConversationScopeIsSubtreeForOwnerlessLineage(t *testing.T) {
	ck := assert.NewCollecting(t)
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	c := ownerReadFixture(t,
		ownerReadChild{id: "c_d", started: t0},
		ownerReadChild{id: "c_d1", parent: "c_d", started: t0.Add(time.Minute)},
	)
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "s-ownerless" })

	got, err := childConversationScope(childTokenCtx("c_d"), c)
	ck.Require().NoError(err, "ownerless child")
	sel, err := c.subtreeSelector(context.Background(), "c_d")
	ck.Require().NoError(err, "subtree selector")
	ck.True(reflect.DeepEqual(insights.ScopeSubtree(sel), got), "ownerless scope = %+v", got)
	ck.Contains(sel.ConversationIDs, "s-ownerless", "the subtree names the descendant's conversation")
}

// The three conversation reads reach the backend with the subtree scope for an
// ownerless child, not the identity's owner scope.
func TestConnectConversationsReadsUseTheChildSubtree(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1, neither with an owner
	fb := &fakeInsightsBackend{}
	c.insights = fb
	a := connectConversations{c: c}
	wantSel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "subtree selector")
	want := insights.ScopeSubtree(wantSel)

	_, err = a.RunQuery(childTokenCtx("c_d0"), "tools", connectapi.CatalogueFilter{})
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

// insertOwnerUser seeds one user row, admin or not, and returns its id.
func insertOwnerUser(t *testing.T, pool *pgxpool.Pool, username string, admin bool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256, is_admin)
		 VALUES ($1, $1 || ':' || gen_random_uuid()::text, $2) RETURNING id::text`,
		username, admin).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert user %s", username)
	return id
}

// insertOwnedConversation seeds one conversation owned by ownerID. persona is
// unique per run so a search can narrow to this run's rows on the shared,
// never-reset test database.
func insertOwnedConversation(t *testing.T, pool *pgxpool.Pool, ownerID, persona, extRef string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation
		    (persona, model, origin_entrypoint, driven_by, external_ref, owner_user_id)
		 VALUES ($1, 'test/model', 'test', 'server', $2, $3::uuid) RETURNING id::text`,
		persona, extRef, ownerID).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation for owner %s", ownerID)
	return id
}

// cleanupOwnerReadRows removes this run's conversations and users. The test
// database is shared and never reset, so a test must not leave rows behind.
func cleanupOwnerReadRows(t *testing.T, pool *pgxpool.Pool, convIDs, usernames []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		if len(convIDs) > 0 {
			if _, err := pool.Exec(ctx,
				`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`, convIDs); err != nil {
				t.Logf("cleanup conversations: %v", err)
			}
		}
		for _, name := range usernames {
			if _, err := pool.Exec(ctx, `DELETE FROM conversations.users WHERE username = $1`, name); err != nil {
				t.Logf("cleanup user %s: %v", name, err)
			}
		}
	})
}

// conversationIDsOf returns the ids of a search result.
func conversationIDsOf(rows []connectapi.ConversationSummaryRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestChildConversationsOwnerReachesSiblingConversation is the end-to-end proof
// of the read rule on the Connect conversation verbs: a child credential reads
// its OWNER's corpus, so a top-level sibling's captured conversation is visible
// through Search and Export, while a foreign owner's conversation is absent
// from both.
func TestChildConversationsOwnerReachesSiblingConversation(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	ownerName := "owner-read-" + uniq
	foreignName := "owner-read-foreign-" + uniq
	ownerID := insertOwnerUser(t, pool, ownerName, false)
	foreignID := insertOwnerUser(t, pool, foreignName, false)
	persona := "owner-read-persona-" + uniq

	convA := insertOwnedConversation(t, pool, ownerID, persona, "c_"+uniq+"_a")
	convB := insertOwnedConversation(t, pool, ownerID, persona, "c_"+uniq+"_b")
	foreign := insertOwnedConversation(t, pool, foreignID, persona, "c_"+uniq+"_x")
	cleanupOwnerReadRows(t, pool, []string{convA, convB, foreign}, []string{ownerName, foreignName})

	t0 := time.Now()
	childA := "c_" + uniq + "_a"
	c := ownerReadFixture(t,
		ownerReadChild{id: childA, owner: ownerID, started: t0},
		ownerReadChild{id: "c_" + uniq + "_b", owner: ownerID, started: t0.Add(time.Second)},
	)
	c.insights = local.New(local.Options{Pool: pool})
	a := connectConversations{c: c}
	ctx := childTokenCtxFor(childA, "u-identity")

	rows, err := a.Search(ctx, connectapi.ConversationSearchFilter{Persona: persona})
	ck.NoError(err, "child search")
	got := conversationIDsOf(rows)
	ck.Contains(got, convA, "the caller's own conversation must be readable; got %v", got)
	ck.Contains(got, convB, "a same-owner sibling's conversation must be readable; got %v", got)
	ck.NotContains(got, foreign, "a foreign owner's conversation must not be readable; got %v", got)

	_, ok, err := a.Export(ctx, convB)
	ck.NoError(err, "child export of the sibling conversation")
	ck.True(ok, "a same-owner sibling's conversation must export")

	_, ok, err = a.Export(ctx, foreign)
	ck.NoError(err, "export of a foreign conversation is a scope miss, not an error")
	ck.False(ok, "a foreign owner's conversation must not export")
}

// TestChildConversationsOwnerOfAdminIsNotDaemonWide pins the one boundary the
// design draws inside a daemon: a child of an ADMIN-owned lineage reads that
// admin's own corpus, never daemon-wide. The scope is built from the owner id
// alone, so no admin bit can widen it.
func TestChildConversationsOwnerOfAdminIsNotDaemonWide(t *testing.T) {
	ck := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	adminName := "owner-read-admin-" + uniq
	thirdName := "owner-read-third-" + uniq
	adminID := insertOwnerUser(t, pool, adminName, true)
	thirdID := insertOwnerUser(t, pool, thirdName, false)
	persona := "owner-read-admin-persona-" + uniq

	adminConv := insertOwnedConversation(t, pool, adminID, persona, "c_"+uniq+"_admin")
	thirdConv := insertOwnedConversation(t, pool, thirdID, persona, "c_"+uniq+"_third")
	cleanupOwnerReadRows(t, pool, []string{adminConv, thirdConv}, []string{adminName, thirdName})

	childID := "c_" + uniq + "_admin_child"
	c := ownerReadFixture(t,
		ownerReadChild{id: childID, owner: adminID, started: time.Now()},
	)
	c.insights = local.New(local.Options{Pool: pool})
	a := connectConversations{c: c}
	ctx := childTokenCtxFor(childID, "u-identity")

	rows, err := a.Search(ctx, connectapi.ConversationSearchFilter{Persona: persona})
	ck.NoError(err, "child of an admin owner, search")
	got := conversationIDsOf(rows)
	ck.Contains(got, adminConv, "the admin owner's own conversation must be readable; got %v", got)
	ck.NotContains(got, thirdConv, "a third user's conversation must not be readable from a child of an admin; got %v", got)

	_, ok, err := a.Export(ctx, thirdConv)
	ck.NoError(err, "export of a third user's conversation is a scope miss, not an error")
	ck.False(ok, "a child of an admin must not export a third user's conversation")
}

// fakeListReads records what the Controller's listing seam was called with,
// and answers from fields a test sets.
type fakeListReads struct {
	gotScope     insights.Scope
	gotFilter    insights.ListFilter
	gotVisibleID string

	rows    []insights.ConversationListing
	next    string
	err     error
	visible bool
}

func (f *fakeListReads) ListConversations(_ context.Context, scope insights.Scope, flt insights.ListFilter) ([]insights.ConversationListing, string, error) {
	f.gotScope = scope
	f.gotFilter = flt
	return f.rows, f.next, f.err
}

func (f *fakeListReads) Visible(_ context.Context, scope insights.Scope, conversationID string) (bool, error) {
	f.gotScope = scope
	f.gotVisibleID = conversationID
	return f.visible, f.err
}

// TestListConversationsAdapterMatchesTheConversationReadBoundary pins the
// fourth conversation read onto the same boundary the other three use: a
// child credential's List and Visible resolve childConversationScope (the
// OWNER's corpus for an ownerful lineage, the subtree for an ownerless one),
// and the daemon's own LLM conversations are excluded with no way for a
// caller to change that.
func TestListConversationsAdapterMatchesTheConversationReadBoundary(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3) // c_d0 -> c_d1, neither with an owner
	_ = c.st.Update("c_d1", func(s *childstore.Session) { s.SessionID = "01a0-worker" })
	fb := &fakeListReads{}
	c.listInsights = fb
	a := connectConversations{c: c}
	wantSel, err := c.subtreeSelector(context.Background(), "c_d0")
	ck.Require().NoError(err, "subtree selector")
	want := insights.ScopeSubtree(wantSel)

	_, _, err = a.List(childTokenCtx("c_d0"), connectapi.ConversationListFilter{})
	ck.Require().NoError(err, "List")
	ck.True(reflect.DeepEqual(want, fb.gotScope), "List scope = %+v", fb.gotScope)
	ck.EqDeep(recall.ExcludedEntrypoints, fb.gotFilter.Excluded,
		"List must exclude the daemon's own LLM conversations")

	fb.gotScope = insights.Scope{}
	_, err = a.Visible(childTokenCtx("c_d0"), "01a0-worker")
	ck.Require().NoError(err, "Visible")
	ck.True(reflect.DeepEqual(want, fb.gotScope), "Visible scope = %+v", fb.gotScope)
	ck.Eq("01a0-worker", fb.gotVisibleID, "Visible id")

	// A user credential keeps scopeFor's answer, and the exclusion stays.
	user := server.WithIdentity(context.Background(),
		&server.Identity{UserID: "u1", Via: server.ProvenanceUser})
	fb.gotScope = insights.Scope{}
	fb.gotFilter = insights.ListFilter{}
	_, _, err = a.List(user, connectapi.ConversationListFilter{})
	ck.Require().NoError(err, "List as a user")
	ck.True(reflect.DeepEqual(insights.ScopeOwner("u1"), fb.gotScope), "user scope = %+v", fb.gotScope)
	ck.EqDeep(recall.ExcludedEntrypoints, fb.gotFilter.Excluded,
		"the exclusion must hold for a user credential too")
}

// TestListConversationsAdapterMapsTheListingSentinels pins which Connect code
// each listing sentinel answers with: a cursor that cannot serve the request
// and a relevance sort without a query are the caller's mistake
// (invalid_argument), content search on a database without the index is the
// environment's (failed_precondition).
func TestListConversationsAdapterMapsTheListingSentinels(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		{"bad cursor", insights.ErrBadCursor, connect.CodeInvalidArgument},
		{"relevance needs query", insights.ErrRelevanceNeedsQuery, connect.CodeInvalidArgument},
		{"content search unavailable", insights.ErrContentSearchUnavailable, connect.CodeFailedPrecondition},
	} {
		c := limitsFixture(t, 3, 3)
		fb := &fakeListReads{err: tc.err}
		c.listInsights = fb
		a := connectConversations{c: c}
		_, _, err := a.List(childTokenCtx("c_d0"), connectapi.ConversationListFilter{})
		ck.Eq(tc.want, connect.CodeOf(err), "%s: err = %v, want code", tc.name, err)
	}
}

// TestListConversationsAdapterSortMappingPinsBothIotaBlocks pins the sort's
// second hop: the adapter converts by numeric cast (insights.ListSort(f.Sort)),
// which is correct only while the connectapi and insights blocks agree, so a
// reorder of either iota block fails here. (The first hop, proto enum to
// connectapi, is pinned by name in pkg/connectapi.)
func TestListConversationsAdapterSortMappingPinsBothIotaBlocks(t *testing.T) {
	ck := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   connectapi.ConversationListSort
		want insights.ListSort
	}{
		{"recent", connectapi.ConversationListSortRecent, insights.ListSortRecent},
		{"created", connectapi.ConversationListSortCreated, insights.ListSortCreated},
		{"relevance", connectapi.ConversationListSortRelevance, insights.ListSortRelevance},
	} {
		c := limitsFixture(t, 3, 3)
		fb := &fakeListReads{}
		c.listInsights = fb
		a := connectConversations{c: c}
		_, _, err := a.List(childTokenCtx("c_d0"), connectapi.ConversationListFilter{Sort: tc.in})
		ck.Require().NoError(err, "%s: List", tc.name)
		ck.Eq(tc.want, fb.gotFilter.Sort, "%s: sort = %v, want the insights constant", tc.name, fb.gotFilter.Sort)
	}
}

// TestListConversationsAdapterVisibleCodesAStoreError pins Visible's error
// leg at the adapter: a store failure is a redacted internal answer, never
// the raw driver text and never an uncoded error.
func TestListConversationsAdapterVisibleCodesAStoreError(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := limitsFixture(t, 3, 3)
	c.listInsights = &fakeListReads{err: errors.New("pq: could not connect to host=db.internal")}
	a := connectConversations{c: c}

	_, err := a.Visible(childTokenCtx("c_d0"), "01a0-worker")
	ck.Eq(connect.CodeInternal, connect.CodeOf(err), "store error err = %v, want coded internal", err)
	ck.NotStrContains(err.Error(), "db.internal", "the driver text must not reach the peer")
}

// TestListConversationsAdapterVisibleUnwiredPoolIsUnavailable pins the no-DB
// answer: an unwired listing seam is the same Unavailable every other
// conversation read gives, not a raw sentinel.
func TestListConversationsAdapterVisibleUnwiredPoolIsUnavailable(t *testing.T) {
	ck := assert.NewCollecting(t)
	a := connectConversations{c: &Controller{st: childstore.New(), cm: newChildManager()}}

	_, err := a.Visible(childTokenCtx("c_d0"), "01a0-worker")
	ck.Eq(connect.CodeUnavailable, connect.CodeOf(err), "unwired pool err = %v, want unavailable", err)
}
