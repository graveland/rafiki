// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// The views are the entire FDW surface. If a JSONB payload column ever leaks
// into one, conversation content becomes readable from a downstream grafana DB,
// which every Grafana user can query. This test is that boundary.
func TestViewsExcludePayloadColumns(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool))

	// Forbidden columns common to every view in the FDW surface, plus a
	// per-view extension. v_turn.error is a proxy/transport error string
	// (e.g. "upstream status 429", "context canceled") that the dashboard's
	// "top error messages" panel depends on, so it stays out of the common
	// list. v_analysis/v_finding's analysis and error columns are different:
	// analysis is a serialized analyze.Analysis whose Outcome field is
	// natural-language conversation content, and analysis.error can carry
	// model output — both are payload, not transport plumbing.
	//
	// v_finding.title is on the list for the same reason: it is LLM prose from
	// the same struct as Evidence []TurnCite, generated off verbatim
	// conversation quotes, and no consumer reads it.
	common := []string{"request", "response", "prefix_content", "cache_breakpoints", "content"}
	perView := map[string][]string{
		"v_analysis": {"analysis", "error"},
		"v_finding":  {"analysis", "error", "title"},
	}
	for _, view := range []string{"v_conversation", "v_turn", "v_analysis", "v_finding"} {
		forbidden := append(append([]string{}, common...), perView[view]...)
		for _, col := range forbidden {
			var n int
			err := pool.QueryRow(ctx, `
				SELECT count(*) FROM information_schema.columns
				WHERE table_schema = 'conversations' AND table_name = $1 AND column_name = $2`,
				view, col).Scan(&n)
			c.Require().NoError(err, "query columns of %s", view)
			c.Eq(0, n, "%s exposes payload column %q over the FDW", view, col)
		}
	}
}

// owner_username is resolved by joining users on the conversation's FK. The
// column it replaced, owner_canonical, tried to reverse an identity out of free
// text (rewriting a dash-for-at typo, then classifying human/service/system by
// shape). A users row answers all of that directly, so the heuristics were
// deleted in 0019 rather than ported.
func TestOwnerUsernameResolvesThroughUsersJoin(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool))

	var userID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.users (username, token_sha256)
		VALUES ('brent', 'digest-1') RETURNING id`).Scan(&userID), "insert user")
	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner_user_id, origin_entrypoint, driven_by)
		VALUES ($1, 'claude', 'client') RETURNING id`, userID).Scan(&convID), "insert conversation")

	var got string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT owner_username FROM conversations.v_conversation WHERE id = $1`,
		convID).Scan(&got), "select owner_username")
	c.Eq("brent", got, "owner_username")

	// `user rm` tombstones rather than deleting precisely so history keeps
	// resolving to a name. The view's join must not filter on deleted_at.
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.users SET deleted_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("tombstone user: %v", err)
	}
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT owner_username FROM conversations.v_conversation WHERE id = $1`,
		convID).Scan(&got), "select owner_username after tombstone")
	c.Eq("brent", got, "owner_username after tombstone")
}

// owner_user_id is nullable: a proxy request with no authenticated caller is
// captured unattributed. That must read as NULL — not as an empty string and
// not as a guessed identity — and the conversation must still appear, which is
// why the users join is a LEFT JOIN.
func TestOwnerUsernameNullForUnattributedConversation(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.NoError(Migrate(ctx, pool))

	var id string
	c.NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&id), "insert unattributed conversation")
	var username *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT owner_username FROM conversations.v_conversation WHERE id = $1`,
		id).Scan(&username), "select unattributed row")
	if username != nil {
		t.Errorf("owner_username = %q, want NULL for an unattributed conversation", *username)
	}
}

// An unpriced model must read as "unpriced", never as $0 — a silent zero in a
// spend dashboard is worse than a visible gap.
func TestTurnCostUnpricedModelFlagged(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool))

	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&convID), "insert conversation")
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.model_pricing
			(model_id, or_id, prompt_usd, completion_usd, cache_read_usd, cache_write_usd)
		VALUES ('claude-opus-5', 'anthropic/claude-opus-5', 0.000005, 0.000025, 0.0000005, 0.00000625)`); err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request, input_tokens, output_tokens)
		VALUES ($1, 0, 'complete', 'claude-opus-5', '{}'::jsonb, 1000, 100),
		       ($1, 1, 'complete', 'gpt-5.6',       '{}'::jsonb, 1000, 100)`, convID); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT model, input_usd, output_usd, unpriced
		FROM conversations.v_turn WHERE conversation_id = $1 ORDER BY model`, convID)
	c.Require().NoError(err, "select v_turn")
	defer rows.Close()

	got := map[string]struct {
		in, out  float64
		unpriced bool
	}{}
	for rows.Next() {
		var m string
		var in, out float64
		var un bool
		c.Require().NoError(rows.Scan(&m, &in, &out, &un), "scan")
		got[m] = struct {
			in, out  float64
			unpriced bool
		}{in, out, un}
	}
	if g := got["claude-opus-5"]; g.unpriced || g.in != 0.005 || g.out != 0.0025 {
		t.Errorf("claude-opus-5 => %+v, want in=0.005 out=0.0025 unpriced=false", g)
	}
	g := got["gpt-5.6"]
	c.True(g.unpriced, "gpt-5.6 => %+v, want unpriced=true", g)
}

