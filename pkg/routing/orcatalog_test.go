// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

const orFixture = `{"data":[
 {"id":"anthropic/claude-sonnet-5","created":1782000000,"canonical_slug":"anthropic/claude-sonnet-5-20260630"},
 {"id":"anthropic/claude-sonnet-4.6","created":1770000000,"canonical_slug":"x"},
 {"id":"anthropic/claude-opus-4.8","created":1779000000,"canonical_slug":"x"},
 {"id":"anthropic/claude-opus-4.8-fast","created":1779500000,"canonical_slug":"x"},
 {"id":"anthropic/claude-haiku-4.5","created":1760000000,"canonical_slug":"x"},
 {"id":"~anthropic/claude-sonnet-latest","created":1782999999,"canonical_slug":"x"},
 {"id":"openai/gpt-4o","created":1770000000,"canonical_slug":"x"}
]}`

func newTestCatalog(t *testing.T, body string) (*ModelCatalog, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	c := NewModelCatalog(srv.Client(), time.Minute, slog.New(slog.DiscardHandler))
	c.url = srv.URL // test hook
	return c, srv
}

func TestResolveLatest(t *testing.T) {
	ck := assert.NewAborting(t)
	c, srv := newTestCatalog(t, orFixture)
	defer srv.Close()

	// sonnet-latest → newest non-alias, non-fast sonnet = claude-sonnet-5
	ant, or, ok := c.ResolveLatest("sonnet")
	ck.False(!ok || ant != "claude-sonnet-5" || or != "anthropic/claude-sonnet-5", "sonnet: got (%q,%q,%v)", ant, or, ok)
	// opus-latest → 4.8 (the -fast variant is excluded)
	ant, or, ok = c.ResolveLatest("opus")
	ck.False(!ok || ant != "claude-opus-4-8" || or != "anthropic/claude-opus-4.8", "opus: got (%q,%q,%v)", ant, or, ok)
}

func TestLatestAlias(t *testing.T) {
	for _, in := range []string{"haiku-latest", "claude-haiku-latest", "~anthropic/claude-haiku-latest"} {
		if fam, ok := LatestAlias(in); !ok || fam != "haiku" {
			t.Errorf("LatestAlias(%q) = (%q,%v)", in, fam, ok)
		}
	}
	_, ok := LatestAlias("claude-sonnet-5")
	assert.NewCollecting(t).False(ok, "concrete id must not be a latest alias")
}

// TestOpenRouterModel covers the catalog-backed reverse lookup that replaced the
// hand-maintained anthropicToOpenRouter map. The point is that an id present in
// the catalog resolves to its real dotted OR id with no hardcoded map — so
// failover stays valid as new models ship (the map-drift bug this fixes).
func TestOpenRouterModel(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
		{ID: "anthropic/claude-opus-4.8", Created: 3},
		{ID: "~anthropic/claude-opus-latest", Created: 4}, // alias, must be skipped
		{ID: "openai/gpt-4o", Created: 5},
	})
	cases := map[string]string{
		"claude-haiku-4-5":          "anthropic/claude-haiku-4.5", // dash id -> dotted OR id
		"claude-sonnet-5":           "anthropic/claude-sonnet-5",
		"claude-opus-4-8":           "anthropic/claude-opus-4.8",
		"openai/gpt-4o":             "openai/gpt-4o",             // slash: passthrough
		"anthropic/claude-opus-4.8": "anthropic/claude-opus-4.8", // already OR-native
		"claude-future-9":           "anthropic/claude-future-9", // catalog miss: best-effort
	}
	for in, want := range cases {
		got := c.OpenRouterModel(in)
		ck.Eq(want, got, "OpenRouterModel(%q) = %q, want", in, got)
	}
	var nilCat *ModelCatalog
	ck.Eq("anthropic/claude-opus-4-8", nilCat.OpenRouterModel("claude-opus-4-8"), "nil-catalog fallback =")
	ck.Eq("openai/gpt-4o", nilCat.OpenRouterModel("openai/gpt-4o"), "nil-catalog slash passthrough =")
}

// TestResolveNewest covers the model-alias resolver: newest release of a
// model line by catalog prefix, where "release" means the prefix itself or a
// stamped point release ("-0905") — never a variant fork (-thinking, -code),
// a new line (kimi-k3.5), or a ~alias.
func TestResolveNewest(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "moonshotai/kimi-k2.6", Created: 10},
		{ID: "moonshotai/kimi-k3", Created: 20},
		{ID: "moonshotai/kimi-k3-0905", Created: 30},     // stamped point release: newest wins
		{ID: "moonshotai/kimi-k3-thinking", Created: 40}, // variant fork: excluded
		{ID: "moonshotai/kimi-k3.5", Created: 50},        // new line: excluded
		{ID: "~moonshotai/kimi-latest", Created: 60},     // OR alias: excluded
		{ID: "deepseek/deepseek-v4-pro", Created: 70},
		{ID: "deepseek/deepseek-v4-flash", Created: 80}, // different line, never matches -pro
	})
	if got, ok := c.ResolveNewest("moonshotai/kimi-k3"); !ok || got != "moonshotai/kimi-k3-0905" {
		t.Errorf("kimi-k3: got (%q,%v), want moonshotai/kimi-k3-0905", got, ok)
	}
	if got, ok := c.ResolveNewest("deepseek/deepseek-v4-pro"); !ok || got != "deepseek/deepseek-v4-pro" {
		t.Errorf("deepseek-v4-pro: got (%q,%v), want deepseek/deepseek-v4-pro", got, ok)
	}
	got, ok := c.ResolveNewest("deepseek/deepseek-v5")
	assert.NewCollecting(t).False(ok, "absent line must not resolve, got %q", got)
}

