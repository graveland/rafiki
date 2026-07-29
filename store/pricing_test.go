package store

import (
	"context"
	"testing"
)

// The views are the entire FDW surface. If a JSONB payload column ever leaks
// into one, conversation content becomes readable from a downstream grafana DB,
// which every Grafana user can query. This test is that boundary.
func TestViewsExcludePayloadColumns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

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
			if err != nil {
				t.Fatalf("query columns of %s: %v", view, err)
			}
			if n != 0 {
				t.Errorf("%s exposes payload column %q over the FDW", view, col)
			}
		}
	}
}

func TestOwnerCanonicalRewritesDashDomain(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	// kubecfg-svcaccount is deliberately NOT mapped: the real account is
	// svc@example.com, so the obvious rewrite would be wrong. Only the
	// dash-for-at typo is safe to correct. Its kind is "service", not "human":
	// it is a kubeconfig credential, and counting it as a person inflated the
	// distinct-active-owners adoption metric.
	cases := []struct{ owner, want, wantKind string }{
		{"dev-example.com", "dev@example.com", "human"},
		{"dev@example.com", "dev@example.com", "human"},
		{"kubecfg-svcaccount", "kubecfg-svcaccount", "service"},
		{"system:diagnose", "system:diagnose", "system"},
		// Neither system:-prefixed nor an email address → a machine identity.
		{"ci-runner", "ci-runner", "service"},
	}
	for _, c := range cases {
		var id string
		if err := pool.QueryRow(ctx, `
			INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
			VALUES ($1, 'claude', 'client') RETURNING id`, c.owner).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", c.owner, err)
		}
		var got, gotKind string
		if err := pool.QueryRow(ctx,
			`SELECT owner_canonical, owner_kind FROM conversations.v_conversation WHERE id = $1`,
			id).Scan(&got, &gotKind); err != nil {
			t.Fatalf("select %q: %v", c.owner, err)
		}
		if got != c.want || gotKind != c.wantKind {
			t.Errorf("owner %q => (%q, %q), want (%q, %q)", c.owner, got, gotKind, c.want, c.wantKind)
		}
	}
}

// owner is nullable ("nullable for now" per the baseline migration). A NULL
// owner must read as an unknown owner_kind, never as "human" — asserting
// something we do not know.
func TestOwnerCanonicalNullOwnerIsUnknownKind(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('claude', 'client') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert null-owner conversation: %v", err)
	}
	var canonical, kind *string
	if err := pool.QueryRow(ctx,
		`SELECT owner_canonical, owner_kind FROM conversations.v_conversation WHERE id = $1`,
		id).Scan(&canonical, &kind); err != nil {
		t.Fatalf("select null-owner row: %v", err)
	}
	if canonical != nil {
		t.Errorf("owner_canonical = %q, want NULL for a NULL owner", *canonical)
	}
	if kind != nil {
		t.Errorf("owner_kind = %q, want NULL for a NULL owner, not a guessed \"human\"", *kind)
	}
}

