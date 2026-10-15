// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// spendCellString reads a StringEntry cell out of a spend query row.
func spendCellString(t *testing.T, e Entry) string {
	t.Helper()
	v, ok := e.(StringEntry)
	assert.NewAborting(t).True(ok, "spend cell is %T, want StringEntry", e)
	return string(v)
}

// spendCellInt reads an IntEntry cell out of a spend query row.
func spendCellInt(t *testing.T, e Entry) int64 {
	t.Helper()
	v, ok := e.(IntEntry)
	assert.NewAborting(t).True(ok, "spend cell is %T, want IntEntry", e)
	return int64(v)
}

// spendCellFloat reads a FloatEntry cell out of a spend query row.
func spendCellFloat(t *testing.T, e Entry) float64 {
	t.Helper()
	v, ok := e.(FloatEntry)
	assert.NewAborting(t).True(ok, "spend cell is %T, want FloatEntry", e)
	return float64(v)
}

// spendKey identifies one spend row: its UTC day, model and served provider.
type spendKey struct{ day, model, provider string }

// spendRows indexes a spend result by (day, model, provider) so tests match
// rows by identity rather than position.
func spendRows(t *testing.T, got QueryResult) map[spendKey][]Entry {
	t.Helper()
	out := map[spendKey][]Entry{}
	for _, row := range got.Rows {
		out[spendKey{spendCellString(t, row[0]), spendCellString(t, row[1]), spendCellString(t, row[2])}] = row
	}
	return out
}

// TestQuerySpendBucketsByUTCDayModelProvider seeds turns across two UTC days,
// two models and two providers and expects them bucketed by (day, model,
// provider) with the token sums per bucket. One turn is seeded at
// 2026-10-01T23:30:00-05:00 -- 04:30 UTC on 2026-10-02 -- and must land in the
// 2026-10-02 bucket, not the 2026-10-01 one.
func TestQuerySpendBucketsByUTCDayModelProvider(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "spend-alice")

	// Three turns on 2026-10-01 UTC, m-a / p1.
	for i := 0; i < 3; i++ {
		insertTurn(t, pool, convID, seedTurn{
			ordinal: i, model: "m-a", servedProvider: "p1",
			inTok: 100, outTok: 10, cacheRead: 5, cacheCreate: 1,
			createdAt: time.Date(2026, 10, 1, 3+i, 0, 0, 0, time.UTC),
		})
	}
	// One on 2026-10-02 UTC, m-a / p1.
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 3, model: "m-a", servedProvider: "p1",
		inTok: 7, outTok: 1, cacheRead: 0, cacheCreate: 0,
		createdAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
	})
	// One on 2026-10-01 in -05:00, which is 2026-10-02 04:30 UTC.
	minus5 := time.FixedZone("-05", -5*3600)
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 4, model: "m-a", servedProvider: "p1",
		inTok: 11, outTok: 2, cacheRead: 0, cacheCreate: 0,
		createdAt: time.Date(2026, 10, 1, 23, 30, 0, 0, minus5),
	})
	// One on 2026-10-01 UTC, m-b / p2.
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 5, model: "m-b", servedProvider: "p2",
		inTok: 50, outTok: 5, cacheRead: 2, cacheCreate: 3,
		createdAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	})

	got, err := New(pool).Query(ctx, ScopeAll(), "spend", StatsFilter{})
	c.Require().NoError(err, "query spend")
	c.Require().Len(got.Rows, 3, "spend query returned %d rows, want 3", len(got.Rows))
	byKey := spendRows(t, got)

	rowA := byKey[spendKey{"2026-10-01", "m-a", "p1"}]
	if rowA == nil {
		t.Fatalf("no row for 2026-10-01/m-a/p1; got %v", byKey)
	}
	c.Eq(int64(3), spendCellInt(t, rowA[3]), "2026-10-01 m-a turns")
	c.Eq(int64(300), spendCellInt(t, rowA[4]), "2026-10-01 m-a input")
	c.Eq(int64(30), spendCellInt(t, rowA[5]), "2026-10-01 m-a output")
	c.Eq(int64(15), spendCellInt(t, rowA[6]), "2026-10-01 m-a cache_read")
	c.Eq(int64(3), spendCellInt(t, rowA[7]), "2026-10-01 m-a cache_creation")

	rowB := byKey[spendKey{"2026-10-02", "m-a", "p1"}]
	if rowB == nil {
		t.Fatalf("no row for 2026-10-02/m-a/p1 (the -05:00 turn must land here); got %v", byKey)
	}
	c.Eq(int64(2), spendCellInt(t, rowB[3]), "2026-10-02 m-a turns")
	c.Eq(int64(18), spendCellInt(t, rowB[4]), "2026-10-02 m-a input")

	rowC := byKey[spendKey{"2026-10-01", "m-b", "p2"}]
	if rowC == nil {
		t.Fatalf("no row for 2026-10-01/m-b/p2; got %v", byKey)
	}
	c.Eq(int64(1), spendCellInt(t, rowC[3]), "2026-10-01 m-b turns")
	c.Eq(int64(50), spendCellInt(t, rowC[4]), "2026-10-01 m-b input")
}