func TestModelAliases(t *testing.T) {
	c := assert.NewAborting(t)
	got := ModelAliases()
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2", "kimi-k3"}
	c.Len(got, len(want), "ModelAliases = %v, want %v", got, want)
	for i := range want {
		c.Eq(want[i], got[i], "ModelAliases = %v, want %v", got, want)
	}
}

func TestResolveModel(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
		{ID: "moonshotai/kimi-k3", Created: 3},
		{ID: "deepseek/deepseek-v4-pro", Created: 4},
		{ID: "deepseek/deepseek-v4-flash", Created: 5},
		{ID: "z-ai/glm-5.2", Created: 6},
		{ID: "openai/gpt-4o", Created: 7},
		{ID: "~openai/gpt-latest", Created: 8}, // auto-latest alias (tilde form only)
		{ID: "~anthropic/claude-sonnet-latest", Created: 9},
	})
	mustResolve := func(def, req, want string) {
		t.Helper()
		if got, err := ResolveModel(c, def, req); err != nil || got != want {
			t.Errorf("ResolveModel(%q,%q) = (%q,%v), want %q", def, req, got, err, want)
		}
	}
	mustResolve("haiku-latest", "", "claude-haiku-4-5") // empty -> default alias -> catalog
	mustResolve("haiku-latest", "sonnet-latest", "claude-sonnet-5")
	mustResolve("haiku-latest", "claude-opus-4-8", "claude-opus-4-8") // concrete passthrough
	mustResolve("haiku-latest", "openai/gpt-4o", "openai/gpt-4o")     // real slash id: passthrough

	// OpenRouter auto-latest alias: the bare form (AllIDs strips the ~, and users
	// copy-paste it) is re-tilded to the real catalog id instead of 400ing.
	mustResolve("haiku-latest", "openai/gpt-latest", "~openai/gpt-latest")
	// Already-tilde form is left as-is.
	mustResolve("haiku-latest", "~openai/gpt-latest", "~openai/gpt-latest")
	// A slash id with neither bare nor tilde form must not gain an invented tilde;
	// it passes through and OpenRouter's (now surfaced) error explains it.
	mustResolve("haiku-latest", "openai/nonexistent", "openai/nonexistent")
	// Anthropic -latest resolves to a concrete id in every form, including OR's
	// ~anthropic/claude-<fam>-latest.
	mustResolve("haiku-latest", "~anthropic/claude-sonnet-latest", "claude-sonnet-5")

	// An "anthropic/<x>" id names the native/direct Anthropic sender: the prefix
	// is stripped and <x> resolved exactly as a bare id, so the concrete result
	// has NO slash and downstream slash-routing keeps it on the Anthropic path.
	mustResolve("haiku-latest", "anthropic/sonnet-latest", "claude-sonnet-5")       // alias, same as bare sonnet-latest
	mustResolve("haiku-latest", "anthropic/claude-sonnet-4-5", "claude-sonnet-4-5") // concrete id, prefix stripped, passthrough
	// A non-anthropic provider prefix stays an OpenRouter-native slash id, unchanged.
	mustResolve("haiku-latest", "deepseek/deepseek-chat", "deepseek/deepseek-chat")

	// Short model aliases resolve to the line's newest OR id (slash form, so
	// downstream slash routing sends them to OpenRouter). An empty request
	// resolves through a model-alias default too.
	mustResolve("haiku-latest", "kimi-k3", "moonshotai/kimi-k3")
	mustResolve("haiku-latest", "deepseek-v4-pro", "deepseek/deepseek-v4-pro")
	mustResolve("haiku-latest", "deepseek-v4-flash", "deepseek/deepseek-v4-flash")
	mustResolve("haiku-latest", "glm-5.2", "z-ai/glm-5.2")
	mustResolve("kimi-k3", "", "moonshotai/kimi-k3")

	// An alias the catalog can't resolve errors — there is no hardcoded
	// fallback list to silently paper over it with a stale id.
	if _, err := ResolveModel(c, "", "opus-latest"); err == nil {
		t.Error("opus-latest absent from catalog must error, not fall back to a hardcoded id")
	}
	if _, err := ResolveModel(nil, "", "haiku-latest"); err == nil {
		t.Error("nil catalog + -latest must error")
	}
	if _, err := ResolveModel(nil, "", "kimi-k3"); err == nil {
		t.Error("nil catalog + model alias must error")
	}
	// No requested model AND no default must error loudly — there is no hardcoded
	// silent default (haiku or otherwise) to paper over an unset model.
	_, err := ResolveModel(c, "", "")
	assert.NewCollecting(t).Error(err, "empty model + empty default must error, not silently pick a model")
}

// TestProviderPrefsFor covers provider pinning: a pinned line matches its
// base id and stamped point releases (same inModelLine semantics as aliases),
// never a different line, and unpinned models carry no preferences.
func TestProviderPrefsFor(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, id := range []string{"z-ai/glm-5.2", "z-ai/glm-5.2-0905"} {
		prefs, ok := ProviderPrefsFor(id)
		if !ok {
			t.Errorf("%s: want a pin", id)
			continue
		}
		c.False(len(prefs.Only) != 1 || prefs.Only[0] != "fireworks", "%s: Only = %v, want [fireworks]", id, prefs.Only)
	}
	for _, id := range []string{"z-ai/glm-5.20", "z-ai/glm-5", "z-ai/glm-5.2.1", "moonshotai/kimi-k3", "claude-haiku-4-5"} {
		_, ok := ProviderPrefsFor(id)
		c.False(ok, "%s: must not be pinned", id)
	}
}