// An unpriced model must read as "unpriced", never as $0 — a silent zero in a
// spend dashboard is worse than a visible gap.
func TestTurnCostUnpricedModelFlagged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
		VALUES ('dev@example.com', 'claude', 'client') RETURNING id`).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
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
	if err != nil {
		t.Fatalf("select v_turn: %v", err)
	}
	defer rows.Close()

	got := map[string]struct {
		in, out  float64
		unpriced bool
	}{}
	for rows.Next() {
		var m string
		var in, out float64
		var un bool
		if err := rows.Scan(&m, &in, &out, &un); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[m] = struct {
			in, out  float64
			unpriced bool
		}{in, out, un}
	}
	if g := got["claude-opus-5"]; g.unpriced || g.in != 0.005 || g.out != 0.0025 {
		t.Errorf("claude-opus-5 => %+v, want in=0.005 out=0.0025 unpriced=false", g)
	}
	if g := got["gpt-5.6"]; !g.unpriced {
		t.Errorf("gpt-5.6 => %+v, want unpriced=true", g)
	}
}

// A pricing row with prompt_usd set but completion_usd still NULL (e.g. a
// partially-synced row) must not read as priced: output_usd would silently
// compute as tokens * 0, which is the exact silent-$0 unpriced exists to catch.
func TestTurnCostPartialPricingRowFlaggedUnpriced(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
		VALUES ('dev@example.com', 'claude', 'client') RETURNING id`).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
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
	if err := pool.QueryRow(ctx, `
		SELECT output_usd, unpriced FROM conversations.v_turn WHERE conversation_id = $1`,
		convID).Scan(&outUSD, &unpriced); err != nil {
		t.Fatalf("select v_turn: %v", err)
	}
	if !unpriced {
		t.Errorf("half-priced-model => unpriced=%v, want true: completion_usd is NULL", unpriced)
	}
	if outUSD != 0 {
		t.Errorf("half-priced-model => output_usd=%v, want 0 (coalesced), but unpriced flag must still be true", outUSD)
	}
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
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A turn using a model the source cannot resolve. The sync must still
	// record a row for it, with or_id NULL, so the dashboard can show it as
	// unpriced.
	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
		VALUES ('dev@example.com', 'claude', 'client') RETURNING id`).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
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
	if err != nil {
		t.Fatalf("SyncModelPricing: %v", err)
	}
	if n == 0 {
		t.Fatal("SyncModelPricing reported 0 rows")
	}
	if !src.warmed {
		t.Error("SyncModelPricing did not call Warm() on the source")
	}

	var orID *string
	var prompt *float64
	if err := pool.QueryRow(ctx,
		`SELECT or_id, prompt_usd FROM conversations.model_pricing WHERE model_id = 'claude-opus-5'`,
	).Scan(&orID, &prompt); err != nil {
		t.Fatalf("observed priced model missing: %v", err)
	}
	if orID == nil || *orID != "anthropic/claude-opus-5" {
		t.Errorf("claude-opus-5 or_id = %v, want anthropic/claude-opus-5", orID)
	}
	if prompt == nil || *prompt != 0.000005 {
		t.Errorf("claude-opus-5 prompt_usd = %v, want 0.000005", prompt)
	}

	if err := pool.QueryRow(ctx,
		`SELECT or_id FROM conversations.model_pricing WHERE model_id = 'gpt-5.6'`,
	).Scan(&orID); err != nil {
		t.Fatalf("observed unpriced model missing: %v", err)
	}
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
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_pricing`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if _, err := SyncModelPricing(ctx, pool, src); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.model_pricing`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if before != after {
		t.Errorf("row count changed across syncs: %d -> %d", before, after)
	}
}

func TestModelPricingCountTracksInserts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// testPool creates a fresh scratch database per test, so an absolute
	// assertion would also work — the delta is asserted anyway because it
	// tests what the function is for (reflecting inserts) rather than the
	// harness's isolation.
	before, err := ModelPricingCount(ctx, pool)
	if err != nil {
		t.Fatalf("ModelPricingCount: %v", err)
	}
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
	if err != nil {
		t.Fatalf("ModelPricingCount after insert: %v", err)
	}
	if after != before+1 {
		t.Errorf("count = %d after inserting one row into %d, want %d", after, before, before+1)
	}
}

// Grafana filters every panel with `col IN ($var)`, and SQL IN never matches
// NULL. A NULL dimension therefore doesn't just show up unlabelled — it drops
// out of the panel entirely, which is how 109 of 113 error turns went missing
// from "top error messages" and "stuck pending" read as structurally zero.
func TestTurnNullDimensionsReadAsSentinelNotNull(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
		VALUES ('dev@example.com', 'claude', 'client') RETURNING id`).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	// model, source and upstream all NULL — the shape of a real errored turn.
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, request, error)
		VALUES ($1, 0, 'error', '{}'::jsonb, 'upstream status 429')`, convID); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	var model, source, upstream string
	if err := pool.QueryRow(ctx, `
		SELECT model, source, upstream FROM conversations.v_turn WHERE conversation_id = $1`,
		convID).Scan(&model, &source, &upstream); err != nil {
		t.Fatalf("select v_turn: %v", err)
	}
	for _, c := range []struct{ col, got string }{
		{"model", model}, {"source", source}, {"upstream", upstream},
	} {
		if c.got != "(unset)" {
			t.Errorf("%s = %q, want \"(unset)\"", c.col, c.got)
		}
	}

	// The point of the sentinel: the turn survives the dashboard's IN filter.
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.v_turn
		WHERE conversation_id = $1 AND upstream IN ('(unset)')`, convID).Scan(&n); err != nil {
		t.Fatalf("count filtered: %v", err)
	}
	if n != 1 {
		t.Errorf("turn count under an IN filter = %d, want 1", n)
	}
}

