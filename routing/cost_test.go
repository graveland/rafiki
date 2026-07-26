package routing

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// pricing with a distinct rate per component, so a formula that cross-wires two
// of them (e.g. charging cache reads at the prompt rate) produces a different
// number instead of coincidentally matching.
var testPricing = ModelPricing{
	PromptUSD:     0.000003,
	CompletionUSD: 0.000015,
	CacheReadUSD:  0.0000003,
	CacheWriteUSD: 0.00000375,
}

func TestModelPricingCostComponents(t *testing.T) {
	usage := anthropic.Usage{
		InputTokens:              1000,
		OutputTokens:             200,
		CacheReadInputTokens:     5000,
		CacheCreationInputTokens: 400,
	}

	got := testPricing.Cost(usage)

	want := CostBreakdown{
		Input:      1000 * 0.000003,
		Output:     200 * 0.000015,
		CacheRead:  5000 * 0.0000003,
		CacheWrite: 400 * 0.00000375,
	}
	want.Total = want.Input + want.Output + want.CacheRead + want.CacheWrite

	if got != want {
		t.Fatalf("Cost() = %+v, want %+v", got, want)
	}
	// Total must be the sum of the parts, not an independently computed value.
	if sum := got.Input + got.Output + got.CacheRead + got.CacheWrite; got.Total != sum {
		t.Errorf("Total = %v, want sum of components %v", got.Total, sum)
	}
}

// A zero-value ModelPricing (an unpriced model) must cost nothing rather than
// producing NaN or panicking — callers treat 0 as "unpriced".
func TestModelPricingCostZeroPricingIsFree(t *testing.T) {
	usage := anthropic.Usage{InputTokens: 1000, OutputTokens: 200}
	if got := (ModelPricing{}).Cost(usage); got != (CostBreakdown{}) {
		t.Fatalf("Cost() = %+v, want zero", got)
	}
}

// Negative control: zero usage against real pricing is also free. Guards
// against a formula that adds a per-turn constant.
func TestModelPricingCostZeroUsageIsFree(t *testing.T) {
	if got := testPricing.Cost(anthropic.Usage{}); got != (CostBreakdown{}) {
		t.Fatalf("Cost() = %+v, want zero", got)
	}
}

// Cost(usage) must be exactly CostOf over the same counts — the SDK-shaped
// entry point is an adapter, not a second formula. A SQL rollup (which has
// int64 counts and no anthropic.Usage) prices through CostOf and must agree.
func TestModelPricingCostOfMatchesCost(t *testing.T) {
	usage := anthropic.Usage{
		InputTokens:              1000,
		OutputTokens:             200,
		CacheReadInputTokens:     5000,
		CacheCreationInputTokens: 400,
	}

	fromUsage := testPricing.Cost(usage)
	fromCounts := testPricing.CostOf(1000, 200, 5000, 400)

	if fromUsage != fromCounts {
		t.Fatalf("Cost() = %+v, CostOf() = %+v; must agree", fromUsage, fromCounts)
	}
	if fromCounts.Total == 0 {
		t.Fatal("CostOf returned a zero total for priced, non-zero usage")
	}
}

// CostOf must not silently swap its parameters — each count is asserted through
// a distinct rate, so a transposed argument list changes the result.
func TestModelPricingCostOfArgumentOrder(t *testing.T) {
	got := testPricing.CostOf(1, 0, 0, 0)
	if got.Input != testPricing.PromptUSD || got.Output != 0 || got.CacheRead != 0 || got.CacheWrite != 0 {
		t.Errorf("CostOf(1,0,0,0) = %+v, want only Input charged at the prompt rate", got)
	}
	got = testPricing.CostOf(0, 0, 0, 1)
	if got.CacheWrite != testPricing.CacheWriteUSD || got.Input != 0 {
		t.Errorf("CostOf(0,0,0,1) = %+v, want only CacheWrite charged", got)
	}
}