// TestCatalogRefreshCoalesces proves concurrent refreshes are coalesced into one
// OpenRouter fetch (singleflight), so the shared server catalog doesn't stampede
// the endpoint on a cold/expired cache under load.
func TestCatalogRefreshCoalesces(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // hold every in-flight request open so callers overlap
		_, _ = w.Write([]byte(orFixture))
	}))
	defer srv.Close()
	c := NewModelCatalog(srv.Client(), time.Minute, slog.New(slog.DiscardHandler))
	c.url = srv.URL

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { c.AllIDs() })
	}
	time.Sleep(50 * time.Millisecond) // let the goroutines converge on the single fetch
	close(release)
	wg.Wait()

	got := atomic.LoadInt32(&hits)
	assert.NewCollecting(t).Eq(1, got, "catalog fetched %d times under %d concurrent callers, want 1", got, n)
}

func TestAllIDs(t *testing.T) {
	ck := assert.NewAborting(t)
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "openai/gpt-4o", Created: 1},
		{ID: "anthropic/claude-opus-4.8", Created: 2},
		{ID: "~anthropic/claude-sonnet-latest", Created: 3},
		{ID: "openai/gpt-4o", Created: 4}, // dup
	})
	got := c.AllIDs()
	want := []string{"anthropic/claude-opus-4.8", "anthropic/claude-sonnet-latest", "openai/gpt-4o"}
	ck.Len(got, len(want), "AllIDs = %v, want %v", got, want)
	for i := range want {
		ck.Eq(want[i], got[i], "AllIDs = %v, want %v", got, want)
	}
}

func TestAutoCompactWindow(t *testing.T) {
	cases := []struct {
		name                      string
		contextLen, maxComp, want int
	}{
		{"codex: reserve capped at 10%", 400000, 128000, 360000},
		{"sonnet: reserve = full max completion", 1000000, 64000, 936000},
		{"gpt-4o: reserve capped at 10%", 128000, 16000, 115200},
		{"small max completion floored to 5%", 200000, 8000, 190000},
		{"no max completion reported -> 5% floor, not full window", 200000, 0, 190000},
		{"zero context -> 0 (caller skips)", 0, 64000, 0},
		{"negative context -> 0", -5, 64000, 0},
	}
	for _, c := range cases {
		got := AutoCompactWindow(c.contextLen, c.maxComp)
		assert.NewCollecting(t).Eq(c.want, got, "%s: AutoCompactWindow(%d,%d) = %d, want", c.name, c.contextLen, c.maxComp, got)
	}
}

func TestContextWindow(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "openai/gpt-5-codex", Created: 1, ContextLength: 400000, MaxCompletionTokens: 128000},
		{ID: "anthropic/claude-sonnet-5", Created: 2, ContextLength: 1000000, MaxCompletionTokens: 64000},
		{ID: "~openai/gpt-latest", Created: 3, ContextLength: 400000, MaxCompletionTokens: 128000},
		{ID: "openai/no-window", Created: 4}, // reports no context_length
	})
	cases := []struct {
		name, model      string
		wantCtx, wantMax int
		wantOK           bool
	}{
		{"slash id direct", "openai/gpt-5-codex", 400000, 128000, true},
		{"OR auto-latest alias re-tilded", "openai/gpt-latest", 400000, 128000, true},
		{"family-latest -> anthropic OR entry", "sonnet-latest", 1000000, 64000, true},
		{"dated snapshot id -> base model", "claude-sonnet-5-20260630", 1000000, 64000, true},
		{"catalog miss", "openai/unknown", 0, 0, false},
		{"entry without a context length", "openai/no-window", 0, 0, false},
	}
	for _, tc := range cases {
		gotCtx, gotMax, ok := c.ContextWindow(tc.model)
		ck.False(ok != tc.wantOK || gotCtx != tc.wantCtx || gotMax != tc.wantMax, "%s: ContextWindow(%q) = (%d,%d,%v), want (%d,%d,%v)", tc.name, tc.model, gotCtx, gotMax, ok, tc.wantCtx, tc.wantMax, tc.wantOK)
	}
	var nilCat *ModelCatalog
	_, _, ok := nilCat.ContextWindow("openai/gpt-5-codex")
	ck.False(ok, "nil catalog must return ok=false")
}

// memStore is an in-memory SnapshotStore for tests. An empty store returns
// (nil,nil), which the catalog treats as a cold cache.
type memStore struct{ data []byte }

func (m *memStore) Load() ([]byte, error) { return m.data, nil }
func (m *memStore) Save(b []byte) error   { m.data = b; return nil }