// conversation_turn has no foreign key on conversation_id, and deleting a
// conversation cascades to conversation_message and conversation_analysis but
// not to the turn hypertable. An orphaned turn must still appear on the spend
// surface with its tokens: real money was spent, and an INNER JOIN made it
// vanish instead.
func TestTurnOrphanedTurnStillAppearsUnattributed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var turnConv string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation_turn
			(conversation_id, ordinal, status, model, request, input_tokens, output_tokens)
		VALUES (gen_random_uuid(), 0, 'complete', 'claude-opus-5', '{}'::jsonb, 1000, 100)
		RETURNING conversation_id`).Scan(&turnConv); err != nil {
		t.Fatalf("insert orphaned turn: %v", err)
	}

	var owner, ownerKind, entrypoint, drivenBy string
	var in, out int64
	if err := pool.QueryRow(ctx, `
		SELECT owner_canonical, owner_kind, origin_entrypoint, driven_by, input_tokens, output_tokens
		FROM conversations.v_turn WHERE conversation_id = $1`,
		turnConv).Scan(&owner, &ownerKind, &entrypoint, &drivenBy, &in, &out); err != nil {
		t.Fatalf("orphaned turn missing from v_turn: %v", err)
	}
	for _, c := range []struct{ col, got string }{
		{"owner_canonical", owner}, {"owner_kind", ownerKind},
		{"origin_entrypoint", entrypoint}, {"driven_by", drivenBy},
	} {
		if c.got != "(unattributed)" {
			t.Errorf("%s = %q, want \"(unattributed)\"", c.col, c.got)
		}
	}
	if in != 1000 || out != 100 {
		t.Errorf("orphaned turn tokens = %d/%d, want 1000/100", in, out)
	}
}

// cache_saved_usd must be NULL, not 0, when the cache read price is unknown:
// with a 0 stand-in the saving computes as the cache tokens at the FULL prompt
// price. And a row is only unpriced for a missing cache price when it actually
// has cache tokens to price.
func TestTurnCacheSavingsUnknownWhenCacheUnpriced(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
		VALUES ('dev@example.com', 'claude', 'client') RETURNING id`).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
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
	if err != nil {
		t.Fatalf("select v_turn: %v", err)
	}
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
		if err := rows.Scan(&tok, &r.saved, &r.unpriced); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tokens = append(tokens, tok)
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}

	// The turn WITH cache reads: unknown saving, and flagged unpriced.
	if got[0].saved != nil {
		t.Errorf("cache_saved_usd = %v for an unpriced cache read, want NULL "+
			"(a 0 price would report %v of savings)", *got[0].saved, float64(tokens[0])*0.000005)
	}
	if !got[0].unpriced {
		t.Error("turn with cache reads and no cache price => unpriced=false, want true")
	}
	// The turn WITHOUT cache reads: fully priced. A model that never caches is
	// not an incomplete price.
	if got[1].unpriced {
		t.Error("turn with no cache tokens => unpriced=true, want false: base prices are known")
	}
}

// The syncer must write NULL, not 0, for a cache price the source doesn't
// report — the whole point of ModelPrice's pointer fields.
func TestSyncModelPricingWritesNullForAbsentCachePrice(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

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
	if err := pool.QueryRow(ctx, `
		SELECT cache_read_usd, cache_write_usd FROM conversations.model_pricing
		WHERE model_id = 'vendor/no-cache-model'`).Scan(&read, &write); err != nil {
		t.Fatalf("select uncached model: %v", err)
	}
	if read != nil || write != nil {
		t.Errorf("absent cache prices stored as (%v, %v), want (NULL, NULL)", read, write)
	}

	if err := pool.QueryRow(ctx, `
		SELECT cache_read_usd, cache_write_usd FROM conversations.model_pricing
		WHERE model_id = 'vendor/cached-model'`).Scan(&read, &write); err != nil {
		t.Fatalf("select cached model: %v", err)
	}
	if read == nil || *read != 0.0000005 || write == nil || *write != 0.00000625 {
		t.Errorf("real cache prices stored as (%v, %v), want (5e-07, 6.25e-06)", read, write)
	}
}

// The observed-models scan is bounded to a recent window, because an unbounded
// DISTINCT decompresses every chunk of the turn hypertable ever written. The
// union with model_pricing is what keeps that safe: a model that stopped being
// used must keep getting its price refreshed instead of silently drifting.
func TestSyncModelPricingBoundsObservedModelsButKeepsKnownOnes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation (owner, origin_entrypoint, driven_by)
		VALUES ('dev@example.com', 'claude', 'client') RETURNING id`).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
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
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.model_pricing WHERE model_id = 'ancient-model'`,
	).Scan(&n); err != nil {
		t.Fatalf("count ancient: %v", err)
	}
	if n != 0 {
		t.Errorf("ancient-model got a row: the observed scan is not bounded to the window")
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.model_pricing WHERE model_id = 'recent-model'`,
	).Scan(&n); err != nil {
		t.Fatalf("count recent: %v", err)
	}
	if n != 1 {
		t.Errorf("recent-model rows = %d, want 1", n)
	}

	// The retired model is re-priced from the union arm, not dropped.
	var prompt *float64
	if err := pool.QueryRow(ctx, `
		SELECT prompt_usd FROM conversations.model_pricing WHERE model_id = 'retired-model'`,
	).Scan(&prompt); err != nil {
		t.Fatalf("select retired: %v", err)
	}
	if prompt == nil || *prompt != 0.000009 {
		t.Errorf("retired-model prompt_usd = %v, want 0.000009 refreshed via the union", prompt)
	}
}

// One catalog lookup per key. Each lookup locks and scans a snapshot shared
// with the live proxy's request path, so the separate id/price/context
// accessors this replaced cost several scans per key over ~500 keys.
func TestSyncModelPricingLooksUpEachKeyOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	src := &fakePriceSource{
		ids: []string{"anthropic/claude-opus-5", "moonshotai/kimi-k3"},
		resolve: map[string]string{
			"anthropic/claude-opus-5": "anthropic/claude-opus-5",
			"moonshotai/kimi-k3":      "moonshotai/kimi-k3",
		},
	}
	n, err := SyncModelPricing(ctx, pool, src)
	if err != nil {
		t.Fatalf("SyncModelPricing: %v", err)
	}
	if n != 2 {
		t.Fatalf("upserted %d rows, want 2", n)
	}
	if src.lookups != 2 {
		t.Errorf("Lookup called %d times for 2 keys, want 2", src.lookups)
	}
}
