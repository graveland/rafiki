// SPDX-License-Identifier: Apache-2.0

// Package llm is the library front door: a typed builder over the routing
// core with DB-backed Conversations. Callers never construct a provider's wire
// request for conversation sends — the library owns history loading, request
// assembly, trimming, cache breakpoints and prefix_hash.
package llm

const (
	anthropicBaseURL  = "https://api.anthropic.com"
	openRouterBaseURL = "https://openrouter.ai/api"
)
