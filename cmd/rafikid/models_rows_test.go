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
	"go.graveland.dev/rafiki/pkg/providers"
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
	rows, err := c.ListModelRows(context.Background(), "", "", "")
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

// OpenRouter's "~" auto-latest aliases are real ids users spawn by name, so the
// listing offers them as openrouter/~<id> rows — completion and `rafiki models`
// both read this path.
func TestListModelRowsIncludesTildeAliases(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{catalog: seedCatalogWithPricing(t, "~deepseek/deepseek-flash-latest", nil)}
	rows, err := c.ListModelRows(context.Background(), "", "", "")
	ck.NoError(err)
	for _, r := range rows {
		if r.ID == "openrouter/~deepseek/deepseek-flash-latest" {
			return
		}
	}
	t.Fatalf("alias row missing from %d rows", len(rows))
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
	rows, err := c.ListModelRows(context.Background(), "", "", protocol.KindClaude)
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

	rows, err := c.ListModelRows(context.Background(), "", "", "")
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
		// The catalog advertises the tier; without this row the catalog does
		// not offer "opus-latest" at all and no such row may appear.
		{ID: "~anthropic/claude-opus-latest", Created: 10},
	})
	c := &Controller{catalog: cat}
	rows, err := c.ListModelRows(context.Background(), "", "", protocol.KindClaude)
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

// latestFixture is the catalog + provider set the "<name>-latest" tests share.
//
// The catalog advertises exactly two tiers, each with a "~<vendor>/…-latest"
// alias entry — that advertisement is what makes a tier offerable at all — and
// a "nova" tier advertised with no model behind it, which must stay unofferable.
// The provider set names one provider per vendor plus "local", whose kind has
// no vendor and therefore no latest names.
func latestFixture(t *testing.T) (*routing.ModelCatalog, *providers.Set) {
	t.Helper()
	cat := routing.NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "openai/gpt-6.1-terra", Created: 5,
			Pricing: &routing.ModelPricing{PromptUSD: 5e-6, CompletionUSD: 1.5e-5}},
		{ID: "~openai/gpt-terra-latest", Created: 6},
		{ID: "anthropic/claude-opus-4.8", Created: 3},
		{ID: "~anthropic/claude-opus-latest", Created: 4},
		{ID: "~openai/gpt-nova-latest", Created: 7}, // advertised, nothing behind it
	})
	set := &providers.Set{
		DefaultProvider: "anthropic",
		Providers: map[string]providers.Provider{
			"anthropic": {Kind: providers.KindAnthropic},
			"gpt":       {Kind: providers.KindOpenAIResponses},
			"local":     {Kind: providers.KindOpenAI},
		},
	}
	return cat, set
}

// A "<name>-latest" row is offered under the OPERATOR's provider name, and only
// for a vendor that provider's kind can actually reach — the naming is what a
// spawn accepts, so a row whose id no spawner resolves is worse than no row.
// A tier the catalog cannot resolve stays off the list entirely.
func TestListModelRowsOffersLatestNames(t *testing.T) {
	ck := assert.NewCollecting(t)
	// HOME is not isolated package-wide, and the builtin source reads
	// ~/.pi/agent/models.json; keep this listing hermetic.
	t.Setenv("HOME", t.TempDir())
	cat, set := latestFixture(t)
	c := &Controller{catalog: cat, providers: set}

	rows, err := c.ListModelRows(context.Background(), "", "", "")
	ck.Require().NoError(err)
	byID := make(map[string]connectapi.ModelRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}

	for _, want := range []struct{ id, provider string }{
		{"gpt/gpt-terra-latest", "gpt"},
		{"anthropic/opus-latest", "anthropic"},
	} {
		r, ok := byID[want.id]
		if !ok {
			t.Errorf("row %q missing; the catalog resolves that tier for its provider's kind", want.id)
			continue
		}
		ck.Eq(want.provider, r.Provider, "%s: Provider", want.id)
		ck.Eq(models.SourceBuiltin, models.Source(r.Source), "%s: Source", want.id)
	}

	// A provider whose kind has no vendor gets no latest rows: "openai" is not
	// a vendor for a plain Chat-Completions endpoint, and inventing one would
	// offer names the spawn path cannot resolve.
	for _, r := range rows {
		ck.False(r.Provider == "local" && strings.HasSuffix(r.Model, "-latest"),
			"provider \"local\" (KindOpenAI, no vendor) was offered the latest row %q", r.ID)
	}
	// Names are per vendor, never shared across providers: the anthropic
	// provider must not carry the OpenAI vendor's names.
	ck.False(byID["anthropic/gpt-terra-latest"].ID != "",
		"anthropic provider offered the OpenAI vendor's name %q", "gpt-terra-latest")
	// A tier the catalog ADVERTISES with no model behind it (the fixture's
	// "~openai/gpt-nova-latest" row) is not offered: the alias row alone is not
	// a model, so listing it would hand the picker a name every spawn refuses.
	_, offered := byID["gpt/gpt-nova-latest"]
	ck.False(offered, "gpt/gpt-nova-latest was listed, but the catalog resolves no model for that tier")
}

// A latest row must carry the facts of the model it resolves to — the model id
// it means is what gets priced, so an unpriced alias silently records cost 0
// and defeats a child's maxCost guardrail.
func TestCatalogFactsCoverLatestNames(t *testing.T) {
	ck := assert.NewCollecting(t)
	cat, set := latestFixture(t)
	c := &Controller{catalog: cat, providers: set}

	facts := c.catalogFactsByID(cat.Rows())

	target, ok := facts["openrouter/openai/gpt-6.1-terra"]
	ck.Require().True(ok, "the resolved model has no facts; the fixture seeded no price")
	latest, ok := facts["gpt/gpt-terra-latest"]
	ck.Require().True(ok, "gpt/gpt-terra-latest has no facts; a latest row must carry what it resolves to")
	ck.Require().NotNil(latest.promptUSD, "gpt/gpt-terra-latest: promptUSD")
	ck.Require().NotNil(target.promptUSD, "openrouter/openai/gpt-6.1-terra: promptUSD")
	ck.Eq(*target.promptUSD, *latest.promptUSD, "latest row prompt price")
	if latest.completionUSD == nil || target.completionUSD == nil {
		t.Fatalf("completion prices: latest=%v target=%v, want both present", latest.completionUSD, target.completionUSD)
	}
	ck.Eq(*target.completionUSD, *latest.completionUSD, "latest row completion price")

	// The Anthropic arm resolves through the native spelling, and the fact must
	// land on the same row id the listing offers.
	_, ok = facts["anthropic/opus-latest"]
	ck.True(ok, "anthropic/opus-latest has no facts; the Anthropic arm of the vendor loop is not covered")

	// An advertised tier with no model behind it is not offered, so it must have
	// no facts either — a listing never offers what a spawn would refuse.
	_, found := facts["gpt/gpt-nova-latest"]
	ck.False(found, "gpt/gpt-nova-latest has facts but the catalog resolves no model for that tier")
}
