// SPDX-License-Identifier: Apache-2.0

package routing

import "github.com/anthropics/anthropic-sdk-go"

// CostBreakdown is the per-component USD cost of one request's token usage.
// Total is always the sum of the four components; callers that only want a
// single number read Total, and those rendering a cost split (e.g. showing what
// cache reads saved) read the components.
type CostBreakdown struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	Total      float64
}

// CostOf prices raw token counts at this model's list rates. This is the
// primitive: it takes counts rather than an anthropic.Usage so a SQL rollup
// (per-model BIGINT sums out of conversation_turn) prices through the same
// formula as a live response, without the DB-facing packages importing the
// Anthropic SDK.
//
// A zero-value ModelPricing (an unpriced model) yields a zero CostBreakdown, so
// callers can treat 0 as "unpriced" without a separate ok check.
//
// Cache writes are billed at the 5-minute rate (CacheWriteUSD), so 1h-TTL
// writes are undercounted: neither a usage block nor a stored turn records the
// TTL, which is why CacheWrite1hUSD is deliberately unused here.
func (p ModelPricing) CostOf(input, output, cacheRead, cacheWrite int64) CostBreakdown {
	c := CostBreakdown{
		Input:      float64(input) * p.PromptUSD,
		Output:     float64(output) * p.CompletionUSD,
		CacheRead:  float64(cacheRead) * p.CacheReadUSD,
		CacheWrite: float64(cacheWrite) * p.CacheWriteUSD,
	}
	c.Total = c.Input + c.Output + c.CacheRead + c.CacheWrite
	return c
}

// Cost prices one response's usage. It is a thin adapter over CostOf — the
// arithmetic has exactly one home.
func (p ModelPricing) Cost(usage anthropic.Usage) CostBreakdown {
	return p.CostOf(
		usage.InputTokens,
		usage.OutputTokens,
		usage.CacheReadInputTokens,
		usage.CacheCreationInputTokens,
	)
}
