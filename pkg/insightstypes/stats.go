// SPDX-License-Identifier: Apache-2.0

package insightstypes

import (
	"time"
)

// zero cache_read is counted as cache waste: a large prompt that should have hit
// a warm prefix cache but didn't.

// StatsFilter narrows the population for GlobalStats. Zero-value fields are ignored.
type StatsFilter struct {
	Since, Until *time.Time
	Owner        string // owner USERNAME; resolved to a users id server-side
	Persona      string
	Source       string
	Model        string
	Path         Path
}

// Stats is the aggregate bundle. Every facet is computed over the same filtered
// population; ByPath additionally splits the token facet by capture path.
type Stats struct {
	Volume     VolumeStats           `json:"volume"`
	Adoption   AdoptionStats         `json:"adoption"`
	Tokens     TokenStats            `json:"tokens"`
	Cost       []CostRow             `json:"cost"`
	Failures   FailureStats          `json:"failures"`
	Latency    LatencyStats          `json:"latency"`
	CacheWaste CacheWasteStats       `json:"cache_waste"`
	Prefix     PrefixStats           `json:"prefix"`
	ByPath     map[string]TokenStats `json:"by_path"` // keyed by "proxy"/"direct"
}

type VolumeStats struct {
	Conversations int64 `json:"conversations"`
	Turns         int64 `json:"turns"`
}

type AdoptionStats struct {
	DistinctOwners int64        `json:"distinct_owners"`
	PerOwner       []OwnerCount `json:"per_owner"`
}

type OwnerCount struct {
	Owner         string `json:"owner"` // username, resolved through the users FK
	Conversations int64  `json:"conversations"`
	Turns         int64  `json:"turns"`
}

type TokenStats struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRatio       float64 `json:"cache_hit_ratio"` // cache_read / (input + cache_read)
}

// CostRow is a per-model token rollup. CostUSD is best-effort, from OpenRouter
// list prices via the injected Pricer, priced by routing.ModelPricing.CostOf
// (the one shared formula — see it for the cache-TTL caveat: 1h-TTL writes are
// undercounted ~2x because a stored turn doesn't record the TTL). 0 means
// unpriced (no Pricer, or the model has no resolvable price).
type CostRow struct {
	Model               string  `json:"model"`
	Turns               int64   `json:"turns"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

type FailureStats struct {
	Turns        int64   `json:"turns"`
	Errors       int64   `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	FailoverRate float64 `json:"failover_rate"` // openrouter-served turns / ALL turns (turns with no recorded upstream count in the denominator)
}

type LatencyStats struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type CacheWasteStats struct {
	WastedTurns       int64 `json:"wasted_turns"`
	WastedInputTokens int64 `json:"wasted_input_tokens"`
	Threshold         int64 `json:"threshold"`
}

// PrefixStats describes cache-prefix reuse. Cross-conversation facets
// (CrossUserPrefixes) are only meaningful for GlobalStats.
type PrefixStats struct {
	DistinctPrefixes     int64   `json:"distinct_prefixes"`
	TurnsWithPrefix      int64   `json:"turns_with_prefix"`
	ReuseRatio           float64 `json:"reuse_ratio"`           // turns-with-prefix / distinct-prefixes
	CrossUserPrefixes    int64   `json:"cross_user_prefixes"`   // prefixes reused across more than one owner
	DriftedConversations int64   `json:"drifted_conversations"` // conversations whose prefix_hash changed across turns
}

// statsScope is a shared WHERE clause plus its args over the turn⋈conversation
// join. Both entry points build one and hand it to compute.