// spendFakePricer prices only m-a, mirroring OR per-token USD.
func spendFakePricer(model string) (routing.ModelPricing, bool) {
	if model != "m-a" {
		return routing.ModelPricing{}, false
	}
	return routing.ModelPricing{
		PromptUSD: 0.000002, CompletionUSD: 0.00001,
		CacheReadUSD: 0.0000002, CacheWriteUSD: 0.0000025,
	}, true
}

// TestQuerySpendPricesWithPricer seeds an m-a row and an m-b row and prices
// with a fake that resolves only m-a: the m-a row carries priced=1 and the
// CostOf result, the m-b row priced=0 and 0 cost.
func TestQuerySpendPricesWithPricer(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "spend-grace")
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 0, model: "m-a", servedProvider: "p1",
		inTok: 1000, outTok: 200, cacheRead: 5000, cacheCreate: 300,
		createdAt: time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC),
	})
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 1, model: "m-b", servedProvider: "p2",
		inTok: 100, outTok: 20,
		createdAt: time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC),
	})

	got, err := New(pool).WithPricer(spendFakePricer).Query(ctx, ScopeAll(), "spend", StatsFilter{})
	c.Require().NoError(err, "query spend")
	c.Require().Len(got.Rows, 2, "spend query returned %d rows, want 2", len(got.Rows))
	byKey := spendRows(t, got)

	rowA := byKey[spendKey{"2026-10-01", "m-a", "p1"}]
	if rowA == nil {
		t.Fatalf("no m-a row; got %v", byKey)
	}
	c.Eq(int64(1), spendCellInt(t, rowA[9]), "m-a priced flag")
	wantA := routing.ModelPricing{
		PromptUSD: 0.000002, CompletionUSD: 0.00001,
		CacheReadUSD: 0.0000002, CacheWriteUSD: 0.0000025,
	}.CostOf(1000, 200, 5000, 300).Total
	if !inDelta(spendCellFloat(t, rowA[8]), wantA, 1e-9) {
		t.Errorf("m-a cost_usd = %g, want %g", spendCellFloat(t, rowA[8]), wantA)
	}

	rowB := byKey[spendKey{"2026-10-01", "m-b", "p2"}]
	if rowB == nil {
		t.Fatalf("no m-b row; got %v", byKey)
	}
	c.Eq(int64(0), spendCellInt(t, rowB[9]), "m-b priced flag")
	c.Eq(0.0, spendCellFloat(t, rowB[8]), "m-b cost_usd")
}

