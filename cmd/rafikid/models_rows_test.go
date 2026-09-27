// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/models"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

func TestSourcesForKindClaudeExcludesOpenRouter(t *testing.T) {
	c := assert.NewCollecting(t)
	got := sourcesForKind(protocol.KindClaude)
	c.False(got[models.SourceOpenRouter], "claude kind admits OpenRouter ids; Claude Code cannot resolve them")
	c.False(!got[models.SourceBuiltin], "claude kind must admit the curated Anthropic ids")
}

func TestSourcesForKindDefaultsToFundi(t *testing.T) {
	// An unset kind is the not-yet-typed completion case and must behave as
	// fundi, the default kind — not as "everything".
	got := sourcesForKind("")
	assert.NewCollecting(t).False(!got[models.SourceOpenRouter] || !got[models.SourceBuiltin], "empty kind = %v, want the fundi source set", got)
}

func TestDecorateLeavesUnknownModelsBare(t *testing.T) {
	c := assert.NewCollecting(t)
	spine := []models.Model{{
		ID: "ollama/llama3", Provider: "ollama", Model: "llama3", Source: models.SourceLocal,
	}}
	rows := decorateRows(spine, nil)
	c.Require().Len(rows, 1, "len(rows) = %d, want 1", len(rows))
	if rows[0].ContextWindow != nil || rows[0].PromptUSD != nil {
		t.Errorf("row = %+v, want no catalog fields for a model with no entry", rows[0])
	}
	c.Nil(rows[0].InputModalities, "InputModalities")
	c.Eq("local", rows[0].Source, "Source")
}

func TestDecorateJoinsCatalogFactsByID(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx, max := 128000, 16384
	spine := []models.Model{{
		ID: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o",
		Source: models.SourceOpenRouter,
	}}
	cat := map[string]catalogFacts{"openai/gpt-4o": {
		name: "GPT-4o", contextLength: &ctx, maxCompletion: &max,
		promptUSD: f64ptr(0.000005), inputModalities: []string{"text", "image"},
	}}
	rows := decorateRows(spine, cat)
	if rows[0].ContextWindow == nil || *rows[0].ContextWindow != 128000 {
		t.Errorf("ContextWindow = %v, want 128000", rows[0].ContextWindow)
	}
	c.Eq("GPT-4o", rows[0].Name, "Name")
	c.Len(rows[0].InputModalities, 2, "InputModalities")
}

// The spine decides which rows exist. A catalog entry with no spine row must
// NOT appear: the spine is where kind-scoping is applied, and a row that
// bypassed it would be offered for a child that cannot run it.
func TestDecorateNeverInventsRows(t *testing.T) {
	ctx := 128000
	cat := map[string]catalogFacts{"openai/gpt-4o": {contextLength: &ctx}}
	rows := decorateRows(nil, cat)
	assert.NewCollecting(t).Empty(rows, "len(rows) = %d, want 0 — the catalog must not invent rows", len(rows))
}

func TestDecorateFiltersByProvider(t *testing.T) {
	spine := []models.Model{
		{ID: "openai/gpt-4o", Provider: "openai", Source: models.SourceOpenRouter},
		{ID: "anthropic/claude-opus-5", Provider: "anthropic", Source: models.SourceBuiltin},
	}
	rows := filterByProvider(decorateRows(spine, nil), "anthropic")
	assert.NewCollecting(t).False(len(rows) != 1 || rows[0].Provider != "anthropic", "rows = %+v, want only the anthropic row", rows)
}

// A model OpenRouter prices normally but whose pricing object carries NO cache
// rates must reach clients with those cache prices ABSENT. The old assembly
// collapsed absent to 0 inside ModelPricing and then took pointers to the
// zeroes — present-and-zero, which every client reads as free caching.
// The fixture seeds the way every existing fixture does: zero cache fields on
// CatalogEntry.Pricing seed ABSENT raw strings.
func TestListModelRowsPreservesCachePriceAbsence(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{catalog: seedCatalogWithPricing(t, "openai/gpt-x",
		&models2Pricing{prompt: 5e-6, completion: 1.5e-5})}
	rows, err := c.ListModelRows(context.Background(), "", "")
	ck.NoError(err)
	var row *connectapi.ModelRow
	for i := range rows {
		if rows[i].ID == "openrouter/openai/gpt-x" {
			row = &rows[i]
		}
	}
	ck.NotNil(row, "seeded model missing from rows (%d rows)", len(rows))
	if row.PromptUSD == nil || *row.PromptUSD != 5e-6 {
		t.Errorf("PromptUSD = %v, want 5e-06 (base prices must survive)", row.PromptUSD)
	}
	if row.CompletionUSD == nil || *row.CompletionUSD != 1.5e-5 {
		t.Errorf("CompletionUSD = %v, want 1.5e-05", row.CompletionUSD)
	}
	if row.CacheReadUSD != nil {
		t.Errorf("CacheReadUSD = %v, want ABSENT — an unreported cache rate must not read as free caching", *row.CacheReadUSD)
	}
	if row.CacheWriteUSD != nil {
		t.Errorf("CacheWriteUSD = %v, want ABSENT", *row.CacheWriteUSD)
	}
}