func TestModelCatalogCache(t *testing.T) {
	c := assert.NewCollecting(t)
	var hits atomic.Int32
	body := `{"data":[{"id":"openai/gpt-5-codex","created":1,"context_length":400000,"top_provider":{"max_completion_tokens":128000}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	store := &memStore{}

	// Cold cache: one fetch, snapshot persisted to the store.
	c1 := NewModelCatalog(srv.Client(), time.Hour, slog.New(slog.DiscardHandler))
	c1.url = srv.URL
	c1.WithCache(store)
	if ctxLen, maxComp, ok := c1.ContextWindow("openai/gpt-5-codex"); !ok || ctxLen != 400000 || maxComp != 128000 {
		t.Fatalf("c1 ContextWindow = (%d,%d,%v), want (400000,128000,true)", ctxLen, maxComp, ok)
	}
	c.Require().Eq(1, hits.Load(), "cold cache should fetch once, got")
	c.Require().NotEmpty(store.data, "fetch must persist a snapshot to the store")

	// A fresh process sharing the warm store must not hit the network.
	c2 := NewModelCatalog(srv.Client(), time.Hour, slog.New(slog.DiscardHandler))
	c2.url = srv.URL
	c2.WithCache(store)
	if ctxLen, _, ok := c2.ContextWindow("openai/gpt-5-codex"); !ok || ctxLen != 400000 {
		t.Fatalf("c2 ContextWindow from cache = (%d,%v), want (400000,true)", ctxLen, ok)
	}
	c.Eq(1, hits.Load(), "warm cache must not re-fetch, got")
}

// TestModelPricingWireDecode guards the json tags on orPricing against the live
// OpenRouter wire shape: the pricing object was previously dropped on decode.
func TestModelPricingWireDecode(t *testing.T) {
	ck := assert.NewCollecting(t)
	const wire = `{"data":[{"id":"anthropic/claude-sonnet-5","created":1,
	 "pricing":{"prompt":"0.000002","completion":"0.00001","input_cache_read":"0.0000002",
	           "input_cache_write":"0.0000025","input_cache_write_1h":"0.000004"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(wire))
	}))
	defer srv.Close()
	c := NewModelCatalog(srv.Client(), time.Minute, slog.New(slog.DiscardHandler))
	c.url = srv.URL
	c.Warm()

	p, ok := c.Pricing("anthropic/claude-sonnet-5")
	ck.Require().True(ok, "pricing not decoded from wire")
	ck.False(p.PromptUSD != 0.000002 || p.CompletionUSD != 0.00001, "base price = prompt %g / completion %g, want 0.000002/0.00001", p.PromptUSD, p.CompletionUSD)
	ck.False(p.CacheReadUSD != 0.0000002 || p.CacheWriteUSD != 0.0000025 || p.CacheWrite1hUSD != 0.000004, "cache prices = read %g / write %g / write1h %g, want 0.0000002/0.0000025/0.000004", p.CacheReadUSD, p.CacheWriteUSD, p.CacheWrite1hUSD)
}

func TestPricing(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	sonnet := ModelPricing{PromptUSD: 0.000002, CompletionUSD: 0.00001, CacheReadUSD: 0.0000002, CacheWriteUSD: 0.0000025}
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-sonnet-5", Created: 2, Pricing: &sonnet},
		{ID: "~openai/gpt-latest", Created: 3, Pricing: &ModelPricing{PromptUSD: 0.000001, CompletionUSD: 0.000003}},
		{ID: "moonshotai/kimi-k3", Created: 4}, // no pricing seeded → unpriced
	})

	// Bare Anthropic id resolves to anthropic/<id>.
	if p, ok := c.Pricing("claude-sonnet-5"); !ok || p.PromptUSD != 0.000002 || p.CacheReadUSD != 0.0000002 {
		t.Errorf("bare-id pricing = (%+v,%v), want sonnet prices", p, ok)
	}
	// Exact OR slug.
	if p, ok := c.Pricing("anthropic/claude-sonnet-5"); !ok || p.CompletionUSD != 0.00001 {
		t.Errorf("slug pricing = (%+v,%v), want sonnet prices", p, ok)
	}
	// Family-latest alias resolves to the newest of the family.
	if p, ok := c.Pricing("sonnet-latest"); !ok || p.PromptUSD != 0.000002 {
		t.Errorf("sonnet-latest pricing = (%+v,%v), want sonnet prices", p, ok)
	}
	// Dated Anthropic snapshot ids aren't on OpenRouter; priced as the base model.
	if p, ok := c.Pricing("claude-sonnet-5-20260101"); !ok || p.PromptUSD != 0.000002 {
		t.Errorf("dated snapshot pricing = (%+v,%v), want sonnet prices", p, ok)
	}
	// A trailing number that isn't a date is not stripped.
	if _, ok := c.Pricing("claude-sonnet-5-12345678"); ok {
		t.Errorf("non-date numeric suffix should not price")
	}
	// Tilde auto-latest requested without its "~" is normalized and resolves.
	if p, ok := c.Pricing("openai/gpt-latest"); !ok || p.PromptUSD != 0.000001 {
		t.Errorf("tilde-alias pricing = (%+v,%v), want gpt prices", p, ok)
	}
	// Unknown model → false.
	if _, ok := c.Pricing("mystery/model-x"); ok {
		t.Error("unknown model must not resolve pricing")
	}
	// Present model without prices → false.
	_, ok := c.Pricing("moonshotai/kimi-k3")
	assert.NewCollecting(t).False(ok, "model with no price strings must resolve ok=false")
}

func TestStripSnapshotDate(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5", true},
		{"claude-sonnet-5", "", false},
		{"moonshotai/kimi-k3", "", false},
		{"claude-x-12345678", "", false}, // 8 digits but not a 20xx date
		{"20251001", "", false},          // no base id
	}
	for _, tc := range cases {
		got, ok := stripSnapshotDate(tc.in)
		assert.NewCollecting(t).False(ok != tc.ok || (ok && got != tc.want), "stripSnapshotDate(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
	}
}