// A pricing row with prompt_usd set but completion_usd still NULL (e.g. a
// partially-synced row) must not read as priced: output_usd would silently
// compute as tokens * 0, which is the exact silent-$0 unpriced exists to catch.
func TestTurnCostPartialPricingRowFlaggedUnpriced(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool))

	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&convID), "insert conversation")
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.model_pricing (model_id, or_id, prompt_usd)
		VALUES ('half-priced-model', 'vendor/half-priced-model', 0.000005)`); err != nil {
		t.Fatalf("insert partial pricing: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request, input_tokens, output_tokens)
		VALUES ($1, 0, 'complete', 'half-priced-model', '{}'::jsonb, 1000, 100)`, convID); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	var outUSD float64
	var unpriced bool
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT output_usd, unpriced FROM conversations.v_turn WHERE conversation_id = $1`,
		convID).Scan(&outUSD, &unpriced), "select v_turn")
	c.True(unpriced, "half-priced-model => unpriced")
	c.Eq(0, outUSD, "half-priced-model => output_usd")
}

// fakePriceSource is a store.PriceSource test double. Using a fake rather than
// the real routing.ModelCatalog makes the unresolvable-model case explicit
// (any key absent from resolve/prices simply isn't found) instead of relying
// on catalog internals, and keeps this package free of a dependency on
// routing — routing already depends on store, so the reverse would cycle.
type fakePriceSource struct {
	warmed  bool
	ids     []string
	resolve map[string]string
	prices  map[string]ModelPrice
	lookups int // counts Lookup calls: the syncer must make exactly one per key
}

func (f *fakePriceSource) Warm() { f.warmed = true }

func (f *fakePriceSource) AllIDs() []string { return f.ids }

func (f *fakePriceSource) Lookup(model string) (ModelInfo, bool) {
	f.lookups++
	id, ok := f.resolve[model]
	if !ok {
		return ModelInfo{}, false
	}
	p, priced := f.prices[model]
	return ModelInfo{ORID: id, Price: p, Priced: priced}, true
}

// usd is a pointer to a cache price, for a source that prices caching. A nil
// cache field means the source does not price it at all.
func usd(v float64) *float64 { return &v }

func TestSyncModelPricingPricesCatalogAndObservedModels(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	// A turn using a model the source cannot resolve. The sync must still
	// record a row for it, with or_id NULL, so the dashboard can show it as
	// unpriced.
	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&convID), "insert conversation")
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request)
		VALUES ($1, 0, 'complete', 'claude-opus-5', '{}'::jsonb),
		       ($1, 1, 'complete', 'gpt-5.6',       '{}'::jsonb)`, convID); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	src := &fakePriceSource{
		ids: []string{"anthropic/claude-opus-5"},
		resolve: map[string]string{
			"claude-opus-5":           "anthropic/claude-opus-5",
			"anthropic/claude-opus-5": "anthropic/claude-opus-5",
			// gpt-5.6 deliberately absent: unresolvable.
		},
		prices: map[string]ModelPrice{
			"claude-opus-5":           {PromptUSD: 0.000005, CompletionUSD: 0.000025},
			"anthropic/claude-opus-5": {PromptUSD: 0.000005, CompletionUSD: 0.000025},
		},
	}

	n, err := SyncModelPricing(ctx, pool, src)
	c.Require().NoError(err, "SyncModelPricing")
	c.Require().NotEq(0, n, "SyncModelPricing reported 0 rows")
	c.True(src.warmed, "SyncModelPricing did not call Warm() on the source")

	var orID *string
	var prompt *float64
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT or_id, prompt_usd FROM conversations.model_pricing WHERE model_id = 'claude-opus-5'`,
	).Scan(&orID, &prompt), "observed priced model missing")
	c.False(orID == nil || *orID != "anthropic/claude-opus-5", "claude-opus-5 or_id = %v, want anthropic/claude-opus-5", orID)
	c.False(prompt == nil || *prompt != 0.000005, "claude-opus-5 prompt_usd = %v, want 0.000005", prompt)

	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT or_id FROM conversations.model_pricing WHERE model_id = 'gpt-5.6'`,
	).Scan(&orID), "observed unpriced model missing")
	if orID != nil {
		t.Errorf("gpt-5.6 or_id = %v, want NULL", *orID)
	}

	// The catalog id itself is also recorded, so a model_pricing lookup works
	// whether the turn stored a bare id or a slash id.
	var n2 int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.model_pricing WHERE model_id = 'anthropic/claude-opus-5'`,
	).Scan(&n2); err != nil || n2 != 1 {
		t.Errorf("catalog id row count = %d (err %v), want 1", n2, err)
	}
}

// Daily re-runs must not accumulate rows or churn history.
func TestSyncModelPricingIsIdempotent(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	src := &fakePriceSource{
		ids: []string{"anthropic/claude-opus-5"},
		resolve: map[string]string{
			"anthropic/claude-opus-5": "anthropic/claude-opus-5",
		},
		prices: map[string]ModelPrice{
			"anthropic/claude-opus-5": {PromptUSD: 0.000005, CompletionUSD: 0.000025},
		},
	}

	if _, err := SyncModelPricing(ctx, pool, src); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	var before int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_pricing`).Scan(&before), "count before")
	if _, err := SyncModelPricing(ctx, pool, src); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	var after int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_pricing`).Scan(&after), "count after")
	c.Eq(after, before, "row count changed across syncs")
}

func TestModelPricingCountTracksInserts(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	// testPool creates a fresh scratch database per test, so an absolute
	// assertion would also work — the delta is asserted anyway because it
	// tests what the function is for (reflecting inserts) rather than the
	// harness's isolation.
	before, err := ModelPricingCount(ctx, pool)
	c.Require().NoError(err, "ModelPricingCount")
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.model_pricing (model_id) VALUES ('test/count-probe')
		 ON CONFLICT (model_id) DO NOTHING`); err != nil {
		t.Fatalf("insert probe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.model_pricing WHERE model_id = 'test/count-probe'`)
	})

	after, err := ModelPricingCount(ctx, pool)
	c.Require().NoError(err, "ModelPricingCount after insert")
	c.Eq(before+1, after, "count = %d after inserting one row into %d, want", after, before)
}

