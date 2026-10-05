// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestConversationSummary_JSONTags(t *testing.T) {
	c := assert.NewCollecting(t)
	b, err := json.Marshal(ConversationSummary{ID: "x", DrivenBy: "client", InputTokens: 42, CacheHitRatio: 0.75, TotalCostUSD: 1.23})
	c.Require().NoError(err, "marshal")
	got := string(b)
	for _, want := range []string{`"driven_by"`, `"input_tokens"`, `"cache_read_tokens"`, `"first_message"`, `"cache_hit_ratio"`, `"total_cost_usd"`} {
		c.StrContains(got, want, "marshaled summary")
	}
	c.False(strings.Contains(got, `"DrivenBy"`) || strings.Contains(got, `"InputTokens"`) || strings.Contains(got, `"TotalCostUSD"`) || strings.Contains(got, `"CacheHitRatio"`), "marshaled summary %s still has CamelCase keys", got)
}

func TestSearch_FiltersByPath(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice") // proxy
	seedConversation(t, pool, "server", "bob")   // direct
	ins := New(pool)

	proxyOnly, err := ins.Search(ctx, ScopeAll(), SearchFilter{Path: PathProxy, Limit: 10})
	c.Require().NoError(err, "search proxy")
	c.Require().Len(proxyOnly, 1, "proxy results = %d, want 1", len(proxyOnly))
	c.Eq("client", proxyOnly[0].DrivenBy, "driven_by")
	c.Greater(0, proxyOnly[0].Turns, "turns")
	c.Eq("alice", proxyOnly[0].Owner, "owner")
	// The aggregate must sum both turns (100+120 in, 0+80 cache_read).
	c.Eq(220, proxyOnly[0].InputTokens, "input_tokens")
	c.Eq(80, proxyOnly[0].CacheReadTokens, "cache_read_tokens")
	c.NotEq("", proxyOnly[0].FirstMessage, "first message snippet is empty, want the seeded user text")

	directOnly, err := ins.Search(ctx, ScopeAll(), SearchFilter{Path: PathDirect, Limit: 10})
	c.Require().NoError(err, "search direct")
	c.Require().False(len(directOnly) != 1 || directOnly[0].DrivenBy != "server", "direct results = %+v, want one server conversation", directOnly)

	all, err := ins.Search(ctx, ScopeAll(), SearchFilter{Limit: 10})
	c.Require().NoError(err, "search all")
	c.Require().Len(all, 2, "unfiltered results = %d, want 2", len(all))
}

func TestSearch_FiltersByOwnerAndText(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice")
	seedConversation(t, pool, "server", "bob")
	ins := New(pool)

	byOwner, err := ins.Search(ctx, ScopeAll(), SearchFilter{Owner: "bob"})
	c.Require().NoError(err, "search owner")
	c.Require().False(len(byOwner) != 1 || byOwner[0].Owner != "bob", "owner filter = %+v, want one bob conversation", byOwner)

	byText, err := ins.Search(ctx, ScopeAll(), SearchFilter{Text: "hello"})
	c.Require().NoError(err, "search text")
	c.Len(byText, 2, "text 'hello' matched %d, want 2", len(byText))

	noText, err := ins.Search(ctx, ScopeAll(), SearchFilter{Text: "nonexistent-substring"})
	c.Require().NoError(err, "search text miss")
	c.Empty(noText, "text miss matched %d, want 0", len(noText))

	byMinTokens, err := ins.Search(ctx, ScopeAll(), SearchFilter{MinTokens: 1000})
	c.Require().NoError(err, "search min tokens")
	c.Empty(byMinTokens, "min tokens 1000 matched %d, want 0 (seeded totals are lower)", len(byMinTokens))
}

func TestSearch_TextMatchesExtractedTextNotJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice") // user msg: [{"type":"text","text":"hello there"}]
	ins := New(pool)

	// "type" appears in the JSON structure but not in the message text.
	byStruct, err := ins.Search(ctx, ScopeAll(), SearchFilter{Text: "type"})
	c.Require().NoError(err, "search")
	c.Empty(byStruct, "text 'type' matched %d, want 0 (must match text, not JSON keys)", len(byStruct))

	// The snippet is the extracted text, not raw JSONB.
	got, err := ins.Search(ctx, ScopeAll(), SearchFilter{Text: "hello"})
	c.Require().NoError(err, "search")
	c.Require().Len(got, 1, "text 'hello' matched %d, want 1", len(got))
	c.Eq("hello there", got[0].FirstMessage, "first_message")
}

