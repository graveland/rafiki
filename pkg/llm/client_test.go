// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"math"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// responsesProviderSet is the shape a daemon that serves a Responses provider
// has: the default Anthropic entry alongside one Responses-kind provider.
// Keeping both is the point: the mapping under test is resolved from the
// configured kinds, and it must not reach a model the Anthropic entry's kind
// would have recorded catalog-shaped.
func responsesProviderSet(t *testing.T) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(`
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.oa]
kind = "openai-responses"
api_key_env = "OPENAI_API_KEY"
`))
	assert.NewAborting(t).NoError(err, "Parse the test provider set")
	return set
}

// twoIDCatalog indexes the SAME slug under both id shapes at different prices,
// so a rollup that consults the wrong one is visible in the number rather than
// hidden behind an equal price. 100 input + 50 output tokens:
//
//	anthropic/claude-sonnet-5 -> 0.2
//	openai/gpt-6.1-sol        -> 0.2
//	openai/claude-sonnet-5    -> 2.0 (the wrong answer, 10x)
func twoIDCatalog(t *testing.T) *routing.ModelCatalog {
	t.Helper()
	cat := routing.NewModelCatalog(nil, time.Hour, testLogger(t))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-sonnet-5", Created: 1, Pricing: &routing.ModelPricing{
			PromptUSD: 0.001, CompletionUSD: 0.002,
		}},
		{ID: "openai/gpt-6.1-sol", Created: 2, Pricing: &routing.ModelPricing{
			PromptUSD: 0.001, CompletionUSD: 0.002,
		}},
		{ID: "openai/claude-sonnet-5", Created: 3, Pricing: &routing.ModelPricing{
			PromptUSD: 0.010, CompletionUSD: 0.020,
		}},
	})
	return cat
}

// usageJSON is one canned assistant message reporting `model` with 100 input
// and 50 output tokens, which is the shape capture stores on the turn row.
func usageJSON(model string) string {
	return `{"id":"msg_cost","type":"message","role":"assistant","model":"` + model + `",
		"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
}

// TestConversationCostPricesResponsesModelByCatalogID is the spend-control
// guard: a Responses-kind provider serves "gpt-6.1-sol" while the catalog
// indexes "openai/gpt-6.1-sol", and pricing the captured row by its bare id
// used to SKIP it. A skipped row is indistinguishable from a free one, so a
// child's lifetime maxCost guardrail reset every turn instead of accumulating.
func TestConversationCostPricesResponsesModelByCatalogID(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		func(*msg.Request) (*msg.Response, error) {
			return cannedMessage(usageJSON("gpt-6.1-sol")), nil
		},
	}}
	c, err := NewClient(
		WithProviders(responsesProviderSet(t)),
		WithProviderSender("oa", sender),
		WithStore(pool),
		WithCatalog(twoIDCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("oa/gpt-6.1-sol"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := c.ConversationCost(ctx, conv.ID)
	want := 100*0.001 + 50*0.002 // the openai/gpt-6.1-sol entry, not 0
	ck.LessOrEqual(1e-9, math.Abs(got-want), "ConversationCost = %v, want %v (the openai/<slug> list price)", got, want)
}

// TestConversationCostUnchangedForAnthropic is the other half of the guard: the
// same fixture, the same provider set (which DOES configure a Responses kind,
// so the mapping is available), but turns served by the Anthropic entry. The
// stored id is catalog-shaped, so it must price at its own entry and never at
// the "openai/"-prefixed one.
func TestConversationCostUnchangedForAnthropic(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		func(*msg.Request) (*msg.Response, error) {
			return cannedMessage(usageJSON("claude-sonnet-5")), nil
		},
	}}
	c, err := NewClient(
		WithProviders(responsesProviderSet(t)),
		WithProviderSender("anthropic", sender),
		WithStore(pool),
		WithCatalog(twoIDCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-sonnet-5"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := c.ConversationCost(ctx, conv.ID)
	want := 100*0.001 + 50*0.002 // anthropic/claude-sonnet-5, NOT openai/claude-sonnet-5 (2.0)
	ck.LessOrEqual(1e-9, math.Abs(got-want), "ConversationCost = %v, want %v (the raw id's own price)", got, want)
}

// TestConversationCostTurnPricingTriesRawIDFirst pins the ordering and the
// candidate set directly, because the DB-backed rollup tests skip without
// RAFIKI_TEST_DSN: a raw-id hit wins over the configured kind's rewrite, a bare
// Responses id is still found through it, and a configuration with no Responses
// kind produces no second candidate at all.
func TestConversationCostTurnPricingTriesRawIDFirst(t *testing.T) {
	ck := assert.NewCollecting(t)
	cat := twoIDCatalog(t)

	c, err := NewClient(WithProviders(responsesProviderSet(t)), WithCatalog(cat), WithLogger(testLogger(t)))
	ck.Require().NoError(err)
	mp, ok := c.turnPricing("claude-sonnet-5")
	ck.Require().True(ok, "turnPricing(claude-sonnet-5)")
	ck.LessOrEqual(1e-9, math.Abs(0.001-mp.PromptUSD), "raw id priced at %v/1k, want 0.001 (the unprefixed entry, not 0.010)", mp.PromptUSD)
	mp, ok = c.turnPricing("gpt-6.1-sol")
	ck.Require().True(ok, "turnPricing(gpt-6.1-sol) must find openai/gpt-6.1-sol")
	ck.LessOrEqual(1e-9, math.Abs(0.001-mp.PromptUSD), "mapped id prompt price = %v, want 0.001", mp.PromptUSD)

	// The shipped default configures no Responses kind, so it adds no candidate:
	// a bare OpenAI id stays unpriced exactly as before this mapping existed.
	plain, err := NewClient(WithCatalog(cat), WithLogger(testLogger(t)))
	ck.Require().NoError(err)
	_, ok = plain.turnPricing("gpt-6.1-sol")
	ck.False(ok, "turnPricing(gpt-6.1-sol) without a Responses kind must miss, not widen")
}
