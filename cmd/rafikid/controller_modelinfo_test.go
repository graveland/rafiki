// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"testing"
	"time"

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