func TestSearch_PlainStringContent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "bob")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, source: "claude", inTok: 10})
	insertMessage(t, pool, convID, 0, "user", `"just a plain string"`) // jsonb string, not an array
	ins := New(pool)

	got, err := ins.Search(ctx, ScopeAll(), SearchFilter{Text: "plain"})
	c.NoError(err, "search")
	c.False(len(got) != 1 || got[0].FirstMessage != "just a plain string", "plain-string content search = %+v, want one row with the string text", got)
}

func TestSearch_ModelAndSourceMatchStatsPopulation(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	// conversation.model differs from the per-turn served model.
	convID := insertConversation(t, pool, "client", "carol")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "served-model-x", source: "claude", inTok: 100})
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "served-model-x", source: "slack", inTok: 100})
	ins := New(pool)

	// Search by served model finds it, and Stats over the same model filter
	// selects the same single conversation (aligned population).
	found, err := ins.Search(ctx, ScopeAll(), SearchFilter{Model: "served-model-x"})
	c.Require().NoError(err, "search model")
	c.Require().Len(found, 1, "search by served model = %d, want 1", len(found))
	s, err := ins.GlobalStats(ctx, ScopeAll(), StatsFilter{Model: "served-model-x"})
	c.Require().NoError(err, "stats model")
	c.Eq(1, s.Volume.Conversations, "stats conversations for served model")

	// A mixed-source conversation is found by search for BOTH of its sources.
	for _, src := range []string{"claude", "slack"} {
		got, err := ins.Search(ctx, ScopeAll(), SearchFilter{Source: src})
		c.Require().NoError(err, "search source %s", src)
		c.Len(got, 1, "search source %s = %d, want 1", src, len(got))
	}
}

