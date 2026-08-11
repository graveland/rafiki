// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"math"
	"testing"

	"go.graveland.dev/rafiki/pkg/routing"
)

// A subtree's spend is the sum of its members' spend. The test inserts two
// conversations reached by DIFFERENT correlation routes — one by conversation
// id (the in-process fundi path) and one by external_ref (the proxy path) —
// because a rollup that only follows one silently reports half the truth for
// a mixed subtree, which is the normal case.
func TestSubtreeCostSumsBothCorrelationRoutes(t *testing.T) {
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
	if err != nil {
		t.Fatalf("insert external_ref conversation: %v", err)
	}

	insertTurn(t, pool, convA, seedTurn{model: "test/model", inTok: 1_000_000, outTok: 500_000})
	insertTurn(t, pool, convB, seedTurn{model: "test/model", inTok: 2_000_000})

	got, err := ins.SubtreeCost(context.Background(), SubtreeSelector{
		ConversationIDs: []string{convA},
		ExternalRefs:    []string{"c_child2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A: 1.0 + 1.0 = 2.0 ; B: 2.0 ; total 4.0
	if math.Abs(got-4.0) > 1e-9 {
		t.Fatalf("SubtreeCost = %f, want 4.0", got)
	}
}

func TestSubtreeCostEmptySelectorIsZeroNotAnError(t *testing.T) {
	ins := New(newTestPool(t))
	got, err := ins.SubtreeCost(context.Background(), SubtreeSelector{})
	if err != nil {
		t.Fatalf("an empty subtree must be 0, not an error: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %f", got)
	}
}

// An unpriced model must not silently count as free. It counts as zero — the
// only honest option without a price — but the caller must be able to tell,
// because "the budget is fine" and "we cannot price this" are different facts.
func TestSubtreeCostReportsUnpricedModels(t *testing.T) {
	pool := newTestPool(t)
	ins := New(pool).WithPricer(func(string) (routing.ModelPricing, bool) {
		return routing.ModelPricing{}, false
	})
	conv := insertConversation(t, pool, "server", "")
	insertTurn(t, pool, conv, seedTurn{model: "mystery/model", inTok: 1_000_000})

	got, unpriced, err := ins.SubtreeCostDetailed(context.Background(),
		SubtreeSelector{ConversationIDs: []string{conv}})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("an unpriced model must contribute 0, got %f", got)
	}
	if len(unpriced) != 1 || unpriced[0] != "mystery/model" {
		t.Fatalf("the caller must learn which models could not be priced; got %v", unpriced)
	}
}