// Grafana filters every panel with `col IN ($var)`, and SQL IN never matches
// NULL. A NULL dimension therefore doesn't just show up unlabelled — it drops
// out of the panel entirely, which is how 109 of 113 error turns went missing
// from "top error messages" and "stuck pending" read as structurally zero.
func TestTurnNullDimensionsReadAsSentinelNotNull(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	ck.Require().NoError(Migrate(ctx, pool))

	var convID string
	ck.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&convID), "insert conversation")
	// model, source and upstream all NULL — the shape of a real errored turn.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, request, error)
		VALUES ($1, 0, 'error', '{}'::jsonb, 'upstream status 429')`, convID); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	var model, source, upstream string
	ck.Require().NoError(pool.QueryRow(ctx, `
		SELECT model, source, upstream FROM conversations.v_turn WHERE conversation_id = $1`,
		convID).Scan(&model, &source, &upstream), "select v_turn")
	for _, c := range []struct{ col, got string }{
		{"model", model}, {"source", source}, {"upstream", upstream},
	} {
		ck.Eq("(unset)", c.got, "%s = %q, want \"(unset)\"", c.col, c.got)
	}

	// The point of the sentinel: the turn survives the dashboard's IN filter.
	var n int
	ck.Require().NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.v_turn
		WHERE conversation_id = $1 AND upstream IN ('(unset)')`, convID).Scan(&n), "count filtered")
	ck.Eq(1, n, "turn count under an IN filter")
}

