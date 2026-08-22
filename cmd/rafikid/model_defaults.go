// SPDX-License-Identifier: Apache-2.0

package main

import "go.graveland.dev/rafiki/pkg/providers"

// minContextFilesBudget/maxContextFilesBudget/contextFilesBudgetFraction bound
// contextFilesBudget's formula: never so little that a whole project's
// context files vanish (1024 tokens still fits a short CLAUDE.md), never so
// much that a huge context window invites dumping unbounded raw text (30000
// tokens is already generous). See docs/plans/2026-08-22-model-aware-prompt-budget-design.md.
const (
	minContextFilesBudget      = 1024
	maxContextFilesBudget      = 30000
	contextFilesBudgetFraction = 5 // window / 5 = 20% of the window
)

// contextFilesBudget computes the default context-files token budget from a
// model's context window: 20% of the window, clamped to
// [minContextFilesBudget, maxContextFilesBudget]. contextWindow <= 0 means
// unknown, and returns 0 (no cap) — guessing wrong for an unfamiliar model
// risks truncating something nobody asked to be cut.
func contextFilesBudget(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}
	budget := contextWindow / contextFilesBudgetFraction
	if budget < minContextFilesBudget {
		budget = minContextFilesBudget
	}
	if budget > maxContextFilesBudget {
		budget = maxContextFilesBudget
	}
	return budget
}

// modelDefaults holds per-model spawn defaults resolved from a provider's
// declared model alias — the fields the OpenRouter catalog can never answer,
// since it only ever observes OpenRouter's own model catalog: how much of
// the prompt budget context files may occupy, and which skills/MCP servers
// are even relevant for this model.
type modelDefaults struct {
	// ContextFilesTokens is the resolved token budget: the alias's explicit
	// ContextFilesTokens override if set, otherwise contextFilesBudget of its
	// declared ContextWindow. 0 means no cap.
	ContextFilesTokens int
	Skills             *string
	MCPServers         *string
}

// resolveModelDefaults looks up model's provider and, if its local id names a
// declared alias (providers.ModelAlias), returns that alias's resolved
// defaults. ok is false when set is nil, the provider is unknown, or the
// model's local id names no declared alias — the caller leaves every
// affected field at its existing zero-value behavior in that case (no
// context-files cap, no model-declared skills/MCP default).
//
// This mirrors Controller.aliasContextWindow (cmd/rafikid/models_presets.go)
// but is a plain function, not a Controller method: it needs only the
// provider registry, never the OpenRouter catalog, so it costs nothing to
// call from agentFlags.toRuntimeOptions, which has no Controller in scope.
func resolveModelDefaults(set *providers.Set, model string) (modelDefaults, bool) {
	if set == nil {
		return modelDefaults{}, false
	}
	name, localID := providers.SplitRaw(model)
	if name == "" {
		name = set.DefaultProvider
	}
	p, ok := set.Get(name)
	if !ok {
		return modelDefaults{}, false
	}
	alias, ok := p.Models[localID]
	if !ok {
		return modelDefaults{}, false
	}
	budget := alias.ContextFilesTokens
	if budget == 0 {
		budget = contextFilesBudget(alias.ContextWindow)
	}
	return modelDefaults{
		ContextFilesTokens: budget,
		Skills:             alias.Skills,
		MCPServers:         alias.MCPServers,
	}, true
}
