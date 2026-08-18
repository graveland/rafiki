// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConversationSummary_JSONTags(t *testing.T) {
	b, err := json.Marshal(ConversationSummary{ID: "x", DrivenBy: "client", InputTokens: 42, CacheHitRatio: 0.75, TotalCostUSD: 1.23})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"driven_by"`, `"input_tokens"`, `"cache_read_tokens"`, `"first_message"`, `"cache_hit_ratio"`, `"total_cost_usd"`} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled summary %s missing %s", got, want)
		}
	}
	if strings.Contains(got, `"DrivenBy"`) || strings.Contains(got, `"InputTokens"`) || strings.Contains(got, `"TotalCostUSD"`) || strings.Contains(got, `"CacheHitRatio"`) {
		t.Errorf("marshaled summary %s still has CamelCase keys", got)
	}
}

func TestSearch_FiltersByPath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice") // proxy
	seedConversation(t, pool, "server", "bob")   // direct
	ins := New(pool)

	proxyOnly, err := ins.Search(ctx, SearchFilter{Path: PathProxy, Limit: 10})
	if err != nil {
		t.Fatalf("search proxy: %v", err)
	}
	if len(proxyOnly) != 1 {
		t.Fatalf("proxy results = %d, want 1", len(proxyOnly))
	}
	if proxyOnly[0].DrivenBy != "client" {
		t.Errorf("driven_by = %q, want client", proxyOnly[0].DrivenBy)
	}
	if proxyOnly[0].Turns <= 0 {
		t.Errorf("turns = %d, want > 0", proxyOnly[0].Turns)
	}
	if proxyOnly[0].Owner != "alice" {
		t.Errorf("owner = %q, want alice", proxyOnly[0].Owner)
	}
	// The aggregate must sum both turns (100+120 in, 0+80 cache_read).
	if proxyOnly[0].InputTokens != 220 {
		t.Errorf("input_tokens = %d, want 220", proxyOnly[0].InputTokens)
	}
	if proxyOnly[0].CacheReadTokens != 80 {
		t.Errorf("cache_read_tokens = %d, want 80", proxyOnly[0].CacheReadTokens)
	}
	if proxyOnly[0].FirstMessage == "" {
		t.Error("first message snippet is empty, want the seeded user text")
	}

	directOnly, err := ins.Search(ctx, SearchFilter{Path: PathDirect, Limit: 10})
	if err != nil {
		t.Fatalf("search direct: %v", err)
	}
	if len(directOnly) != 1 || directOnly[0].DrivenBy != "server" {
		t.Fatalf("direct results = %+v, want one server conversation", directOnly)
	}

	all, err := ins.Search(ctx, SearchFilter{Limit: 10})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered results = %d, want 2", len(all))
	}
}

func TestSearch_FiltersByOwnerAndText(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice")
	seedConversation(t, pool, "server", "bob")
	ins := New(pool)

	byOwner, err := ins.Search(ctx, SearchFilter{Owner: "bob"})
	if err != nil {
		t.Fatalf("search owner: %v", err)
	}
	if len(byOwner) != 1 || byOwner[0].Owner != "bob" {
		t.Fatalf("owner filter = %+v, want one bob conversation", byOwner)
	}

	byText, err := ins.Search(ctx, SearchFilter{Text: "hello"})
	if err != nil {
		t.Fatalf("search text: %v", err)
	}
	if len(byText) != 2 {
		t.Errorf("text 'hello' matched %d, want 2", len(byText))
	}

	noText, err := ins.Search(ctx, SearchFilter{Text: "nonexistent-substring"})
	if err != nil {
		t.Fatalf("search text miss: %v", err)
	}
	if len(noText) != 0 {
		t.Errorf("text miss matched %d, want 0", len(noText))
	}

	byMinTokens, err := ins.Search(ctx, SearchFilter{MinTokens: 1000})
	if err != nil {
		t.Fatalf("search min tokens: %v", err)
	}
	if len(byMinTokens) != 0 {
		t.Errorf("min tokens 1000 matched %d, want 0 (seeded totals are lower)", len(byMinTokens))
	}
}

func TestSearch_TextMatchesExtractedTextNotJSON(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice") // user msg: [{"type":"text","text":"hello there"}]
	ins := New(pool)

	// "type" appears in the JSON structure but not in the message text.
	byStruct, err := ins.Search(ctx, SearchFilter{Text: "type"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(byStruct) != 0 {
		t.Errorf("text 'type' matched %d, want 0 (must match text, not JSON keys)", len(byStruct))
	}

	// The snippet is the extracted text, not raw JSONB.
	got, err := ins.Search(ctx, SearchFilter{Text: "hello"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("text 'hello' matched %d, want 1", len(got))
	}
	if got[0].FirstMessage != "hello there" {
		t.Errorf("first_message = %q, want %q", got[0].FirstMessage, "hello there")
	}
}

func TestSearch_PlainStringContent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "bob")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, source: "claude", inTok: 10})
	insertMessage(t, pool, convID, 0, "user", `"just a plain string"`) // jsonb string, not an array
	ins := New(pool)

	got, err := ins.Search(ctx, SearchFilter{Text: "plain"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].FirstMessage != "just a plain string" {
		t.Fatalf("plain-string content search = %+v, want one row with the string text", got)
	}
}

func TestSearch_ModelAndSourceMatchStatsPopulation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	// conversation.model differs from the per-turn served model.
	convID := insertConversation(t, pool, "client", "carol")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "served-model-x", source: "claude", inTok: 100})
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "served-model-x", source: "slack", inTok: 100})
	ins := New(pool)

	// Search by served model finds it, and Stats over the same model filter
	// selects the same single conversation (aligned population).
	found, err := ins.Search(ctx, SearchFilter{Model: "served-model-x"})
	if err != nil {
		t.Fatalf("search model: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("search by served model = %d, want 1", len(found))
	}
	s, err := ins.GlobalStats(ctx, StatsFilter{Model: "served-model-x"})
	if err != nil {
		t.Fatalf("stats model: %v", err)
	}
	if s.Volume.Conversations != 1 {
		t.Errorf("stats conversations for served model = %d, want 1 (must match search)", s.Volume.Conversations)
	}

	// A mixed-source conversation is found by search for BOTH of its sources.
	for _, src := range []string{"claude", "slack"} {
		got, err := ins.Search(ctx, SearchFilter{Source: src})
		if err != nil {
			t.Fatalf("search source %s: %v", src, err)
		}
		if len(got) != 1 {
			t.Errorf("search source %s = %d, want 1", src, len(got))
		}
	}
}

func TestSearch_SinceMatchesTurnActivityLikeStats(t *testing.T) {
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
	rows, err := New(pool).Search(ctx, SearchFilter{Since: &since})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != oldConv {
		t.Fatalf("search since = %+v, want exactly the old conversation with the recent turn", rows)
	}

	// The stats population over the same filter agrees.
	s, err := New(pool).GlobalStats(ctx, StatsFilter{Since: &since})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.Volume.Conversations != 1 || s.Volume.Turns != 1 {
		t.Errorf("stats volume = %d/%d, want 1/1", s.Volume.Conversations, s.Volume.Turns)
	}
}

func TestSearch_TurnFiltersRequireOneMatchingTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	// model=X and source=Y on DIFFERENT turns must not match a combined
	// model+source filter (stats ANDs them on the same turn row).
	split := insertConversation(t, pool, "client", "carol")
	insertTurn(t, pool, split, seedTurn{ordinal: 0, model: "model-x", source: "slack", inTok: 10})
	insertTurn(t, pool, split, seedTurn{ordinal: 1, model: "model-y", source: "claude", inTok: 10})

	both := insertConversation(t, pool, "client", "dan")
	insertTurn(t, pool, both, seedTurn{ordinal: 0, model: "model-x", source: "claude", inTok: 10})

	rows, err := New(pool).Search(ctx, SearchFilter{Model: "model-x", Source: "claude"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != both {
		t.Fatalf("search model+source = %+v, want only the conversation with both on one turn", rows)
	}
}

// TestSearch_ZeroTokenConversationHasZeroCacheHitRatio covers a conversation
// created (via EnsureConversationByExternalRef) before its first turn
// completes: in_tok and cache_read are both 0, so the ratio's denominator is
// nullif(0+0, 0) = NULL. Without the outer coalesce, the column scans NULL
// into the bare float64 CacheHitRatio field and Search fails outright — not
// just for that row, for the whole call.
func TestSearch_ZeroTokenConversationHasZeroCacheHitRatio(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	insertConversation(t, pool, "client", "alice") // no turns: in_tok=cache_read=0

	rows, err := New(pool).Search(ctx, SearchFilter{Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("results = %d, want 1", len(rows))
	}
	if rows[0].CacheHitRatio != 0 {
		t.Errorf("cache_hit_ratio = %v, want 0", rows[0].CacheHitRatio)
	}
}

func TestSearch_FiltersByEntrypoint(t *testing.T) {
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
	byEntrypoint, err := ins.Search(ctx, SearchFilter{Entrypoint: "analyze"})
	if err != nil {
		t.Fatalf("search by entrypoint: %v", err)
	}
	if len(byEntrypoint) != 1 || byEntrypoint[0].ID != analyzeConv {
		t.Fatalf("entrypoint filter = %+v, want one analyze conversation", byEntrypoint)
	}

	// ExcludeEntrypoint='analyze' should return only testConv
	exclude, err := ins.Search(ctx, SearchFilter{ExcludeEntrypoint: "analyze"})
	if err != nil {
		t.Fatalf("search exclude entrypoint: %v", err)
	}
	if len(exclude) != 1 || exclude[0].ID != testConv {
		t.Fatalf("exclude entrypoint filter = %+v, want one test conversation", exclude)
	}

	// Zero-value filter returns both
	all, err := ins.Search(ctx, SearchFilter{Limit: 10})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered results = %d, want 2", len(all))
	}
}

// `user rm` tombstones instead of deleting and the username uniqueness index
// is partial (WHERE deleted_at IS NULL), so after a remove + recreate several
// rows share one name. The owner filter resolves that to the most recent
// ACTIVE-or-not row; a bare `username = ...` subquery would fail the whole
// query with "more than one row returned by a subquery used as an expression".
func TestSearch_OwnerFilterSurvivesAReusedUsername(t *testing.T) {
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
	if newID == oldID {
		t.Fatal("recreating a tombstoned username reused the same row")
	}
	newConv := seedConversation(t, pool, "client", "carol")

	got, err := New(pool).Search(ctx, SearchFilter{Owner: "carol"})
	if err != nil {
		t.Fatalf("search by a reused owner name: %v", err)
	}
	// The subselect picks the newest row, so only the live carol's
	// conversation matches — the tombstoned one keeps its own id.
	if len(got) != 1 || got[0].ID != newConv {
		t.Fatalf("owner filter returned %+v, want only the live carol's conversation %s", got, newConv)
	}
	if got[0].Owner != "carol" {
		t.Errorf("owner = %q, want carol", got[0].Owner)
	}

	// The tombstoned user's own conversation still renders their name: the
	// display join is unfiltered by deleted_at even though the filter is not.
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(u.username,'') FROM conversations.conversation c
		   LEFT JOIN conversations.users u ON u.id = c.owner_user_id
		  WHERE c.id = $1::uuid`, oldConv).Scan(&name); err != nil {
		t.Fatalf("read tombstoned owner name: %v", err)
	}
	if name != "carol" {
		t.Errorf("tombstoned owner resolves to %q, want carol", name)
	}
}