func TestSearch_SinceMatchesTurnActivityLikeStats(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	// An old conversation with a recent turn, and a recent conversation whose
	// only turn is old. A since-window over "now-ish" must select the first
	// and reject the second — matching the GlobalStats turn population.
	oldConv := insertConversation(t, pool, "client", "alice")
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET created_at = now() - interval '10 days' WHERE id = $1::uuid`,
		oldConv); err != nil {
		t.Fatalf("backdate conversation: %v", err)
	}
	insertTurn(t, pool, oldConv, seedTurn{ordinal: 0, model: "m", source: "claude", inTok: 10})

	staleConv := insertConversation(t, pool, "client", "bob")
	insertTurn(t, pool, staleConv, seedTurn{
		ordinal: 0, model: "m", source: "claude", inTok: 10,
		createdAt: time.Now().Add(-10 * 24 * time.Hour),
	})

	since := time.Now().Add(-time.Hour)
	rows, err := New(pool).Search(ctx, ScopeAll(), SearchFilter{Since: &since})
	c.NoError(err, "search")
	c.False(len(rows) != 1 || rows[0].ID != oldConv, "search since = %+v, want exactly the old conversation with the recent turn", rows)

	// The stats population over the same filter agrees.
	s, err := New(pool).GlobalStats(ctx, ScopeAll(), StatsFilter{Since: &since})
	c.NoError(err, "stats")
	if s.Volume.Conversations != 1 || s.Volume.Turns != 1 {
		t.Errorf("stats volume = %d/%d, want 1/1", s.Volume.Conversations, s.Volume.Turns)
	}
}

func TestSearch_TurnFiltersRequireOneMatchingTurn(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	// model=X and source=Y on DIFFERENT turns must not match a combined
	// model+source filter (stats ANDs them on the same turn row).
	split := insertConversation(t, pool, "client", "carol")
	insertTurn(t, pool, split, seedTurn{ordinal: 0, model: "model-x", source: "slack", inTok: 10})
	insertTurn(t, pool, split, seedTurn{ordinal: 1, model: "model-y", source: "claude", inTok: 10})

	both := insertConversation(t, pool, "client", "dan")
	insertTurn(t, pool, both, seedTurn{ordinal: 0, model: "model-x", source: "claude", inTok: 10})

	rows, err := New(pool).Search(ctx, ScopeAll(), SearchFilter{Model: "model-x", Source: "claude"})
	c.NoError(err, "search")
	c.False(len(rows) != 1 || rows[0].ID != both, "search model+source = %+v, want only the conversation with both on one turn", rows)
}

// TestSearch_ZeroTokenConversationHasZeroCacheHitRatio covers a conversation
// created (via EnsureConversationByExternalRef) before its first turn
// completes: in_tok and cache_read are both 0, so the ratio's denominator is
// nullif(0+0, 0) = NULL. Without the outer coalesce, the column scans NULL
// into the bare float64 CacheHitRatio field and Search fails outright — not
// just for that row, for the whole call.
func TestSearch_ZeroTokenConversationHasZeroCacheHitRatio(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	insertConversation(t, pool, "client", "alice") // no turns: in_tok=cache_read=0

	rows, err := New(pool).Search(ctx, ScopeAll(), SearchFilter{Limit: 10})
	c.Require().NoError(err, "search")
	c.Require().Len(rows, 1, "results = %d, want 1", len(rows))
	c.Eq(0, rows[0].CacheHitRatio, "cache_hit_ratio")
}

func TestSearch_FiltersByEntrypoint(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	// Create a conversation with 'test' entrypoint (default from insertConversation)
	testConv := seedConversation(t, pool, "client", "alice")

	// Create a conversation with 'analyze' entrypoint
	analyzeConv := seedConversation(t, pool, "client", "bob")
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET origin_entrypoint = 'analyze' WHERE id = $1::uuid`,
		analyzeConv); err != nil {
		t.Fatalf("update entrypoint: %v", err)
	}

	ins := New(pool)

	// Filter by Entrypoint='analyze' should return only analyzeConv
	byEntrypoint, err := ins.Search(ctx, ScopeAll(), SearchFilter{Entrypoint: "analyze"})
	c.NoError(err, "search by entrypoint")
	c.False(len(byEntrypoint) != 1 || byEntrypoint[0].ID != analyzeConv, "entrypoint filter = %+v, want one analyze conversation", byEntrypoint)

	// ExcludeEntrypoint='analyze' should return only testConv
	exclude, err := ins.Search(ctx, ScopeAll(), SearchFilter{ExcludeEntrypoint: "analyze"})
	c.NoError(err, "search exclude entrypoint")
	c.False(len(exclude) != 1 || exclude[0].ID != testConv, "exclude entrypoint filter = %+v, want one test conversation", exclude)

	// Zero-value filter returns both
	all, err := ins.Search(ctx, ScopeAll(), SearchFilter{Limit: 10})
	c.NoError(err, "search all")
	c.Len(all, 2, "unfiltered results = %d, want 2", len(all))
}

// `user rm` tombstones instead of deleting and the username uniqueness index
// is partial (WHERE deleted_at IS NULL), so after a remove + recreate several
// rows share one name. The owner filter resolves that to the most recent
// ACTIVE-or-not row; a bare `username = ...` subquery would fail the whole
// query with "more than one row returned by a subquery used as an expression".
func TestSearch_OwnerFilterSurvivesAReusedUsername(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)

	oldID := ensureUser(t, pool, "carol")
	oldConv := seedConversation(t, pool, "client", "carol")
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.users SET deleted_at = now() WHERE id = $1::uuid`, oldID); err != nil {
		t.Fatalf("tombstone carol: %v", err)
	}
	// A second, live carol: ensureUser only ever finds the active row.
	newID := ensureUser(t, pool, "carol")
	c.Require().NotEq(oldID, newID, "recreating a tombstoned username reused the same row")
	newConv := seedConversation(t, pool, "client", "carol")

	got, err := New(pool).Search(ctx, ScopeAll(), SearchFilter{Owner: "carol"})
	c.Require().NoError(err, "search by a reused owner name")
	// The subselect picks the newest row, so only the live carol's
	// conversation matches — the tombstoned one keeps its own id.
	c.Require().False(len(got) != 1 || got[0].ID != newConv, "owner filter returned %+v, want only the live carol's conversation %s", got, newConv)
	c.Eq("carol", got[0].Owner, "owner")

	// The tombstoned user's own conversation still renders their name: the
	// display join is unfiltered by deleted_at even though the filter is not.
	var name string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT coalesce(u.username,'') FROM conversations.conversation c
		   LEFT JOIN conversations.users u ON u.id = c.owner_user_id
		  WHERE c.id = $1::uuid`, oldConv).Scan(&name), "read tombstoned owner name")
	c.Eq("carol", name, "tombstoned owner resolves to")
}

