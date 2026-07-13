// Package llm is the library front door: a typed builder over the routing
// core with DB-backed Conversations. Callers never construct
// anthropic.MessageNewParams for conversation sends — the library owns
// history loading, request assembly, trimming, cache breakpoints and
// prefix_hash.
package llm

// Upstream identifies a configured model provider. Values match the
// conversation_turn.upstream column.
type Upstream string

const (
	UpstreamAnthropic  Upstream = "anthropic"
	UpstreamOpenRouter Upstream = "openrouter"
)

// CacheTTL is a prompt-cache breakpoint lifetime.
type CacheTTL string

const (
	Cache1h CacheTTL = "1h"
	Cache5m CacheTTL = "5m"
)

const (
	anthropicBaseURL  = "https://api.anthropic.com"
	openRouterBaseURL = "https://openrouter.ai/api"
)
