// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

func TestModelInfoAnswersFromTheWarmCatalog(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{}
	c.SetCatalog(seedTestCatalog(t, map[string]int{"anthropic/claude-opus-5": 200000}))

	got := c.ModelInfo("anthropic/claude-opus-5")
	ck.Require().True(got.Known, "a catalogued model must be Known")
	ck.Eq(200000, got.ContextWindow, "ContextWindow =")
	reserve := got.ContextWindow - got.AutoCompactWindow
	ck.False(reserve < got.ContextWindow/20 || reserve > got.ContextWindow/10, "AutoCompactWindow %d reserves %d, outside the 5%%-10%% band", got.AutoCompactWindow, reserve)
}

// An unknown model is Known=false with zeroes — NOT an error. The client's
// whole degradation path depends on "I do not know" being an ordinary answer
// rather than a failure it has to distinguish from a transport problem.
func TestModelInfoUnknownModelIsNotAnError(t *testing.T) {
	c := &Controller{}
	c.SetCatalog(seedTestCatalog(t, nil))
	got := c.ModelInfo("who/knows")
	assert.NewAborting(t).False(got.Known || got.AutoCompactWindow != 0, "got %+v", got)
}

// No catalog configured at all (the proxy is disabled) behaves identically.
func TestModelInfoWithNoCatalogIsNotAPanic(t *testing.T) {
	c := &Controller{} // catalog is nil
	got := c.ModelInfo("anything")
	assert.NewAborting(t).False(got.Known, "a nil catalog cannot know anything")
}

// A declared alias answers ModelInfo even with no catalog at all — this is
// the whole point: a local provider's model is never in the OpenRouter
// catalog, so the registry must be able to answer on its own.
func TestModelInfo_AliasDeclaresContextWindow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{}
	c.providers = mustParseProviders(t, `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window = 16384
`)

	got := c.ModelInfo("vmlx/qwen")
	ck.Require().True(got.Known, "an aliased model with a declared context window must be Known")
	ck.Eq(16384, got.ContextWindow, "ContextWindow")
	ck.Eq("vmlx/models/Qwen3.8-27B-Abliterated-MLX-4bit", got.ResolvedID, "ResolvedID =")
	ck.False(got.AutoCompactWindow <= 0 || got.AutoCompactWindow >= got.ContextWindow, "AutoCompactWindow = %d, want in (0, %d)", got.AutoCompactWindow, got.ContextWindow)
}

// An alias declared purely for its shorthand (no context_window) must not
// fabricate a context window, and must still fall through to the catalog
// (which, with none seeded here, leaves it unknown).
func TestModelInfo_AliasWithoutContextWindowFallsThrough(t *testing.T) {
	c := &Controller{}
	c.providers = mustParseProviders(t, `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
`)
	c.SetCatalog(seedTestCatalog(t, nil))

	got := c.ModelInfo("vmlx/qwen")
	assert.NewAborting(t).False(got.Known, "got %+v, want Known=false: no context_window declared and nothing in the catalog", got)
}

// The registry's declared context window takes priority even when the
// catalog also knows the model, since only the registry can be correct for a
// provider the catalog was never able to observe.
func TestContextWindow_AliasTakesPriorityOverCatalog(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{}
	c.providers = mustParseProviders(t, `
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window = 16384
`)
	c.SetCatalog(seedTestCatalog(t, map[string]int{"vmlx/qwen": 200000}))

	ctxLen, _, ok := c.ContextWindow("vmlx/qwen")
	ck.Require().True(ok, "expected ok=true")
	ck.Eq(16384, ctxLen, "ContextWindow")
}

func mustParseProviders(t *testing.T, toml string) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(toml))
	assert.NewAborting(t).NoError(err, "providers.Parse")
	return set
}

// seedTestCatalog builds a *routing.ModelCatalog with the given model→context
// length entries injected via SeedForTest, so no network is touched.
func seedTestCatalog(t *testing.T, entries map[string]int) *routing.ModelCatalog {
	t.Helper()
	c := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	es := make([]routing.CatalogEntry, 0, len(entries))
	for id, ctxLen := range entries {
		es = append(es, routing.CatalogEntry{ID: id, ContextLength: ctxLen})
	}
	c.SeedForTest(es)
	return c
}