// seedCatalogWithPricing seeds a catalog with base prices only; cache rates
// unset, which OpenRouter sends by omitting the fields entirely.
type models2Pricing struct {
	prompt, completion, cacheRead, cacheWrite float64
}

func seedCatalogWithPricing(t *testing.T, id string, p *models2Pricing) *routing.ModelCatalog {
	t.Helper()
	c := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	entry := routing.CatalogEntry{ID: id}
	if p != nil {
		entry.Pricing = &routing.ModelPricing{
			PromptUSD:     p.prompt,
			CompletionUSD: p.completion,
			CacheReadUSD:  p.cacheRead,
			CacheWriteUSD: p.cacheWrite,
		}
	}
	c.SeedForTest([]routing.CatalogEntry{entry})
	return c
}

func f64ptr(v float64) *float64 { return &v }

// The builtin source's curation is cross-provider — anthropic AND openrouter
// ids share one list — so the source-set filter alone leaks openrouter ids
// into the claude kind. The kind's contract is served at the ROW layer; pin it
// there, where the plan's original source-set test was too coarse to see it.
func TestFilterRowsForKindClaudeKeepsOnlyAnthropic(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []connectapi.ModelRow{
		{ID: "anthropic/claude-opus-5", Provider: "anthropic", Source: "builtin"},
		{ID: "anthropic/opus-latest", Provider: "anthropic", Source: "builtin"},
		{ID: "openrouter/openai/gpt-4o", Provider: "openrouter", Source: "builtin"},
		{ID: "openrouter/google/gemini-2.5-pro", Provider: "openrouter", Source: "builtin"},
		{ID: "anthropic-oauth/claude-opus-5", Provider: "anthropic-oauth", Source: "user-config"},
	}

	got := filterRowsForKind(rows, protocol.KindClaude)
	c.Require().Len(got, 2, "claude rows")
	for _, r := range got {
		c.Eq("anthropic", r.Provider, "claude row %q has provider %q, want anthropic", r.ID, r.Provider)
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
	ck := assert.NewCollecting(t)
	c := &Controller{} // catalog nil, providers default: builtin-only spine for claude
	rows, err := c.ListModelRows(context.Background(), "", protocol.KindClaude)
	ck.Require().NoError(err)
	ck.Require().NotEmpty(rows, "no rows for the claude kind; the builtin spine came back empty")
	for _, r := range rows {
		ck.Eq("anthropic", r.Provider, "claude-kind row %q has provider %q; the builtin curation's non-Anthropic ids leak", r.ID, r.Provider)
	}
}

// byteSnapshotStore feeds the catalog a raw snapshot body, so a test can seed
// PRESENT-ZERO optional fields — SeedForTest's plain ints cannot express them.
type byteSnapshotStore struct{ data []byte }

func (s *byteSnapshotStore) Load() ([]byte, error) { return s.data, nil }
func (s *byteSnapshotStore) Save(b []byte) error   { s.data = b; return nil }

// The full presence chain on the daemon's own assembly: raw OpenRouter JSON →
// catalog Rows() → catalogFactsByID → decorateRows → the connectapi rows the
// ListModels handler serves. Present-zero must survive as present-zero and
// absent as absent — the review's end-to-end table, at the layer clients see.
func TestListModelRowsPreservePresenceEndToEnd(t *testing.T) {
	ck := assert.NewCollecting(t)
	snapshot := `{"fetched":"2099-01-01T00:00:00Z","models":[
	 {"id":"openai/zeroed","name":"Zeroed","created":1,
	  "context_length":0,
	  "top_provider":{"max_completion_tokens":0},
	  "pricing":{"prompt":"0","completion":"0",
	             "input_cache_read":"0","input_cache_write":"0"}},
	 {"id":"openai/priced","name":"Priced","created":2,
	  "context_length":128000,
	  "pricing":{"prompt":"0.000005","completion":"0.000015"}},
	 {"id":"openai/bare","name":"Bare","created":3}
	]}`
	cat := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler)).
		WithCache(&byteSnapshotStore{data: []byte(snapshot)})
	c := &Controller{catalog: cat}

	rows, err := c.ListModelRows(context.Background(), "", "")
	ck.Require().NoError(err)
	byID := map[string]connectapi.ModelRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	zeroed, ok := byID["openrouter/openai/zeroed"]
	if !ok {
		t.Fatalf("zeroed model missing from rows (%d rows)", len(rows))
	}
	for name, got := range map[string]*int{
		"ContextWindow":       zeroed.ContextWindow,
		"MaxCompletionTokens": zeroed.MaxCompletionTokens,
	} {
		ck.False(got == nil || *got != 0, "zeroed %s = %v, want a pointer to 0 — a reported zero is a fact, not an absence", name, got)
	}
	for name, got := range map[string]*float64{
		"PromptUSD":     zeroed.PromptUSD,
		"CompletionUSD": zeroed.CompletionUSD,
		"CacheReadUSD":  zeroed.CacheReadUSD,
		"CacheWriteUSD": zeroed.CacheWriteUSD,
	} {
		ck.False(got == nil || *got != 0, "zeroed %s = %v, want a pointer to 0", name, got)
	}

	priced, ok := byID["openrouter/openai/priced"]
	if !ok {
		t.Fatalf("priced model missing from rows (%d rows)", len(rows))
	}
	ck.False(priced.ContextWindow == nil || *priced.ContextWindow != 128000, "priced ContextWindow = %v, want 128000", priced.ContextWindow)
	if priced.CacheReadUSD != nil {
		t.Errorf("priced CacheReadUSD = %v, want ABSENT", *priced.CacheReadUSD)
	}
	if priced.MaxCompletionTokens != nil {
		t.Errorf("priced MaxCompletionTokens = %v, want ABSENT (field not in snapshot)", *priced.MaxCompletionTokens)
	}

	// bare: every optional field omitted from the snapshot, so ALL SIX must
	// arrive ABSENT through the full chain — the absent form is a per-field
	// fact, not something the first missing field can stand in for.
	bare, ok := byID["openrouter/openai/bare"]
	if !ok {
		t.Fatalf("bare model missing from rows (%d rows)", len(rows))
	}
	for name, got := range map[string]*int{
		"ContextWindow":       bare.ContextWindow,
		"MaxCompletionTokens": bare.MaxCompletionTokens,
	} {
		if got != nil {
			t.Errorf("bare %s = %v, want ABSENT (field not in snapshot)", name, *got)
		}
	}
	for name, got := range map[string]*float64{
		"PromptUSD":     bare.PromptUSD,
		"CompletionUSD": bare.CompletionUSD,
		"CacheReadUSD":  bare.CacheReadUSD,
		"CacheWriteUSD": bare.CacheWriteUSD,
	} {
		if got != nil {
			t.Errorf("bare %s = %v, want ABSENT (field not in snapshot)", name, *got)
		}
	}
}

