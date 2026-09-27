// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"math"
	"testing"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// A subtree's spend is the sum of its members' spend. The test inserts two
// conversations reached by DIFFERENT correlation routes — one by conversation
// id (the in-process fundi path) and one by external_ref (the proxy path) —
// because a rollup that only follows one silently reports half the truth for
// a mixed subtree, which is the normal case.
func TestSubtreeCostSumsBothCorrelationRoutes(t *testing.T) {
	c := assert.NewAborting(t)
	pool := newTestPool(t)
	ins := New(pool).WithPricer(func(model string) (routing.ModelPricing, bool) {
		if model == "test/model" {
			// $1 per 1M input, $2 per 1M output.
			return routing.ModelPricing{PromptUSD: 1e-6, CompletionUSD: 2e-6}, true
		}
		return routing.ModelPricing{}, false
	})

	convA := insertConversation(t, pool, "server", "") // reached by id

	// convB needs external_ref set; insertConversation doesn't set it, so do
	// it manually.
	var convB string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (persona, model, origin_entrypoint, driven_by, external_ref)
		 VALUES ('team-platform', 'claude-fable-5', 'test', 'client', 'c_child2') RETURNING id::text`,
	).Scan(&convB)
	c.NoError(err, "insert external_ref conversation")

	insertTurn(t, pool, convA, seedTurn{model: "test/model", inTok: 1_000_000, outTok: 500_000})
	insertTurn(t, pool, convB, seedTurn{model: "test/model", inTok: 2_000_000})

	got, err := ins.SubtreeCost(context.Background(), SubtreeSelector{
		ConversationIDs: []string{convA},
		ExternalRefs:    []string{"c_child2"},
	})
	c.NoError(err)
	// A: 1.0 + 1.0 = 2.0 ; B: 2.0 ; total 4.0
	c.LessOrEqual(1e-9, math.Abs(got-4.0), "SubtreeCost = %f, want 4.0", got)
}

func TestSubtreeCostEmptySelectorIsZeroNotAnError(t *testing.T) {
	c := assert.NewAborting(t)
	ins := New(newTestPool(t))
	got, err := ins.SubtreeCost(context.Background(), SubtreeSelector{})
	c.NoError(err, "an empty subtree must be 0, not an error")
	c.Eq(0, got, "got")
}

// An unpriced model must not silently count as free. It counts as zero — the
// only honest option without a price — but the caller must be able to tell,
// because "the budget is fine" and "we cannot price this" are different facts.
func TestSubtreeCostReportsUnpricedModels(t *testing.T) {
	c := assert.NewAborting(t)
	pool := newTestPool(t)
	ins := New(pool).WithPricer(func(string) (routing.ModelPricing, bool) {
		return routing.ModelPricing{}, false
	})
	conv := insertConversation(t, pool, "server", "")
	insertTurn(t, pool, conv, seedTurn{model: "mystery/model", inTok: 1_000_000})

	got, unpriced, err := ins.SubtreeCostDetailed(context.Background(),
		SubtreeSelector{ConversationIDs: []string{conv}})
	c.NoError(err)
	c.Eq(0, got, "an unpriced model must contribute 0, got")
	c.False(len(unpriced) != 1 || unpriced[0] != "mystery/model", "the caller must learn which models could not be priced; got %v", unpriced)
}

// CostsByConversation is SubtreeCost's per-row form: same two correlation
// routes, but each conversation priced separately so a LIST can be filled in
// one round trip instead of one per child.
func TestCostsByConversationPricesEachRouteSeparately(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	ins := New(pool).WithPricer(func(model string) (routing.ModelPricing, bool) {
		if model == "test/model" {
			return routing.ModelPricing{PromptUSD: 1e-6, CompletionUSD: 2e-6}, true
		}
		return routing.ModelPricing{}, false
	})

	convA := insertConversation(t, pool, "server", "")
	var convB string
	c.Require().NoError(pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (persona, model, origin_entrypoint, driven_by, external_ref)
		 VALUES ('team-platform', 'claude-fable-5', 'test', 'client', 'c_child2') RETURNING id::text`,
	).Scan(&convB), "insert external_ref conversation")

	insertTurn(t, pool, convA, seedTurn{model: "test/model", inTok: 1_000_000, outTok: 500_000})
	insertTurn(t, pool, convB, seedTurn{model: "test/model", inTok: 2_000_000})

	rows, err := ins.CostsByConversation(context.Background(), SubtreeSelector{
		ConversationIDs: []string{convA},
		ExternalRefs:    []string{"c_child2"},
	})
	c.Require().NoError(err)
	c.Require().Len(rows, 2, "got %d rows, want 2", len(rows))
	byID := map[string]ConversationCost{}
	for _, r := range rows {
		byID[r.ConversationID] = r
	}
	// A is 1.0 input + 1.0 output; B is 2.0 input. SubtreeCost collapses these
	// to 4.0; the point here is that they stay apart.
	if got := byID[convA].Cost; math.Abs(got-2.0) > 1e-9 {
		t.Errorf("convA = %f, want 2.0", got)
	}
	if got := byID[convB].Cost; math.Abs(got-2.0) > 1e-9 {
		t.Errorf("convB = %f, want 2.0", got)
	}
	c.Eq("c_child2", byID[convB].ExternalRef, "convB external_ref")
}

// $1::uuid[] rejects the whole ARRAY on one malformed element, so a single
// non-UUID session id would fail every child's cost in the batch rather than
// just its own. That risk is created by batching; the per-child form could
// only ever poison one row.
func TestCostsByConversationSurvivesANonUUIDSessionID(t *testing.T) {
	c := assert.NewAborting(t)
	pool := newTestPool(t)
	ins := New(pool).WithPricer(func(string) (routing.ModelPricing, bool) {
		return routing.ModelPricing{PromptUSD: 1e-6}, true
	})

	convA := insertConversation(t, pool, "server", "")
	insertTurn(t, pool, convA, seedTurn{model: "test/model", inTok: 1_000_000})

	rows, err := ins.CostsByConversation(context.Background(), SubtreeSelector{
		// A pi session id is not a UUID. controller.go warns about handing one
		// to a query expecting one; here it must simply not match.
		ConversationIDs: []string{convA, "sess_not_a_uuid"},
	})
	c.NoError(err, "one malformed id must not fail the batch")
	c.False(len(rows) != 1 || math.Abs(rows[0].Cost-1.0) > 1e-9, "got %+v, want one row costing 1.0", rows)
}
