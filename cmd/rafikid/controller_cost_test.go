// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// responsesProvidersTOML configures one Responses-kind provider ("oa") beside
// the shipped defaults, so a bare OpenAI id is the shape CatalogModelID
// rewrites and an Anthropic id is the shape it must leave alone.
const responsesProvidersTOML = `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"
`

// seedResponsesCatalog seeds the catalog entries this file prices against: the
// Responses provider's model under the id the catalog actually indexes it
// under ("openai/gpt-6.1-sol"), an Anthropic entry, and a 10x decoy under the
// openai/ namespace so a mapping that widened to an existing kind (or a
// candidate offered before the raw id) would be visible as a wrong number
// rather than a missing one.
func seedResponsesCatalog() *routing.ModelCatalog {
	c := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]routing.CatalogEntry{
		{ID: "openai/gpt-6.1-sol", Created: 1, Pricing: &routing.ModelPricing{
			PromptUSD: 0.000003, CompletionUSD: 0.000015,
		}},
		{ID: "anthropic/claude-sonnet-5", Created: 2, Pricing: &routing.ModelPricing{
			PromptUSD: 0.000003, CompletionUSD: 0.000015,
		}},
		{ID: "openai/claude-sonnet-5", Created: 3, Pricing: &routing.ModelPricing{
			PromptUSD: 0.00003, CompletionUSD: 0.00015,
		}},
	})
	return c
}

// TestCatalogPricerPricesResponsesModelByCatalogID pins the pricer both of
// SetCatalog's injection sites now use. A turn on a Responses-kind provider is
// recorded under its PROVIDER-LOCAL id ("gpt-6.1-sol") while the catalog
// indexes "openai/gpt-6.1-sol", so pricing the raw string misses, and a miss
// is the same number as a free turn, which is how a ChatGPT-billed child's
// spend became invisible to every budget that reads it.
func TestCatalogPricerPricesResponsesModelByCatalogID(t *testing.T) {
	c := assert.NewCollecting(t)
	cat := seedResponsesCatalog()
	set := mustParseProviders(t, responsesProvidersTOML)

	// 1M input on gpt-6.1-sol = $3.00, the openai/gpt-6.1-sol entry.
	price, ok := catalogPricer(cat, set)("gpt-6.1-sol")
	c.Require().True(ok, "catalogPricer missed a Responses-kind model id")
	c.Eq(3.0, price.CostOf(1_000_000, 0, 0, 0).Total, "priced cost")

	// The same configuration must still price an Anthropic id from the
	// anthropic/ entry: $15.00, never the $150.00 openai/ decoy.
	price, ok = catalogPricer(cat, set)("claude-sonnet-5")
	c.Require().True(ok, "catalogPricer missed an Anthropic model id")
	c.Eq(15.0, price.CostOf(0, 1_000_000, 0, 0).Total, "Anthropic cost is unchanged")

	// No Responses kind configured: nothing is invented. This is the
	// no-widening rule for every existing providers.toml.
	if _, ok := catalogPricer(cat, providers.Default())("gpt-6.1-sol"); ok {
		t.Error("a set with no Responses kind resolved a Responses-kind id; the mapping must only offer shapes the configuration can produce")
	}

	// A nil set leaves the raw lookup alone (and a nil catalog leaves the
	// pricer nil, which local.New and WithPricer both treat as unpriced).
	if _, ok := catalogPricer(cat, nil)("gpt-6.1-sol"); ok {
		t.Error("a nil provider set must not rewrite model ids")
	}
	if p := catalogPricer(nil, set); p != nil {
		t.Error("catalogPricer(nil, set) is not nil; an absent catalog must stay unpriced")
	}
}

// TestListChildrenPricesResponsesModelByCatalogID pins the WIRING, not the
// helper: a controller built the way main.go builds one (NewController with the
// provider registry, then SetCatalog) reports a real cost for a child whose
// captured turns record a Responses-kind provider's bare model id.
//
// Mutation-checked: pricing the raw id (cat.Pricing, the pre-fix wiring) leaves
// "gpt-6.1-sol" unresolvable, CostsByConversation short-circuits to no rows,
// and CostUSD comes back 0, which the cockpit adopts as "no cost known".
func TestListChildrenPricesResponsesModelByCatalogID(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	// Unique per run: openTestPool shares the RAFIKI_TEST_DSN database with every
	// other run, and conversation.external_ref is globally unique.
	extRef := fmt.Sprintf("c_responses_%d", time.Now().UnixNano())
	convID := insertConvForCost(t, pool, extRef)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation_turn WHERE conversation_id = $1::uuid`, convID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = $1::uuid`, convID)
	})

	// The row as capture writes it: the provider-LOCAL id, never the catalog's.
	insertTurnForCost(t, pool, convID, "gpt-6.1-sol", 1_000_000, 500_000)

	st := childstore.New()
	dir := t.TempDir()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, pool, nil, false, ctx, nil, nil, nil,
		mustParseProviders(t, responsesProvidersTOML))
	ctrl.SetCatalog(seedResponsesCatalog())

	st.Insert(&childstore.Session{ChildID: extRef, Status: protocol.StatusExited})

	var got *float64
	for _, s := range ctrl.ListChildren(nil) {
		if s.ChildID == extRef {
			got = s.CostUSD
		}
	}
	if got == nil {
		t.Fatal("CostUSD is nil: a Responses-kind model priced by its raw id resolves to nothing, so the coster reports no rows at all")
	}
	if math.Abs(*got-10.5) > 1e-9 {
		t.Errorf("CostUSD = %v, want 10.5 ($3/1M input for 1M in + $15/1M output for 0.5M out)", *got)
	}
}
