// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
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

// The builtin source's curation is cross-provider — anthropic AND openrouter
// ids share one list — so the source-set filter alone leaks openrouter ids
// into the claude kind. The kind's contract is served at the ROW layer; pin it
// there, where the plan's original source-set test was too coarse to see it.
func TestFilterRowsForKindClaudeKeepsOnlyAnthropic(t *testing.T) {
	rows := []connectapi.ModelRow{
		{ID: "anthropic/claude-opus-5", Provider: "anthropic", Source: "builtin"},
		{ID: "anthropic/opus-latest", Provider: "anthropic", Source: "builtin"},
		{ID: "openrouter/openai/gpt-4o", Provider: "openrouter", Source: "builtin"},
		{ID: "openrouter/google/gemini-2.5-pro", Provider: "openrouter", Source: "builtin"},
		{ID: "anthropic-oauth/claude-opus-5", Provider: "anthropic-oauth", Source: "user-config"},
	}

	got := filterRowsForKind(rows, protocol.KindClaude)
	if len(got) != 2 {
		t.Fatalf("claude rows = %+v, want only the two provider==anthropic rows", got)
	}
	for _, r := range got {
		if r.Provider != "anthropic" {
			t.Errorf("claude row %q has provider %q, want anthropic", r.ID, r.Provider)
		}
	}

	// Only claude narrows rows. fundi, the empty not-yet-typed case, and any
	// other kind keep everything — the source set did that job.
	for _, kind := range []string{"", protocol.KindFundi, "nonsense"} {
		if got := filterRowsForKind(rows, kind); len(got) != len(rows) {
			t.Errorf("kind %q dropped rows (%d of %d); only the claude kind narrows rows", kind, len(got), len(rows))
		}
	}
}

// End to end over the real spine: sourcesForKind(claude) admits {builtin}, and
// the builtin curation carries openrouter ids, so ListModelRows must narrow
// the rows itself or claude completion offers ids Claude Code cannot run.
// Hermetic: the claude source set enables no network-backed source and the
// default provider set is in-memory.
func TestListModelRowsClaudeKindAdmitsOnlyAnthropic(t *testing.T) {
	c := &Controller{} // catalog nil, providers default: builtin-only spine for claude
	rows, err := c.ListModelRows(context.Background(), "", protocol.KindClaude)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows for the claude kind; the builtin spine came back empty")
	}
	for _, r := range rows {
		if r.Provider != "anthropic" {
			t.Errorf("claude-kind row %q has provider %q; the builtin curation's non-Anthropic ids leak", r.ID, r.Provider)
		}
	}
}
