// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestSourcesForKindClaudeExcludesOpenRouter(t *testing.T) {
	got := sourcesForKind(protocol.KindClaude)
	if got[models.SourceOpenRouter] {
		t.Error("claude kind admits OpenRouter ids; Claude Code cannot resolve them")
	}
	if !got[models.SourceBuiltin] {
		t.Error("claude kind must admit the curated Anthropic ids")
	}
}

func TestSourcesForKindDefaultsToFundi(t *testing.T) {
	// An unset kind is the not-yet-typed completion case and must behave as
	// fundi, the default kind — not as "everything".
	got := sourcesForKind("")
	if !got[models.SourceOpenRouter] || !got[models.SourceBuiltin] {
		t.Errorf("empty kind = %v, want the fundi source set", got)
	}
}

func TestDecorateLeavesUnknownModelsBare(t *testing.T) {
	spine := []models.Model{{
		ID: "ollama/llama3", Provider: "ollama", Model: "llama3", Source: models.SourceLocal,
	}}
	rows := decorateRows(spine, nil)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].ContextWindow != nil || rows[0].PromptUSD != nil {
		t.Errorf("row = %+v, want no catalog fields for a model with no entry", rows[0])
	}
	if rows[0].InputModalities != nil {
		t.Errorf("InputModalities = %v, want nil (unknown)", rows[0].InputModalities)
	}
	if rows[0].Source != "local" {
		t.Errorf("Source = %q, want %q", rows[0].Source, "local")
	}
}

func TestDecorateJoinsCatalogFactsByID(t *testing.T) {
	spine := []models.Model{{
		ID: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o",
		Source: models.SourceOpenRouter,
	}}
	cat := map[string]catalogFacts{"openai/gpt-4o": {
		name: "GPT-4o", contextLength: 128000, maxCompletion: 16384,
		promptUSD: f64ptr(0.000005), inputModalities: []string{"text", "image"},
	}}
	rows := decorateRows(spine, cat)
	if rows[0].ContextWindow == nil || *rows[0].ContextWindow != 128000 {
		t.Errorf("ContextWindow = %v, want 128000", rows[0].ContextWindow)
	}
	if rows[0].Name != "GPT-4o" {
		t.Errorf("Name = %q, want GPT-4o", rows[0].Name)
	}
	if len(rows[0].InputModalities) != 2 {
		t.Errorf("InputModalities = %v, want 2", rows[0].InputModalities)
	}
}

// The spine decides which rows exist. A catalog entry with no spine row must
// NOT appear: the spine is where kind-scoping is applied, and a row that
// bypassed it would be offered for a child that cannot run it.
func TestDecorateNeverInventsRows(t *testing.T) {
	cat := map[string]catalogFacts{"openai/gpt-4o": {contextLength: 128000}}
	if rows := decorateRows(nil, cat); len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0 — the catalog must not invent rows", len(rows))
	}
}

func TestDecorateFiltersByProvider(t *testing.T) {
	spine := []models.Model{
		{ID: "openai/gpt-4o", Provider: "openai", Source: models.SourceOpenRouter},
		{ID: "anthropic/claude-opus-5", Provider: "anthropic", Source: models.SourceBuiltin},
	}
	rows := filterByProvider(decorateRows(spine, nil), "anthropic")
	if len(rows) != 1 || rows[0].Provider != "anthropic" {
		t.Errorf("rows = %+v, want only the anthropic row", rows)
	}
}

func f64ptr(v float64) *float64 { return &v }
