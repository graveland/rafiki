package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/batch"
	"go.graveland.dev/rafiki/pkg/batchdb"
	"go.graveland.dev/rafiki/pkg/providers"
)

// newBatcher builds the daemon's ONE parked-call batcher over the shared pool,
// submitting through the provider registry's `openrouter` entry.
//
// It returns ok=false (and a nil batcher) — the daemon runs with no batcher
// and a :batch first call fails with pkg/llm's "no batcher configured" error —
// when any of these holds:
//
//   - pool is nil (a DSN-less daemon has nowhere to keep batch rows);
//   - no provider named `openrouter` is configured (batch rows and Batch-API
//     calls are OpenRouter-shaped, so nothing else can back them);
//   - that provider's kind is not anthropic-openrouter;
//   - its APIKey() resolves empty (an unset OPENROUTER_API_KEY) — a batcher
//     that can only produce 401s is worse than none.
//
// One case deliberately does NOT refuse here: a :batch model that resolves to
// a provider OTHER than `openrouter` still spawns fine. Its first call parks
// only if THAT provider's kind is anthropic-openrouter and pkg/llm holds this
// batcher — but a batcher is wired daemon-wide, regardless of which provider a
// given child's model names, so pkg/llm would park a non-openrouter :batch
// request into rows submitted through openrouter's credentials. prepareSend
// already refuses that at the kind gate (:batch models are served only by an
// anthropic-openrouter provider), and a non-openrouter provider with that kind
// is today unreachable: the batcher is built from the provider literally named
// `openrouter`, so the documented rule is — a :batch model resolving to any
// provider other than `openrouter` gets no valid route: the kind gate fails it
// (not anthropic-openrouter) or it parks through the openrouter batcher, whose
// credentials cannot serve it. There is no live fallback either way.
//
// Start is the CALLER's job (go b.Start(ctx) — it blocks), so the daemon owns
// the goroutine and the lifetime, not this constructor.
func newBatcher(set *providers.Set, pool *pgxpool.Pool) (*batch.Batcher, bool) {
	if pool == nil {
		return nil, false
	}
	if set == nil {
		set = providers.Default()
	}
	p, ok := set.Get("openrouter")
	if !ok {
		slog.Info("batch transport disabled: no provider named \"openrouter\"")
		return nil, false
	}
	if p.Kind != providers.KindAnthropicOpenRouter {
		slog.Info("batch transport disabled: the \"openrouter\" provider is not an anthropic-openrouter provider",
			"kind", string(p.Kind))
		return nil, false
	}
	if p.APIKey() == "" {
		slog.Info("batch transport disabled: the \"openrouter\" provider's API key is unset")
		return nil, false
	}
	api := batch.NewOpenRouter(p.BaseURL, p.APIKey(), nil)
	store := batchdb.New(pool)
	return batch.New(store, api, batch.Options{}), true
}