// conversation_turn has no foreign key on conversation_id, and deleting a
// conversation cascades to conversation_message and conversation_analysis but
// not to the turn hypertable. An orphaned turn must still appear on the spend
// surface with its tokens: real money was spent, and an INNER JOIN made it
// vanish instead.
func TestTurnOrphanedTurnStillAppearsUnattributed(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	ck.Require().NoError(Migrate(ctx, pool))

	var turnConv string
	ck.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request, input_tokens, output_tokens)
		VALUES (gen_random_uuid(), 0, 'complete', 'claude-opus-5', '{}'::jsonb, 1000, 100)
		RETURNING conversation_id`).Scan(&turnConv), "insert orphaned turn")

	var owner, entrypoint, drivenBy string
	var in, out int64
	ck.Require().NoError(pool.QueryRow(ctx, `
		SELECT owner_username, origin_entrypoint, driven_by, input_tokens, output_tokens
		FROM conversations.v_turn WHERE conversation_id = $1`,
		turnConv).Scan(&owner, &entrypoint, &drivenBy, &in, &out), "orphaned turn missing from v_turn")
	for _, c := range []struct{ col, got string }{
		{"owner_username", owner},
		{"origin_entrypoint", entrypoint}, {"driven_by", drivenBy},
	} {
		ck.Eq("(unattributed)", c.got, "%s = %q, want \"(unattributed)\"", c.col, c.got)
	}
	ck.False(in != 1000 || out != 100, "orphaned turn tokens = %d/%d, want 1000/100", in, out)
}

// cache_saved_usd must be NULL, not 0, when the cache read price is unknown:
// with a 0 stand-in the saving computes as the cache tokens at the FULL prompt
// price. And a row is only unpriced for a missing cache price when it actually
// has cache tokens to price.
func TestTurnCacheSavingsUnknownWhenCacheUnpriced(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool))

	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&convID), "insert conversation")
	// Base prices known, cache prices NULL: a model the source doesn't cache.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.model_pricing (model_id, or_id, prompt_usd, completion_usd)
		VALUES ('no-cache-model', 'vendor/no-cache-model', 0.000005, 0.000025)`); err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request, input_tokens, output_tokens,
			 cache_read_tokens, cache_creation_tokens)
		VALUES ($1, 0, 'complete', 'no-cache-model', '{}'::jsonb, 1000, 100, 50000, 0),
		       ($1, 1, 'complete', 'no-cache-model', '{}'::jsonb, 1000, 100, 0, 0)`,
		convID); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT cache_read_tokens, cache_saved_usd, unpriced
		FROM conversations.v_turn WHERE conversation_id = $1 ORDER BY cache_read_tokens DESC`,
		convID)
	c.Require().NoError(err, "select v_turn")
	defer rows.Close()

	type row struct {
		saved    *float64
		unpriced bool
	}
	var got []row
	var tokens []int64
	for rows.Next() {
		var tok int64
		var r row
		c.Require().NoError(rows.Scan(&tok, &r.saved, &r.unpriced), "scan")
		tokens = append(tokens, tok)
		got = append(got, r)
	}
	c.Require().NoError(rows.Err(), "rows")
	c.Require().Len(got, 2, "rows = %d, want 2", len(got))

	// The turn WITH cache reads: unknown saving, and flagged unpriced.
	if got[0].saved != nil {
		t.Errorf("cache_saved_usd = %v for an unpriced cache read, want NULL "+
			"(a 0 price would report %v of savings)", *got[0].saved, float64(tokens[0])*0.000005)
	}
	c.True(got[0].unpriced, "turn with cache reads and no cache price => unpriced=false, want true")
	// The turn WITHOUT cache reads: fully priced. A model that never caches is
	// not an incomplete price.
	c.False(got[1].unpriced, "turn with no cache tokens => unpriced=true, want false: base prices are known")
}

