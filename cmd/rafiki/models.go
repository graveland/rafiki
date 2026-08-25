package main

import (
	"context"
	"sort"

	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
)

// modelSourcesForKind returns the model sources that a child of the given kind
// can actually resolve.
//
// The two kinds have genuinely different model universes, and offering one
// kind's ids to another produces a child that spawns, attaches, and then never
// answers — no error, just a TUI that never becomes idle:
//
//   - "fundi" (the DEFAULT kind) is rafiki's native runtime through this
//     module's llm/routing, so it takes exactly what rafiki resolves: the
//     curated Anthropic ids and <family>-latest aliases, plus any OpenRouter
//     slash id.
//   - "claude" shells out to Claude Code, which only knows Anthropic ids.
//
// When --kind has not been typed yet, cobra gives us the flag's default and we
// answer for that rather than for everything — a completion list that is a
// superset of what will actually work is how this went wrong before.
func modelSourcesForKind(kind string) map[models.Source]bool {
	switch kind {
	case protocol.KindClaude:
		// Claude Code resolves Anthropic ids itself; the curated list and the
		// family aliases are the only entries that can apply.
		return map[models.Source]bool{models.SourceBuiltin: true}
	default: // protocol.KindFundi, and the not-yet-typed case
		return map[models.Source]bool{
			models.SourceBuiltin:    true,
			models.SourceOpenRouter: true,
		}
	}
}

// completeModel returns tab-completion candidates for the --model flag, scoped
// to the kind of child being created (see modelSourcesForKind). Sources are
// best-effort and errors are swallowed, so completion never blocks or errors.
func completeModel(kind, _ string) []string {
	// ListSources, not List-then-filter: an unwanted source is never consulted,
	// so a pi completion pays no OpenRouter round trip and a fundi completion
	// pays no Ollama/LM Studio probe.
	set, err := providers.Load(paths.ProvidersFile())
	if err != nil {
		return nil
	}
	list := models.ListSources(context.Background(), set, modelSourcesForKind(kind))
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}