// The claude kind's rows come from the catalog, not only the curated list: a
// model released after the list was written must appear under its native
// (dashed) id, priced. A curated dashed id must pick up its dotted entry's
// price, a <family>-latest alias its resolved entry's, and no ":batch" twin
// may surface as a row.
func TestListModelRowsClaudeKindFollowsCatalog(t *testing.T) {
	ck := assert.NewCollecting(t)
	cat := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	price := func(p float64) *routing.ModelPricing {
		return &routing.ModelPricing{PromptUSD: p, CompletionUSD: 5 * p}
	}
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-opus-5.5", Created: 9, Pricing: price(5e-6)},
		{ID: "anthropic/claude-opus-5.5:batch", Created: 9, Pricing: price(2.5e-6)},
		{ID: "anthropic/claude-haiku-4.5", Created: 3, Pricing: price(1e-6)},
		{ID: "openai/gpt-4o", Created: 1, Pricing: price(2.5e-6)},
	})
	c := &Controller{catalog: cat}
	rows, err := c.ListModelRows(context.Background(), "", protocol.KindClaude)
	ck.Require().NoError(err)
	byID := make(map[string]connectapi.ModelRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
		ck.False(r.Provider != "anthropic" || strings.Contains(r.ID, ":"), "claude-kind row %q (provider %q) must not be offered", r.ID, r.Provider)
	}
	for id, want := range map[string]float64{
		"anthropic/claude-opus-5-5":  5e-6, // catalog-only: not in the curated list
		"anthropic/claude-haiku-4-5": 1e-6, // curated dashed id, dotted catalog entry
		"anthropic/opus-latest":      5e-6, // alias priced as what it resolves to
	} {
		r, ok := byID[id]
		if !ok {
			t.Errorf("row %q missing", id)
			continue
		}
		ck.False(r.PromptUSD == nil || *r.PromptUSD != want, "%s: PromptUSD = %v, want %v", id, r.PromptUSD, want)
	}
}