// TestFetchBackoff proves a failed fetch suppresses further network attempts
// for fetchBackoff — a cold cache during an OpenRouter outage must not fire a
// GET on every resolve.
func TestFetchBackoff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("not json")) // decode fails → recorded failure
	}))
	defer srv.Close()
	c := NewModelCatalog(srv.Client(), time.Minute, slog.New(slog.DiscardHandler))
	c.url = srv.URL
	for range 5 {
		c.Warm() // each would refresh; backoff must cap fetches to one
	}
	got := hits.Load()
	assert.NewCollecting(t).Eq(1, got, "fetch hits = %d, want 1 (failed fetch backs off for %s)", got, fetchBackoff)
}

// TestResolveIDUsesCatalogResolution proves ResolveID is a thin wrapper over
// the same resolution entryFor already applies to Pricing and ContextWindow —
// it must not reimplement any of the bare-id/slash-id/alias rules.
func TestResolveIDUsesCatalogResolution(t *testing.T) {
	c := NewModelCatalog(nil, time.Hour, nil)
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-opus-5"},
		{ID: "moonshotai/kimi-k3"},
	})

	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"claude-opus-5", "anthropic/claude-opus-5", true},           // bare Anthropic id
		{"anthropic/claude-opus-5", "anthropic/claude-opus-5", true}, // slash id passes through
		{"kimi-k3", "moonshotai/kimi-k3", true},                      // modelAliases
		{"gpt-5.6", "", false},                                       // not in catalog
	}
	for _, tc := range cases {
		got, ok := c.ResolveID(tc.in)
		assert.NewCollecting(t).False(got != tc.want || ok != tc.wantOK, "ResolveID(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
	}
}

// TestModelCatalogPriceSatisfiesStorePriceSource proves Price reports the
// catalog's prices in the shape store.SyncModelPricing writes.
func TestModelCatalogPriceSatisfiesStorePriceSource(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewModelCatalog(nil, time.Hour, nil)
	c.SeedForTest([]CatalogEntry{
		{
			ID: "anthropic/claude-opus-5",
			Pricing: &ModelPricing{
				PromptUSD:     0.000005,
				CompletionUSD: 0.000025,
				CacheReadUSD:  0.0000005,
				CacheWriteUSD: 0.00000625,
			},
		},
		{ID: "moonshotai/kimi-k3"}, // no Pricing set: unpriced
	})

	got, ok := c.Price("claude-opus-5")
	ck.Require().True(ok, "Price(claude-opus-5) ok = false, want true")
	ck.False(got.PromptUSD != 0.000005 || got.CompletionUSD != 0.000025, "Price(claude-opus-5) base = %g/%g, want 0.000005/0.000025", got.PromptUSD, got.CompletionUSD)
	ck.False(got.CacheReadUSD == nil || *got.CacheReadUSD != 0.0000005, "Price(claude-opus-5) cache read = %v, want 0.0000005", got.CacheReadUSD)
	ck.False(got.CacheWriteUSD == nil || *got.CacheWriteUSD != 0.00000625, "Price(claude-opus-5) cache write = %v, want 0.00000625", got.CacheWriteUSD)

	if _, ok := c.Price("kimi-k3"); ok {
		t.Error("Price(kimi-k3) ok = true, want false: entry has no Pricing")
	}
	if _, ok := c.Price("gpt-5.6"); ok {
		t.Error("Price(gpt-5.6) ok = true, want false: not in catalog")
	}
}

// A model OpenRouter prices but does not cache reports nil cache prices, not
// zero ones. Zero is a real price meaning "free"; recording it for an absent
// rate made the dashboard's cache-savings tile compute the cache tokens at the
// full prompt price.
func TestPriceAbsentCachePriceIsNil(t *testing.T) {
	c := NewModelCatalog(nil, time.Hour, nil)
	c.SeedForTest([]CatalogEntry{
		// No cache prices seeded → OpenRouter omits the fields.
		{ID: "vendor/no-cache-model", Pricing: &ModelPricing{PromptUSD: 0.000001, CompletionUSD: 0.000002}},
	})

	got, ok := c.Price("vendor/no-cache-model")
	assert.NewAborting(t).True(ok, "Price ok = false, want true: base prices are present")
	if got.CacheReadUSD != nil {
		t.Errorf("cache read = %v, want nil for an omitted price", *got.CacheReadUSD)
	}
	if got.CacheWriteUSD != nil {
		t.Errorf("cache write = %v, want nil for an omitted price", *got.CacheWriteUSD)
	}
}

// Lookup is the syncer's only catalog accessor, so it must carry the id, the
// prices and the priced/unpriced verdict that ResolveID + Price used to report
// separately.
func TestLookupReportsIDAndPriceInOneCall(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewModelCatalog(nil, time.Hour, nil)
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-opus-5", Pricing: &ModelPricing{
			PromptUSD: 0.000005, CompletionUSD: 0.000025, CacheReadUSD: 0.0000005,
		}},
		{ID: "moonshotai/kimi-k3"}, // in the catalog, but unpriced
	})

	// A bare Anthropic id resolves, and reports the same id ResolveID does.
	info, ok := c.Lookup("claude-opus-5")
	ck.Require().True(ok, "Lookup(claude-opus-5) ok = false, want true")
	ck.Eq("anthropic/claude-opus-5", info.ORID, "ORID")
	ck.False(!info.Priced || info.Price.PromptUSD != 0.000005, "Lookup(claude-opus-5) = %+v, want priced with prompt 0.000005", info)
	id, _ := c.ResolveID("claude-opus-5")
	ck.Eq(info.ORID, id, "Lookup ORID")

	// An entry with no prices is still found — the syncer records the row with
	// its or_id and NULL prices rather than dropping the model.
	info, ok = c.Lookup("kimi-k3")
	ck.Require().True(ok, "Lookup(kimi-k3) ok = false, want true: the entry exists")
	ck.Eq("moonshotai/kimi-k3", info.ORID, "ORID")
	ck.False(info.Priced, "Lookup(kimi-k3) Priced = true, want false: entry has no prices")

	if _, ok := c.Lookup("gpt-5.6"); ok {
		t.Error("Lookup(gpt-5.6) ok = true, want false: not in catalog")
	}
}