// The syncer must write NULL, not 0, for a cache price the source doesn't
// report — the whole point of ModelPrice's pointer fields.
func TestSyncModelPricingWritesNullForAbsentCachePrice(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	src := &fakePriceSource{
		ids: []string{"vendor/no-cache-model", "vendor/cached-model"},
		resolve: map[string]string{
			"vendor/no-cache-model": "vendor/no-cache-model",
			"vendor/cached-model":   "vendor/cached-model",
		},
		prices: map[string]ModelPrice{
			// No cache prices at all.
			"vendor/no-cache-model": {PromptUSD: 0.000001, CompletionUSD: 0.000002},
			"vendor/cached-model": {
				PromptUSD: 0.000005, CompletionUSD: 0.000025,
				CacheReadUSD: usd(0.0000005), CacheWriteUSD: usd(0.00000625),
			},
		},
	}
	if _, err := SyncModelPricing(ctx, pool, src); err != nil {
		t.Fatalf("SyncModelPricing: %v", err)
	}

	var read, write *float64
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT cache_read_usd, cache_write_usd FROM conversations.model_pricing
		WHERE model_id = 'vendor/no-cache-model'`).Scan(&read, &write), "select uncached model")
	c.False(read != nil || write != nil, "absent cache prices stored as (%v, %v), want (NULL, NULL)", read, write)

	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT cache_read_usd, cache_write_usd FROM conversations.model_pricing
		WHERE model_id = 'vendor/cached-model'`).Scan(&read, &write), "select cached model")
	c.False(read == nil || *read != 0.0000005 || write == nil || *write != 0.00000625, "real cache prices stored as (%v, %v), want (5e-07, 6.25e-06)", read, write)
}

// The observed-models scan is bounded to a recent window, because an unbounded
// DISTINCT decompresses every chunk of the turn hypertable ever written. The
// union with model_pricing is what keeps that safe: a model that stopped being
// used must keep getting its price refreshed instead of silently drifting.
func TestSyncModelPricingBoundsObservedModelsButKeepsKnownOnes(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	var convID string
	c.Require().NoError(pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&convID), "insert conversation")
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request, created_at)
		VALUES ($1, 0, 'complete', 'recent-model',  '{}'::jsonb, now()),
		       ($1, 1, 'complete', 'ancient-model', '{}'::jsonb, now() - interval '90 days')`,
		convID); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	// A model already priced but no longer appearing on any recent turn.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.model_pricing (model_id) VALUES ('retired-model')`); err != nil {
		t.Fatalf("insert retired pricing row: %v", err)
	}

	src := &fakePriceSource{
		resolve: map[string]string{"retired-model": "vendor/retired-model"},
		prices:  map[string]ModelPrice{"retired-model": {PromptUSD: 0.000009, CompletionUSD: 0.00001}},
	}
	if _, err := SyncModelPricing(ctx, pool, src); err != nil {
		t.Fatalf("SyncModelPricing: %v", err)
	}

	// The 90-day-old model is outside the window and not already priced, so the
	// scan never sees it.
	var n int
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.model_pricing WHERE model_id = 'ancient-model'`,
	).Scan(&n), "count ancient")
	c.Eq(0, n, "ancient-model got a row: the observed scan is not bounded to the window")
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.model_pricing WHERE model_id = 'recent-model'`,
	).Scan(&n), "count recent")
	c.Eq(1, n, "recent-model rows")

	// The retired model is re-priced from the union arm, not dropped.
	var prompt *float64
	c.Require().NoError(pool.QueryRow(ctx, `
		SELECT prompt_usd FROM conversations.model_pricing WHERE model_id = 'retired-model'`,
	).Scan(&prompt), "select retired")
	c.False(prompt == nil || *prompt != 0.000009, "retired-model prompt_usd = %v, want 0.000009 refreshed via the union", prompt)
}

// One catalog lookup per key. Each lookup locks and scans a snapshot shared
// with the live proxy's request path, so the separate id/price/context
// accessors this replaced cost several scans per key over ~500 keys.
func TestSyncModelPricingLooksUpEachKeyOnce(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	ctx := context.Background()
	c.Require().NoError(Migrate(ctx, pool), "migrate")

	src := &fakePriceSource{
		ids: []string{"anthropic/claude-opus-5", "moonshotai/kimi-k3"},
		resolve: map[string]string{
			"anthropic/claude-opus-5": "anthropic/claude-opus-5",
			"moonshotai/kimi-k3":      "moonshotai/kimi-k3",
		},
	}
	n, err := SyncModelPricing(ctx, pool, src)
	c.Require().NoError(err, "SyncModelPricing")
	c.Require().Eq(2, n, "upserted")
	c.Eq(2, src.lookups, "Lookup called")
}
