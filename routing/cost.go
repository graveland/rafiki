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

// Cost prices one request's usage at this model's list rates. A zero-value
// ModelPricing (an unpriced model) yields a zero CostBreakdown, so callers can
// treat 0 as "unpriced" without a separate ok check.
//
// Cache writes are billed at the 5-minute rate (CacheWriteUSD).
// anthropic.Usage does not distinguish 5m from 1h cache creation, so
// CacheWrite1hUSD is deliberately unused here — pricing a 1h-TTL write needs
// information the usage block doesn't carry.
func (p ModelPricing) Cost(usage anthropic.Usage) CostBreakdown {
	c := CostBreakdown{
		Input:      float64(usage.InputTokens) * p.PromptUSD,
		Output:     float64(usage.OutputTokens) * p.CompletionUSD,
		CacheRead:  float64(usage.CacheReadInputTokens) * p.CacheReadUSD,
		CacheWrite: float64(usage.CacheCreationInputTokens) * p.CacheWriteUSD,
	}
	c.Total = c.Input + c.Output + c.CacheRead + c.CacheWrite
	return c
}