// A typed-nil *ModelCatalog reaches store.SyncModelPricing as a NON-nil
// store.PriceSource, so the syncer's `src == nil` check cannot catch it. Every
// interface method must survive it: the sync runs in a bare goroutine with no
// recover, so a panic here takes the server down.
func TestNilCatalogSatisfiesPriceSourceWithoutPanic(t *testing.T) {
	c := assert.NewCollecting(t)
	var src store.PriceSource = (*ModelCatalog)(nil)

	src.Warm()
	c.Empty(src.AllIDs(), "AllIDs on a nil catalog")
	_, ok := src.Lookup("claude-opus-5")
	c.False(ok, "Lookup on a nil catalog ok = true, want false")
}

// TestProviderPrefsMarshal proves the wire shape OpenRouter expects: only the
// populated fields appear. An ignore-only prefs object must not emit an empty
// "only", which OpenRouter would read as "restrict routing to no providers".
func TestProviderPrefsMarshal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prefs ProviderPrefs
		want  string
	}{
		{"ignore only", ProviderPrefs{Ignore: []string{"coreweave"}}, `{"ignore":["coreweave"]}`},
		{"only only", ProviderPrefs{Only: []string{"fireworks"}}, `{"only":["fireworks"]}`},
		{"both", ProviderPrefs{Only: []string{"fireworks"}, Ignore: []string{"coreweave"}},
			`{"only":["fireworks"],"ignore":["coreweave"]}`},
		{"empty", ProviderPrefs{}, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			b, err := json.Marshal(tc.prefs)
			c.Require().NoError(err, "Marshal")
			c.Eq(tc.want, string(b), "Marshal = %s, want", b)
		})
	}
}

func TestCatalogDecodesNameAndInputModalities(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":[
	 {"id":"openai/gpt-4o","name":"GPT-4o","created":1,"context_length":128000,
	  "architecture":{"input_modalities":["text","image"]},
	  "pricing":{"prompt":"0.000005","completion":"0.000015"}},
	 {"id":"openai/text-only","name":"Text Only","created":2,"context_length":8000,
	  "architecture":{"input_modalities":["text"]}}
	]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	rows := c.Rows()
	byID := map[string]CatalogRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	got, ok := byID["openai/gpt-4o"]
	if !ok {
		t.Fatalf("gpt-4o missing from Rows(); got %d rows", len(rows))
	}
	ck.Eq("GPT-4o", got.Name, "Name")
	ck.False(len(got.InputModalities) != 2 || got.InputModalities[0] != "text" ||
		got.InputModalities[1] != "image", "InputModalities = %v, want [text image]", got.InputModalities)
	ck.False(got.ContextLength == nil || *got.ContextLength != 128000, "ContextLength = %v, want 128000", got.ContextLength)
	ck.False(got.PromptUSD == nil || *got.PromptUSD != 0.000005, "PromptUSD = %v, want 0.000005", got.PromptUSD)
	ck.False(got.CompletionUSD == nil || *got.CompletionUSD != 0.000015, "CompletionUSD = %v, want 0.000015", got.CompletionUSD)
	// The fixture prices no cache rates: those must be ABSENT, not zero.
	ck.False(got.CacheReadUSD != nil || got.CacheWriteUSD != nil, "cache prices = %v/%v, want nil for a model OpenRouter prices without them", got.CacheReadUSD, got.CacheWriteUSD)

	// A text-only model reports ["text"] and must stay distinguishable from
	// a model the catalog knows nothing about (nil).
	if txt := byID["openai/text-only"]; len(txt.InputModalities) != 1 {
		t.Errorf("text-only InputModalities = %v, want [text]", txt.InputModalities)
	}
}

// An entry with no architecture block must yield NIL modalities, not an empty
// non-nil slice: nil is what the picker reads as "unknown", and an empty slice
// would read as "this model accepts nothing".
func TestCatalogAbsentArchitectureYieldsNilModalities(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":[{"id":"openai/bare","created":1,"context_length":4096}]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	rows := c.Rows()
	ck.Require().Len(rows, 1, "len(rows) = %d, want 1", len(rows))
	ck.Nil(rows[0].InputModalities, "InputModalities")
	if rows[0].PromptUSD != nil || rows[0].CompletionUSD != nil ||
		rows[0].CacheReadUSD != nil || rows[0].CacheWriteUSD != nil {
		t.Errorf("prices = %#v, want all-nil for an unpriced entry", rows[0])
	}
}

// A snapshot persisted before these fields existed must still decode, with the
// new fields empty, rather than failing the whole cache load. Same caveat
// Pricing already carries.
func TestCatalogStaleSnapshotDecodesWithoutNewFields(t *testing.T) {
	old := `{"fetched":"2099-01-01T00:00:00Z","models":[
	 {"id":"openai/gpt-4o","created":1,"context_length":128000}
	]}`
	store := &memStore{data: []byte(old)}
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler)).WithCache(store)

	rows := c.Rows()
	assert.NewAborting(t).Len(rows, 1, "len(rows) = %d, want 1 from the stale snapshot", len(rows))
	if rows[0].Name != "" || rows[0].InputModalities != nil {
		t.Errorf("stale row = %+v, want empty Name and nil InputModalities", rows[0])
	}
	// The one field the old snapshot DID carry must decode as present; the
	// fields it predates must be nil, not zero.
	if rows[0].ContextLength == nil || *rows[0].ContextLength != 128000 {
		t.Errorf("ContextLength = %v, want the value the old snapshot carried", rows[0].ContextLength)
	}
	if rows[0].MaxCompletionTokens != nil || rows[0].PromptUSD != nil ||
		rows[0].CompletionUSD != nil || rows[0].CacheReadUSD != nil || rows[0].CacheWriteUSD != nil {
		t.Errorf("stale row optionals = %+v, want all nil", rows[0])
	}
}

