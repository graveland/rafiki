// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// The two conversationReader constructors are the binding story for the
// conversation_search/export tools: the scope they bake in is what keeps a
// caller inside its own corpus. Scope is unexported-fielded (a struct literal
// cannot construct a valid one outside pkg/insights), so equality against the
// constructors is the observable -- zero value denies, ScopeAll admits all.
func TestNewControllerConversationReaderScopesAnEmptyOwnerToDeny(t *testing.T) {
	if r := newControllerConversationReader(nil, ""); r.scope != (insights.Scope{}) {
		t.Errorf("anonymous spawn scope = %+v, want the zero-value deny-all Scope", r.scope)
	}
	if r := newControllerConversationReader(nil, "u-owner"); r.scope != insights.ScopeOwner("u-owner") {
		t.Errorf("owned spawn scope = %+v, want ScopeOwner", r.scope)
	}
}

func TestNewMCPConversationReaderScopePerIdentity(t *testing.T) {
	if r := newMCPConversationReader(nil, users.Identity{IsAdmin: true}); r.scope != insights.ScopeAll() {
		t.Errorf("admin caller scope = %+v, want ScopeAll", r.scope)
	}
	if r := newMCPConversationReader(nil, users.Identity{UserID: "u-alice"}); r.scope != insights.ScopeOwner("u-alice") {
		t.Errorf("user caller scope = %+v, want ScopeOwner", r.scope)
	}
	if r := newMCPConversationReader(nil, users.Identity{}); r.scope != (insights.Scope{}) {
		t.Errorf("anonymous caller scope = %+v, want the zero-value deny-all Scope", r.scope)
	}
}

// TestRunQueryMapsCatalogueEntriesAndColumns covers the insights.QueryResult
// onto tools.CatalogueResult mapping inside RunQuery. It needs no controller
// (the mapping is factored into catalogueResult) because conversationReader's
// ctrl is the concrete *Controller -- there is no fake to bind, and the two
// scope-construction tests above are the level the nil-ctrl readers support.
func TestRunQueryMapsCatalogueEntriesAndColumns(t *testing.T) {
	c := assert.NewCollecting(t)
	res, err := catalogueResult(insights.QueryResult{
		Columns: []insights.Column{
			{Name: "tool", Kind: insights.ColString},
			{Name: "calls", Kind: insights.ColInt},
			{Name: "coverage", Kind: insights.ColFloat, Format: "pct"},
		},
		Rows: [][]insights.Entry{
			{insights.StringEntry("bash"), insights.IntEntry(3), insights.FloatEntry(0.75)},
		},
	})
	c.Require().NoError(err, "catalogueResult")
	c.False(len(res.Columns) != 3 ||
		res.Columns[0].Kind != "string" || res.Columns[1].Kind != "int" ||
		res.Columns[2].Kind != "float" || res.Columns[2].Format != "pct", "columns = %+v, want the three kinds mapped and Format carried", res.Columns)
	c.Require().False(len(res.Rows) != 1 || len(res.Rows[0]) != 3, "rows = %+v, want one row of three cells", res.Rows)
	str, i, fl := res.Rows[0][0], res.Rows[0][1], res.Rows[0][2]
	c.False(str.Str != "bash" || str.IsInt || str.IsFloat, "cell 0 = %+v, want Str only", str)
	c.False(!i.IsInt || i.Int != 3 || i.IsFloat, "cell 1 = %+v, want Int only", i)
	c.False(!fl.IsFloat || fl.Float != 0.75 || fl.IsInt, "cell 2 = %+v, want Float only", fl)

	// An Entry outside the three concrete types cannot be built outside
	// pkg/insights (isEntry is unexported), but a nil cell reaches the same
	// default arm: fail loudly rather than render an empty cell.
	if _, err := catalogueResult(insights.QueryResult{
		Rows: [][]insights.Entry{{nil}},
	}); err == nil {
		t.Error("catalogueResult with a nil Entry returned no error")
	}
}