// TestQuerySpendUnpricedWithoutPricer pins that with no pricer every row is
// unpriced (priced=0, cost_usd=0), never a spurious $0 that reads as "free".
func TestQuerySpendUnpricedWithoutPricer(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "spend-heidi")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "m-a", servedProvider: "p1", inTok: 1000, outTok: 200})
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "m-b", servedProvider: "p2", inTok: 10, outTok: 1})

	got, err := New(pool).Query(ctx, ScopeAll(), "spend", StatsFilter{})
	c.Require().NoError(err, "query spend")
	c.Require().Len(got.Rows, 2, "spend query returned %d rows, want 2", len(got.Rows))
	for _, row := range got.Rows {
		c.Eq(int64(0), spendCellInt(t, row[9]), "priced flag")
		c.Eq(0.0, spendCellFloat(t, row[8]), "cost_usd")
	}
}

// TestQuerySpendScopeOwnerExcludesOtherOwners is the standard scope-owner
// shape: a ScopeOwner(bob) spend query over conversations owned by bob and
// carol yields exactly bob's row.
func TestQuerySpendScopeOwnerExcludesOtherOwners(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	bobConv := insertConversation(t, pool, "client", "spend-bob")
	insertTurn(t, pool, bobConv, seedTurn{ordinal: 0, model: "scope-model-bob", servedProvider: "p1"})
	carolConv := insertConversation(t, pool, "client", "spend-carol")
	insertTurn(t, pool, carolConv, seedTurn{ordinal: 0, model: "scope-model-carol", servedProvider: "p1"})

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "spend-bob")), "spend", StatsFilter{})
	c.Require().NoError(err, "query spend scoped to bob")
	c.Require().Len(got.Rows, 1, "scoped spend query returned %d rows, want 1 (bob's only)", len(got.Rows))
	c.Eq("scope-model-bob", spendCellString(t, got.Rows[0][1]), "model")
}

// TestQuerySpendSubtreeScope is the child-credential boundary: two
// conversations, a selector naming only the first, and only the first's rows
// come back.
func TestQuerySpendSubtreeScope(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	first := insertConversation(t, pool, "client", "spend-subtree")
	insertTurn(t, pool, first, seedTurn{ordinal: 0, model: "subtree-first", servedProvider: "p1"})
	second := insertConversation(t, pool, "client", "spend-subtree")
	insertTurn(t, pool, second, seedTurn{ordinal: 0, model: "subtree-second", servedProvider: "p1"})

	got, err := New(pool).Query(ctx, ScopeSubtree(SubtreeSelector{ConversationIDs: []string{first}}), "spend", StatsFilter{})
	c.Require().NoError(err, "query spend in subtree")
	c.Require().Len(got.Rows, 1, "subtree spend query returned %d rows, want 1 (first conversation only)", len(got.Rows))
	c.Eq("subtree-first", spendCellString(t, got.Rows[0][1]), "model")
}

// TestQuerySpendModelFilter pins that f.Model narrows to the requested model.
func TestQuerySpendModelFilter(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "spend-filter")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "m-a", servedProvider: "p1"})
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "m-b", servedProvider: "p1"})

	got, err := New(pool).Query(ctx, ScopeAll(), "spend", StatsFilter{Model: "m-b"})
	c.Require().NoError(err, "query spend filtered")
	c.Require().Len(got.Rows, 1, "filtered spend query returned %d rows, want 1", len(got.Rows))
	c.Eq("m-b", spendCellString(t, got.Rows[0][1]), "model")
}

// TestQuerySpendNullModelIsARow pins that a turn whose model is NULL (an
// unpriced/unknown model) is a row with model "", never dropped: dropping it
// would hide spend, the exact failure a budget can't afford.
func TestQuerySpendNullModelIsARow(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "spend-null")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "", servedProvider: "p1", inTok: 40, outTok: 4})

	got, err := New(pool).Query(ctx, ScopeAll(), "spend", StatsFilter{})
	c.Require().NoError(err, "query spend")
	c.Require().Len(got.Rows, 1, "spend query returned %d rows, want 1 (a NULL-model turn is a row)", len(got.Rows))
	c.Eq("", spendCellString(t, got.Rows[0][1]), "model")
	c.Eq(int64(40), spendCellInt(t, got.Rows[0][4]), "input")
}