// TestSearchScopeOwnerExcludesOtherOwners pins that scope ANDs with the query
// rather than being replaced by it: a ScopeOwner(bob) search over conversations
// owned by bob and carol yields exactly bob's, and the owner column reports him.
func TestSearchScopeOwnerExcludesOtherOwners(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "bob")
	seedConversation(t, pool, "client", "carol")
	ins := New(pool)

	got, err := ins.Search(ctx, ScopeOwner(ensureUser(t, pool, "bob")), SearchFilter{})
	c.Require().NoError(err, "search scoped to bob")
	c.Require().Len(got, 1, "scoped search returned %d rows, want 1 (bob's only)", len(got))
	c.Eq("bob", got[0].Owner, "owner")
}

// TestSearchScopeAndOwnerFilterCompose pins that the caller's own Owner filter
// NARROWS within scope and can never widen it: naming carol under a scope
// scoped to bob composes two owner conditions that cannot both be true, so the
// answer is zero rows — never carol's conversation.
func TestSearchScopeAndOwnerFilterCompose(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	bobID := ensureUser(t, pool, "bob")
	seedConversation(t, pool, "client", "bob")
	seedConversation(t, pool, "client", "carol")
	ins := New(pool)

	got, err := ins.Search(ctx, ScopeOwner(bobID), SearchFilter{Owner: "carol"})
	c.NoError(err, "search scoped to bob, filtered by owner carol")
	c.Empty(got, "scope + other-owner filter returned %d rows, want 0 (a narrow scope must never return another owner's rows)", len(got))
}

// TestSearchScopeZeroValueReturnsNoRows proves the zero-value-denies rule
// end-to-end through the real query builder, not just cond()'s unit level: a
// Scope{} that reached Search must return no rows at all.
func TestSearchScopeZeroValueReturnsNoRows(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "bob")
	ins := New(pool)

	got, err := ins.Search(ctx, Scope{}, SearchFilter{})
	c.NoError(err, "search with zero-value scope")
	c.Empty(got, "zero-value scope returned %d rows, want 0 (the zero value denies)", len(got))
}

// closedFilterOwner is a per-run username, so a shared test DSN can never leave
// another run's rows behind to be matched by name.
func closedFilterOwner(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestSearchSummaryCarriesClosedAt pins that a closed row reports ClosedAt set
// to the stamped instant (within a second) and an open row leaves it nil.
func TestSearchSummaryCarriesClosedAt(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	owner := closedFilterOwner("closed-at")
	openID := seedConversation(t, pool, "client", owner)
	closedID := seedConversation(t, pool, "client", owner)
	stamped := time.Now().Add(-time.Hour).Truncate(time.Second)
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET closed_at = $2 WHERE id = $1::uuid`, closedID, stamped); err != nil {
		t.Fatalf("stamp closed_at: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`, []string{openID, closedID})
	})

	rows, err := New(pool).Search(ctx, ScopeAll(), SearchFilter{Limit: 10})
	c.Require().NoError(err, "search")
	byID := map[string]ConversationSummary{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	c.Require().HasKey(byID, closedID, "the closed row must be returned")
	c.Require().HasKey(byID, openID, "the open row must be returned")

	gotClosed := byID[closedID].ClosedAt
	c.Require().True(gotClosed != nil, "closed row ClosedAt = nil, want the stamped time")
	c.True(gotClosed.Sub(stamped).Abs() < time.Second,
		"closed row ClosedAt = %v, want within a second of %v", gotClosed, stamped)
	c.True(byID[openID].ClosedAt == nil, "open row ClosedAt = %v, want nil", byID[openID].ClosedAt)
}