// Rows() must skip the "~" alias ids for the same reason AllIDs does: they are
// routing aliases, not models a user picks.
func TestRowsSkipsTildeAliases(t *testing.T) {
	c, srv := newTestCatalog(t, orFixture)
	defer srv.Close()
	for _, r := range c.Rows() {
		assert.NewCollecting(t).False(strings.HasPrefix(r.ID, "~"), "Rows() returned alias id %q", r.ID)
	}
}

// Presence must survive enumeration for ALL SIX optional numeric fields, in
// both forms: absent (OpenRouter omitted the field) stays nil, and explicitly
// reported zero stays a pointer to 0. The old shape collapsed absence into
// ModelPricing zeroes (a priced model with unreported cache rates read as free
// caching) and dropped reported zeroes with > 0 guards — a table over every
// field is what catches that class of collapse, and layer-local tests did not.
//
// Three fixtures between them cover both forms for all six fields:
//   - openai/priced: base prices present, everything else absent — the shape
//     that used to arrive as present-and-zero cache rates.
//   - openai/unpriced: no pricing object at all — prompt/completion absent.
//   - openai/zeroed: every optional field explicitly 0 — present-zero.
func TestRowsPreservePresenceOnEveryOptionalField(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":[
	 {"id":"openai/priced","name":"Priced","created":1,
	  "context_length":128000,
	  "top_provider":{"max_completion_tokens":16384},
	  "pricing":{"prompt":"0.000005","completion":"0.000015"}},
	 {"id":"openai/unpriced","name":"Unpriced","created":2},
	 {"id":"openai/zeroed","name":"Zeroed","created":3,
	  "context_length":0,
	  "top_provider":{"max_completion_tokens":0},
	  "pricing":{"prompt":"0","completion":"0",
	             "input_cache_read":"0","input_cache_write":"0"}}
	]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	rows := c.Rows()
	byID := map[string]CatalogRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	// priced: base prices present with their values; every other optional
	// ABSENT — including the cache rates, which the old assembly published as
	// zeroes.
	priced := byID["openai/priced"]
	ck.Require().NotEq("", priced.ID, "priced model missing from Rows()")
	ck.False(priced.PromptUSD == nil || *priced.PromptUSD != 0.000005, "priced PromptUSD = %v, want 0.000005", priced.PromptUSD)
	ck.False(priced.CompletionUSD == nil || *priced.CompletionUSD != 0.000015, "priced CompletionUSD = %v, want 0.000015", priced.CompletionUSD)
	ck.False(priced.ContextLength == nil || *priced.ContextLength != 128000, "priced ContextLength = %v, want 128000", priced.ContextLength)
	ck.False(priced.MaxCompletionTokens == nil || *priced.MaxCompletionTokens != 16384, "priced MaxCompletionTokens = %v, want 16384", priced.MaxCompletionTokens)
	if priced.CacheReadUSD != nil {
		t.Errorf("priced CacheReadUSD = %v, want nil — an unreported cache rate must not read as free caching", *priced.CacheReadUSD)
	}
	if priced.CacheWriteUSD != nil {
		t.Errorf("priced CacheWriteUSD = %v, want nil", *priced.CacheWriteUSD)
	}

	// unpriced: prompt/completion absent too.
	unpriced := byID["openai/unpriced"]
	ck.Require().NotEq("", unpriced.ID, "unpriced model missing from Rows()")
	for name, got := range map[string]*float64{
		"PromptUSD":     unpriced.PromptUSD,
		"CompletionUSD": unpriced.CompletionUSD,
		"CacheReadUSD":  unpriced.CacheReadUSD,
		"CacheWriteUSD": unpriced.CacheWriteUSD,
	} {
		if got != nil {
			t.Errorf("unpriced %s = %v, want nil", name, *got)
		}
	}
	if unpriced.ContextLength != nil {
		t.Errorf("unpriced ContextLength = %v, want nil", *unpriced.ContextLength)
	}
	if unpriced.MaxCompletionTokens != nil {
		t.Errorf("unpriced MaxCompletionTokens = %v, want nil", *unpriced.MaxCompletionTokens)
	}

	zeroed := byID["openai/zeroed"]
	ck.Require().NotEq("", zeroed.ID, "zeroed model missing from Rows()")
	for name, got := range map[string]*float64{
		"PromptUSD":     zeroed.PromptUSD,
		"CompletionUSD": zeroed.CompletionUSD,
		"CacheReadUSD":  zeroed.CacheReadUSD,
		"CacheWriteUSD": zeroed.CacheWriteUSD,
	} {
		if got == nil {
			t.Errorf("reported-zero %s = nil, want a pointer to 0 — presence is the fact", name)
			continue
		}
		ck.Eq(0, *got, "reported-zero %s = %v, want 0", name, *got)
	}
	ck.False(zeroed.ContextLength == nil || *zeroed.ContextLength != 0, "reported-zero ContextLength = %v, want a pointer to 0", zeroed.ContextLength)
	ck.False(zeroed.MaxCompletionTokens == nil || *zeroed.MaxCompletionTokens != 0, "reported-zero MaxCompletionTokens = %v, want a pointer to 0", zeroed.MaxCompletionTokens)
}