// TestConversationReaderRunQueryMapsScope observes the scope a reader was
// built with reaching the call. ctrl is the concrete *Controller (no fake
// exists), so this runs against a real pool: the zero-value scope must be
// refused by insights.Query's admission switch (ClassOwnerScoped under an
// invalid scope answers not-found), while an owner scope -- whatever it
// owns, here nothing -- runs and returns the query's declared columns.
func TestConversationReaderRunQueryMapsScope(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := openTestPool(t)
	dir := testSocketDir(t)
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")
	for _, d := range []string{stateDir, logsDir} {
		c.Require().NoError(os.MkdirAll(d, 0o700), "mkdirall %s", d)
	}
	ctrl := NewController(childstore.New(), stateDir, logsDir, filepath.Join(dir, "c.sock"), nil, pool, nil, false, t.Context(), nil, nil, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ctrl.ShutdownAllChildren(ctx, time.Second, time.Second); err != nil {
			t.Logf("cleanup: ShutdownAllChildren: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The zero-value Scope denies: an anonymous spawn's reader must not be
	// able to run a ClassOwnerScoped query at all.
	anon := newControllerConversationReader(ctrl, "")
	if _, err := anon.RunQuery(ctx, "tools", tools.CatalogueFilter{}); err == nil {
		t.Error("RunQuery with the zero-value scope returned no error; the deny-all scope did not reach the admission switch")
	}

	// An owner scope runs: no conversations carry this owner id, so the
	// result is empty -- but the declared columns come back regardless,
	// which is what proves the query executed rather than erroring.
	owned := newControllerConversationReader(ctrl, "00000000-0000-0000-0000-0000000000aa")
	res, err := owned.RunQuery(ctx, "tools", tools.CatalogueFilter{})
	c.Require().NoError(err, "RunQuery with an owner scope")
	c.False(len(res.Columns) != 6 || res.Columns[0].Name != "tool" || res.Columns[1].Name != "calls" ||
		res.Columns[2].Name != "ok" || res.Columns[3].Name != "errors" ||
		res.Columns[4].Name != "unmatched" || res.Columns[5].Name != "conversations", "columns = %+v, want the tools query's declared schema", res.Columns)
	c.Empty(res.Rows, "rows")

	// An unknown name is refused even under a valid scope.
	if _, err := owned.RunQuery(ctx, "no-such-query", tools.CatalogueFilter{}); err == nil {
		t.Error("RunQuery with an unknown name returned no error")
	}
}

// listFixture is one conversations.conversation row a listing test reads. The
// name carries a per-run token: the shared test database is never reset, so
// every assertion narrows by that token and never by a global count.
type listFixture struct {
	ownerID string // users.id; "" leaves owner_user_id NULL
	name    string
	entry   string // origin_entrypoint; 'test' when empty
	cwd     string
}

func insertListFixture(t *testing.T, pool *pgxpool.Pool, f listFixture) string {
	t.Helper()
	var ownerArg any
	if f.ownerID != "" {
		ownerArg = f.ownerID
	}
	entry := f.entry
	if entry == "" {
		entry = "test"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx, `
INSERT INTO conversations.conversation
    (owner_user_id, name, origin_entrypoint, driven_by, cwd, node_count)
VALUES ($1::uuid, $2, $3, 'client', NULLIF($4,''), 1)
RETURNING id::text`, ownerArg, f.name, entry, f.cwd).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert listing conversation %q", f.name)
	return id
}

// insertListOwner writes one users row for a per-run owner scope.
func insertListOwner(t *testing.T, pool *pgxpool.Pool, label string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ($1, $1 || ':' || gen_random_uuid()::text) RETURNING id::text`,
		fmt.Sprintf("%s-%d", label, time.Now().UnixNano())).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert listing owner %q", label)
	return id
}

// insertListChild links a child row to a conversation, carrying the labels a
// labels filter needs.
func insertListChild(t *testing.T, pool *pgxpool.Pool, childID, convID, labels string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
INSERT INTO conversations.child (child_id, conversation_id, kind, status, spawned_at, labels)
VALUES ($1, $2::uuid, 'fundi', 'idle', now(), $3::jsonb)`, childID, convID, labels)
	assert.NewAborting(t).NoError(err, "insert listing child %s", childID)
}

// cleanupListFixtures removes this run's rows. Every id is per-run unique, so
// the deletes reach fixture rows only.
func cleanupListFixtures(t *testing.T, pool *pgxpool.Pool, convIDs, childIDs, userIDs []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		if len(childIDs) > 0 {
			if _, err := pool.Exec(ctx,
				`DELETE FROM conversations.child WHERE child_id = ANY($1)`, childIDs); err != nil {
				t.Logf("cleanup children: %v", err)
			}
		}
		if len(convIDs) > 0 {
			if _, err := pool.Exec(ctx,
				`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`, convIDs); err != nil {
				t.Logf("cleanup conversations: %v", err)
			}
		}
		if len(userIDs) > 0 {
			if _, err := pool.Exec(ctx,
				`DELETE FROM conversations.users WHERE id = ANY($1::uuid[])`, userIDs); err != nil {
				t.Logf("cleanup users: %v", err)
			}
		}
	})
}

