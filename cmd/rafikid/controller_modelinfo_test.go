// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

func TestModelInfoAnswersFromTheWarmCatalog(t *testing.T) {
	c := &Controller{}
	c.SetCatalog(seedTestCatalog(t, map[string]int{"anthropic/claude-opus-5": 200000}))

	got := c.ModelInfo("anthropic/claude-opus-5")
	if !got.Known {
		t.Fatal("a catalogued model must be Known")
	}
	if got.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d", got.ContextWindow)
	}
	reserve := got.ContextWindow - got.AutoCompactWindow
	if reserve < got.ContextWindow/20 || reserve > got.ContextWindow/10 {
		t.Errorf("AutoCompactWindow %d reserves %d, outside the 5%%-10%% band", got.AutoCompactWindow, reserve)
	}
}

// An unknown model is Known=false with zeroes — NOT an error. The client's
// whole degradation path depends on "I do not know" being an ordinary answer
// rather than a failure it has to distinguish from a transport problem.
func TestModelInfoUnknownModelIsNotAnError(t *testing.T) {
	c := &Controller{}
	c.SetCatalog(seedTestCatalog(t, nil))
	got := c.ModelInfo("who/knows")
	if got.Known || got.AutoCompactWindow != 0 {
		t.Fatalf("got %+v", got)
	}
}

// No catalog configured at all (the proxy is disabled) behaves identically.
func TestModelInfoWithNoCatalogIsNotAPanic(t *testing.T) {
	c := &Controller{} // catalog is nil
	got := c.ModelInfo("anything")
	if got.Known {
		t.Fatal("a nil catalog cannot know anything")
	}
}

// A declared alias answers ModelInfo even with no catalog at all — this is
// the whole point: a local provider's model is never in the OpenRouter
// catalog, so the registry must be able to answer on its own.
func TestModelInfo_AliasDeclaresContextWindow(t *testing.T) {
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
	if !got.Known {
		t.Fatal("an aliased model with a declared context window must be Known")
	}
	if got.ContextWindow != 16384 {
		t.Errorf("ContextWindow = %d, want 16384", got.ContextWindow)
	}
	if got.ResolvedID != "vmlx/models/Qwen3.8-27B-Abliterated-MLX-4bit" {
		t.Errorf("ResolvedID = %q", got.ResolvedID)
	}
	if got.AutoCompactWindow <= 0 || got.AutoCompactWindow >= got.ContextWindow {
		t.Errorf("AutoCompactWindow = %d, want in (0, %d)", got.AutoCompactWindow, got.ContextWindow)
	}
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
	if got.Known {
		t.Fatalf("got %+v, want Known=false: no context_window declared and nothing in the catalog", got)
	}
}

// The registry's declared context window takes priority even when the
// catalog also knows the model, since only the registry can be correct for a
// provider the catalog was never able to observe.
func TestContextWindow_AliasTakesPriorityOverCatalog(t *testing.T) {
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
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ctxLen != 16384 {
		t.Errorf("ContextWindow = %d, want the registry's 16384, not the catalog's 200000", ctxLen)
	}
}

func mustParseProviders(t *testing.T, toml string) *providers.Set {
	t.Helper()
	set, err := providers.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("providers.Parse: %v", err)
	}
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