func TestCatalogDecodesToolSupportAndExpiry(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":[
	 {"id":"a/tools","created":100,"context_length":1000,
	  "supported_parameters":["tools","tool_choice","temperature"],
	  "expiration_date":"2026-09-08"},
	 {"id":"a/no-tools","created":200,"context_length":1000,
	  "supported_parameters":["temperature"]},
	 {"id":"a/unknown","created":300,"context_length":1000}
	]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	by := map[string]CatalogRow{}
	for _, r := range c.Rows() {
		by[r.ID] = r
	}

	if got := by["a/tools"].SupportedParameters; len(got) != 3 || got[0] != "tools" {
		t.Errorf("SupportedParameters = %v, want the declared three", got)
	}
	ck.Eq("2026-09-08", by["a/tools"].ExpiresAt, "ExpiresAt")
	ck.Len(by["a/no-tools"].SupportedParameters, 1, "SupportedParameters")
	ck.Eq("", by["a/no-tools"].ExpiresAt, "ExpiresAt")
	// An entry with NO list means UNKNOWN, not "supports nothing" -- the same
	// rule InputModalities follows. Three real catalog entries are like this,
	// and every locally-served model has no entry at all.
	ck.Nil(by["a/unknown"].SupportedParameters, "SupportedParameters")
	// created carries through so a list can be ordered newest-first.
	ck.Eq(300, by["a/unknown"].Created, "Created")
}

func TestCatalogDecodesCutoffAndAgenticScore(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":[
	 {"id":"a/scored","created":1,"context_length":1000,
	  "knowledge_cutoff":"2026-02-16",
	  "benchmarks":{"artificial_analysis":{"intelligence_index":65.7,
	    "coding_index":81.6,"agentic_index":59.2}}},
	 {"id":"b/unscored","created":2,"context_length":1000}
	]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	by := map[string]CatalogRow{}
	for _, r := range c.Rows() {
		by[r.ID] = r
	}

	ck.Eq("2026-02-16", by["a/scored"].KnowledgeCutoff, "KnowledgeCutoff")
	if got := by["a/scored"].AgenticIndex; got == nil || *got != 59.2 {
		t.Errorf("AgenticIndex = %v, want 59.2", got)
	}
	// Absent is UNSCORED, never zero: 62% of the live catalog carries no
	// benchmark at all, and a zero would sort as the worst model rather than
	// as no answer.
	ck.Nil(by["b/unscored"].AgenticIndex, "AgenticIndex")
	ck.Eq("", by["b/unscored"].KnowledgeCutoff, "KnowledgeCutoff")
}

// The three artificial_analysis indices arrive together, so a row with one has
// all three and a row with none has none.
func TestCatalogDecodesAllThreeBenchmarkScores(t *testing.T) {
	ck := assert.NewCollecting(t)
	body := `{"data":[{"id":"a/scored","created":1,"context_length":1000,
	 "benchmarks":{"artificial_analysis":{"intelligence_index":65.7,
	   "coding_index":81.6,"agentic_index":59.2}}}]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	r := c.Rows()[0]
	ck.False(r.IntelligenceIndex == nil || *r.IntelligenceIndex != 65.7, "IntelligenceIndex = %v, want 65.7", r.IntelligenceIndex)
	ck.False(r.CodingIndex == nil || *r.CodingIndex != 81.6, "CodingIndex = %v, want 81.6", r.CodingIndex)
	ck.False(r.AgenticIndex == nil || *r.AgenticIndex != 59.2, "AgenticIndex = %v, want 59.2", r.AgenticIndex)
}

// OpenRouter lists a ":batch" twin beside each Anthropic model with the SAME
// created stamp, so a tie-broken-by-order pick could land on the twin and
// hand the native sender "claude-opus-5-5:batch". Seed the twin FIRST.
func TestResolveLatestSkipsVariantTwins(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-opus-5.5:batch", Created: 9},
		{ID: "anthropic/claude-opus-5.5", Created: 9},
		{ID: "anthropic/claude-opus-5", Created: 5},
	})
	ant, or, ok := c.ResolveLatest("opus")
	assert.NewAborting(t).False(!ok || ant != "claude-opus-5-5" || or != "anthropic/claude-opus-5.5", "opus: got (%q,%q,%v)", ant, or, ok)
}

// AnthropicID is the native-sender spelling of a catalog row, and empty for
// anything the native sender cannot run: other vendors, and the ~alias,
// ":batch" and "-fast" variants.
func TestRowsCarryAnthropicID(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-opus-5.5", Created: 1},
		{ID: "anthropic/claude-opus-5.5:batch", Created: 1},
		{ID: "anthropic/claude-opus-4.8-fast", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 1},
		{ID: "openai/gpt-4o", Created: 1},
	})
	want := map[string]string{
		"anthropic/claude-opus-5.5":       "claude-opus-5-5",
		"anthropic/claude-opus-5.5:batch": "",
		"anthropic/claude-opus-4.8-fast":  "",
		"anthropic/claude-sonnet-5":       "claude-sonnet-5",
		"openai/gpt-4o":                   "",
	}
	for _, r := range c.Rows() {
		got := r.AnthropicID
		assert.NewCollecting(t).Eq(want[r.ID], got, "%s: AnthropicID = %q, want", r.ID, got)
	}
}