// listRowIDs collects a tool page's row ids.
func listRowIDs(rows []tools.ConversationListRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// TestConversationListToolExcludesDaemonsOwnConversations pins the fundi/MCP
// faces' exclusion through the reader the tools call: the daemon's own LLM
// jobs (recall-summary, analyze) are never listed, and the filter cannot
// re-admit them. The owner scope is the fundi runtime's, so a row of another
// owner must stay out too.
func TestConversationListToolExcludesDaemonsOwnConversations(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := insertListOwner(t, pool, "convlist-excluded")
	other := insertListOwner(t, pool, "convlist-excluded-other")

	kept := insertListFixture(t, pool, listFixture{ownerID: owner, name: "visible-" + uniq})
	summary := insertListFixture(t, pool, listFixture{ownerID: owner, name: "recall-summary-" + uniq, entry: "recall-summary"})
	analyze := insertListFixture(t, pool, listFixture{ownerID: owner, name: "analyze-" + uniq, entry: "analyze"})
	foreign := insertListFixture(t, pool, listFixture{ownerID: other, name: "foreign-" + uniq})
	cleanupListFixtures(t, pool, []string{kept, summary, analyze, foreign}, nil, []string{owner, other})

	ctrl := &Controller{pool: pool, listInsights: insights.New(pool)}
	reader := newControllerConversationReader(ctrl, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := reader.ConversationList(ctx, tools.ConversationListQuery{NameContains: uniq})
	c.Require().NoError(err, "ConversationList")
	c.EqDeep([]string{kept}, listRowIDs(page.Rows),
		"the listing must carry the readable conversation and neither the daemon's own jobs nor another owner's row")
}

// TestConversationListToolAppliesTheLabelsFilter pins the labels mapping
// end to end: two conversations match the name, and only the one whose child
// carries the label comes back. Dropping Labels from the mapping returns both.
func TestConversationListToolAppliesTheLabelsFilter(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := insertListOwner(t, pool, "convlist-labels")

	labelled := insertListFixture(t, pool, listFixture{ownerID: owner, name: "labelled-" + uniq})
	bare := insertListFixture(t, pool, listFixture{ownerID: owner, name: "bare-" + uniq})
	childID := "c_" + uniq + "_labels"
	insertListChild(t, pool, childID, labelled, fmt.Sprintf(`{"team":%q}`, uniq))
	cleanupListFixtures(t, pool, []string{labelled, bare}, []string{childID}, []string{owner})

	ctrl := &Controller{pool: pool, listInsights: insights.New(pool)}
	reader := newControllerConversationReader(ctrl, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := reader.ConversationList(ctx, tools.ConversationListQuery{
		NameContains: uniq, Labels: map[string]string{"team": uniq},
	})
	c.Require().NoError(err, "ConversationList")
	c.EqDeep([]string{labelled}, listRowIDs(page.Rows),
		"only the conversation whose child carries the label may be listed")
}

// TestConversationListToolAppliesTheCwdFilter pins the cwd mapping end to end:
// two conversations match the name, and only the one under the requested
// directory comes back. Dropping CwdContains from the mapping returns both.
func TestConversationListToolAppliesTheCwdFilter(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := insertListOwner(t, pool, "convlist-cwd")

	wanted := insertListFixture(t, pool, listFixture{ownerID: owner, name: "cwd-" + uniq, cwd: "/wanted/" + uniq})
	elsewhere := insertListFixture(t, pool, listFixture{ownerID: owner, name: "cwd-other-" + uniq, cwd: "/elsewhere/" + uniq})
	cleanupListFixtures(t, pool, []string{wanted, elsewhere}, nil, []string{owner})

	ctrl := &Controller{pool: pool, listInsights: insights.New(pool)}
	reader := newControllerConversationReader(ctrl, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := reader.ConversationList(ctx, tools.ConversationListQuery{
		NameContains: uniq, CwdContains: "/wanted/" + uniq,
	})
	c.Require().NoError(err, "ConversationList")
	c.EqDeep([]string{wanted}, listRowIDs(page.Rows),
		"only the conversation under the requested cwd may be listed")
}

// TestListConversationsToolFilterMapsEveryField pins the whole tool-to-filter mapping
// field by field, so dropping any single field from it fails here even where a
// database fixture would be needed to observe that field's effect.
func TestListConversationsToolFilterMapsEveryField(t *testing.T) {
	c := assert.NewCollecting(t)
	since, err := time.Parse(time.RFC3339, "2026-01-02T03:04:05Z")
	c.Require().NoError(err, "parse since")
	until, err := time.Parse(time.RFC3339, "2026-01-03T06:07:08Z")
	c.Require().NoError(err, "parse until")

	f, err := listFilterFromTool(tools.ConversationListQuery{
		Limit: 7, Cursor: "cur-1", Sort: "created",
		ParentConversationID: "01a0-parent", Query: "deploy",
		States: []string{"open", "no_agent"}, Kinds: []string{"fundi"},
		NameContains: "fix", CwdContains: "/work",
		Labels: map[string]string{"team": "infra"},
		Since:  &since, Until: &until,
	})
	c.Require().NoError(err, "listFilterFromTool")
	c.Eq(7, f.Limit, "Limit")
	c.Eq("cur-1", f.Cursor, "Cursor")
	c.Eq(insights.ListSortCreated, f.Sort, "Sort")
	c.Eq("01a0-parent", f.ParentConversationID, "ParentConversationID")
	c.Eq("deploy", f.Query, "Query")
	c.EqDeep([]insights.ListState{insights.ListStateOpen, insights.ListStateNoAgent}, f.States, "States")
	c.EqDeep([]string{"fundi"}, f.Kinds, "Kinds")
	c.Eq("fix", f.NameContains, "NameContains")
	c.Eq("/work", f.CwdContains, "CwdContains")
	c.EqDeep(map[string]string{"team": "infra"}, f.Labels, "Labels")
	c.True(f.Since != nil && f.Since.Equal(since), "Since = %v, want %v", f.Since, since)
	c.True(f.Until != nil && f.Until.Equal(until), "Until = %v, want %v", f.Until, until)
	c.EqDeep(recall.ExcludedEntrypoints, f.Excluded,
		"the daemon's own LLM conversations must be excluded on both faces")

	// A sort or state outside the vocabulary fails loudly rather than
	// re-ordering or widening the answer.
	for _, tc := range []struct {
		name, field string
		q           tools.ConversationListQuery
	}{
		{"sort", "sort", tools.ConversationListQuery{Sort: "newest"}},
		{"state", "state", tools.ConversationListQuery{States: []string{"archived"}}},
	} {
		_, err := listFilterFromTool(tc.q)
		c.Require().Error(err, "%s: listFilterFromTool returned no error", tc.name)
		c.StrContains(err.Error(), tc.field, "%s: the error does not name the field", tc.name)
	}
}
