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
	c, srv := newTestCatalog(t, orFixture)
	defer srv.Close()

	// sonnet-latest → newest non-alias, non-fast sonnet = claude-sonnet-5
	ant, or, ok := c.ResolveLatest("sonnet")
	if !ok || ant != "claude-sonnet-5" || or != "anthropic/claude-sonnet-5" {
		t.Fatalf("sonnet: got (%q,%q,%v)", ant, or, ok)
	}
	// opus-latest → 4.8 (the -fast variant is excluded)
	ant, or, ok = c.ResolveLatest("opus")
	if !ok || ant != "claude-opus-4-8" || or != "anthropic/claude-opus-4.8" {
		t.Fatalf("opus: got (%q,%q,%v)", ant, or, ok)
	}
}

func TestLatestAlias(t *testing.T) {
	for _, in := range []string{"haiku-latest", "claude-haiku-latest", "~anthropic/claude-haiku-latest"} {
		if fam, ok := LatestAlias(in); !ok || fam != "haiku" {
			t.Errorf("LatestAlias(%q) = (%q,%v)", in, fam, ok)
		}
	}
	if _, ok := LatestAlias("claude-sonnet-5"); ok {
		t.Error("concrete id must not be a latest alias")
	}
}

// TestOpenRouterModel covers the catalog-backed reverse lookup that replaced the
// hand-maintained anthropicToOpenRouter map. The point is that an id present in
// the catalog resolves to its real dotted OR id with no hardcoded map — so
// failover stays valid as new models ship (the map-drift bug this fixes).
func TestOpenRouterModel(t *testing.T) {
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
		if got := c.OpenRouterModel(in); got != want {
			t.Errorf("OpenRouterModel(%q) = %q, want %q", in, got, want)
		}
	}
	var nilCat *ModelCatalog
	if got := nilCat.OpenRouterModel("claude-opus-4-8"); got != "anthropic/claude-opus-4-8" {
		t.Errorf("nil-catalog fallback = %q", got)
	}
	if got := nilCat.OpenRouterModel("openai/gpt-4o"); got != "openai/gpt-4o" {
		t.Errorf("nil-catalog slash passthrough = %q", got)
	}
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
	if got, ok := c.ResolveNewest("deepseek/deepseek-v5"); ok {
		t.Errorf("absent line must not resolve, got %q", got)
	}
}

func TestModelAliases(t *testing.T) {
	got := ModelAliases()
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2", "kimi-k3"}
	if len(got) != len(want) {
		t.Fatalf("ModelAliases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ModelAliases = %v, want %v", got, want)
		}
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
	if _, err := ResolveModel(c, "", ""); err == nil {
		t.Error("empty model + empty default must error, not silently pick a model")
	}
}

// TestProviderPrefsFor covers provider pinning: a pinned line matches its
// base id and stamped point releases (same inModelLine semantics as aliases),
// never a different line, and unpinned models carry no preferences.
func TestProviderPrefsFor(t *testing.T) {
	for _, id := range []string{"z-ai/glm-5.2", "z-ai/glm-5.2-0905"} {
		prefs, ok := ProviderPrefsFor(id)
		if !ok {
			t.Errorf("%s: want a pin", id)
			continue
		}
		if len(prefs.Only) != 1 || prefs.Only[0] != "fireworks" {
			t.Errorf("%s: Only = %v, want [fireworks]", id, prefs.Only)
		}
	}
	for _, id := range []string{"z-ai/glm-5.20", "z-ai/glm-5", "z-ai/glm-5.2.1", "moonshotai/kimi-k3", "claude-haiku-4-5"} {
		if _, ok := ProviderPrefsFor(id); ok {
			t.Errorf("%s: must not be pinned", id)
		}
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

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("catalog fetched %d times under %d concurrent callers, want 1", got, n)
	}
}

func TestAllIDs(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest([]CatalogEntry{
		{ID: "openai/gpt-4o", Created: 1},
		{ID: "anthropic/claude-opus-4.8", Created: 2},
		{ID: "~anthropic/claude-sonnet-latest", Created: 3},
		{ID: "openai/gpt-4o", Created: 4}, // dup
	})
	got := c.AllIDs()
	want := []string{"anthropic/claude-opus-4.8", "anthropic/claude-sonnet-latest", "openai/gpt-4o"}
	if len(got) != len(want) {
		t.Fatalf("AllIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllIDs = %v, want %v", got, want)
		}
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
		if got := AutoCompactWindow(c.contextLen, c.maxComp); got != c.want {
			t.Errorf("%s: AutoCompactWindow(%d,%d) = %d, want %d", c.name, c.contextLen, c.maxComp, got, c.want)
		}
	}
}

func TestContextWindow(t *testing.T) {
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
		if ok != tc.wantOK || gotCtx != tc.wantCtx || gotMax != tc.wantMax {
			t.Errorf("%s: ContextWindow(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tc.name, tc.model, gotCtx, gotMax, ok, tc.wantCtx, tc.wantMax, tc.wantOK)
		}
	}
	var nilCat *ModelCatalog
	if _, _, ok := nilCat.ContextWindow("openai/gpt-5-codex"); ok {
		t.Error("nil catalog must return ok=false")
	}
}

// memStore is an in-memory SnapshotStore for tests. An empty store returns
// (nil,nil), which the catalog treats as a cold cache.
type memStore struct{ data []byte }

func (m *memStore) Load() ([]byte, error) { return m.data, nil }
func (m *memStore) Save(b []byte) error   { m.data = b; return nil }

func TestModelCatalogCache(t *testing.T) {
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
	if got := hits.Load(); got != 1 {
		t.Fatalf("cold cache should fetch once, got %d hits", got)
	}
	if len(store.data) == 0 {
		t.Fatal("fetch must persist a snapshot to the store")
	}

	// A fresh process sharing the warm store must not hit the network.
	c2 := NewModelCatalog(srv.Client(), time.Hour, slog.New(slog.DiscardHandler))
	c2.url = srv.URL
	c2.WithCache(store)
	if ctxLen, _, ok := c2.ContextWindow("openai/gpt-5-codex"); !ok || ctxLen != 400000 {
		t.Fatalf("c2 ContextWindow from cache = (%d,%v), want (400000,true)", ctxLen, ok)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("warm cache must not re-fetch, got %d hits (want 1)", got)
	}
}

// TestModelPricingWireDecode guards the json tags on orPricing against the live
// OpenRouter wire shape: the pricing object was previously dropped on decode.
func TestModelPricingWireDecode(t *testing.T) {
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
	if !ok {
		t.Fatal("pricing not decoded from wire")
	}
	if p.PromptUSD != 0.000002 || p.CompletionUSD != 0.00001 {
		t.Errorf("base price = prompt %g / completion %g, want 0.000002/0.00001", p.PromptUSD, p.CompletionUSD)
	}
	if p.CacheReadUSD != 0.0000002 || p.CacheWriteUSD != 0.0000025 || p.CacheWrite1hUSD != 0.000004 {
		t.Errorf("cache prices = read %g / write %g / write1h %g, want 0.0000002/0.0000025/0.000004",
			p.CacheReadUSD, p.CacheWriteUSD, p.CacheWrite1hUSD)
	}
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
	if _, ok := c.Pricing("moonshotai/kimi-k3"); ok {
		t.Error("model with no price strings must resolve ok=false")
	}
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
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("stripSnapshotDate(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
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
	if got := hits.Load(); got != 1 {
		t.Errorf("fetch hits = %d, want 1 (failed fetch backs off for %s)", got, fetchBackoff)
	}
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
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ResolveID(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestModelCatalogPriceSatisfiesStorePriceSource proves Price reports the
// catalog's prices in the shape store.SyncModelPricing writes.
func TestModelCatalogPriceSatisfiesStorePriceSource(t *testing.T) {
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
	if !ok {
		t.Fatal("Price(claude-opus-5) ok = false, want true")
	}
	if got.PromptUSD != 0.000005 || got.CompletionUSD != 0.000025 {
		t.Errorf("Price(claude-opus-5) base = %g/%g, want 0.000005/0.000025",
			got.PromptUSD, got.CompletionUSD)
	}
	if got.CacheReadUSD == nil || *got.CacheReadUSD != 0.0000005 {
		t.Errorf("Price(claude-opus-5) cache read = %v, want 0.0000005", got.CacheReadUSD)
	}
	if got.CacheWriteUSD == nil || *got.CacheWriteUSD != 0.00000625 {
		t.Errorf("Price(claude-opus-5) cache write = %v, want 0.00000625", got.CacheWriteUSD)
	}

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
	if !ok {
		t.Fatal("Price ok = false, want true: base prices are present")
	}
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
	c := NewModelCatalog(nil, time.Hour, nil)
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-opus-5", Pricing: &ModelPricing{
			PromptUSD: 0.000005, CompletionUSD: 0.000025, CacheReadUSD: 0.0000005,
		}},
		{ID: "moonshotai/kimi-k3"}, // in the catalog, but unpriced
	})

	// A bare Anthropic id resolves, and reports the same id ResolveID does.
	info, ok := c.Lookup("claude-opus-5")
	if !ok {
		t.Fatal("Lookup(claude-opus-5) ok = false, want true")
	}
	if info.ORID != "anthropic/claude-opus-5" {
		t.Errorf("ORID = %q, want anthropic/claude-opus-5", info.ORID)
	}
	if !info.Priced || info.Price.PromptUSD != 0.000005 {
		t.Errorf("Lookup(claude-opus-5) = %+v, want priced with prompt 0.000005", info)
	}
	if id, _ := c.ResolveID("claude-opus-5"); id != info.ORID {
		t.Errorf("Lookup ORID %q disagrees with ResolveID %q", info.ORID, id)
	}

	// An entry with no prices is still found — the syncer records the row with
	// its or_id and NULL prices rather than dropping the model.
	info, ok = c.Lookup("kimi-k3")
	if !ok {
		t.Fatal("Lookup(kimi-k3) ok = false, want true: the entry exists")
	}
	if info.ORID != "moonshotai/kimi-k3" {
		t.Errorf("ORID = %q, want moonshotai/kimi-k3", info.ORID)
	}
	if info.Priced {
		t.Errorf("Lookup(kimi-k3) Priced = true, want false: entry has no prices")
	}

	if _, ok := c.Lookup("gpt-5.6"); ok {
		t.Error("Lookup(gpt-5.6) ok = true, want false: not in catalog")
	}
}

// A typed-nil *ModelCatalog reaches store.SyncModelPricing as a NON-nil
// store.PriceSource, so the syncer's `src == nil` check cannot catch it. Every
// interface method must survive it: the sync runs in a bare goroutine with no
// recover, so a panic here takes the server down.
func TestNilCatalogSatisfiesPriceSourceWithoutPanic(t *testing.T) {
	var src store.PriceSource = (*ModelCatalog)(nil)

	src.Warm()
	if ids := src.AllIDs(); len(ids) != 0 {
		t.Errorf("AllIDs on a nil catalog = %v, want empty", ids)
	}
	if _, ok := src.Lookup("claude-opus-5"); ok {
		t.Error("Lookup on a nil catalog ok = true, want false")
	}
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
			b, err := json.Marshal(tc.prefs)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("Marshal = %s, want %s", b, tc.want)
			}
		})
	}
}

func TestCatalogDecodesNameAndInputModalities(t *testing.T) {
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
	if got.Name != "GPT-4o" {
		t.Errorf("Name = %q, want %q", got.Name, "GPT-4o")
	}
	if len(got.InputModalities) != 2 || got.InputModalities[0] != "text" ||
		got.InputModalities[1] != "image" {
		t.Errorf("InputModalities = %v, want [text image]", got.InputModalities)
	}
	if got.ContextLength == nil || *got.ContextLength != 128000 {
		t.Errorf("ContextLength = %v, want 128000", got.ContextLength)
	}
	if got.PromptUSD == nil || *got.PromptUSD != 0.000005 {
		t.Errorf("PromptUSD = %v, want 0.000005", got.PromptUSD)
	}
	if got.CompletionUSD == nil || *got.CompletionUSD != 0.000015 {
		t.Errorf("CompletionUSD = %v, want 0.000015", got.CompletionUSD)
	}
	// The fixture prices no cache rates: those must be ABSENT, not zero.
	if got.CacheReadUSD != nil || got.CacheWriteUSD != nil {
		t.Errorf("cache prices = %v/%v, want nil for a model OpenRouter prices without them", got.CacheReadUSD, got.CacheWriteUSD)
	}

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
	body := `{"data":[{"id":"openai/bare","created":1,"context_length":4096}]}`
	c, srv := newTestCatalog(t, body)
	defer srv.Close()

	rows := c.Rows()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].InputModalities != nil {
		t.Errorf("InputModalities = %#v, want nil", rows[0].InputModalities)
	}
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
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 from the stale snapshot", len(rows))
	}
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
		if strings.HasPrefix(r.ID, "~") {
			t.Errorf("Rows() returned alias id %q", r.ID)
		}
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
	if priced.ID == "" {
		t.Fatal("priced model missing from Rows()")
	}
	if priced.PromptUSD == nil || *priced.PromptUSD != 0.000005 {
		t.Errorf("priced PromptUSD = %v, want 0.000005", priced.PromptUSD)
	}
	if priced.CompletionUSD == nil || *priced.CompletionUSD != 0.000015 {
		t.Errorf("priced CompletionUSD = %v, want 0.000015", priced.CompletionUSD)
	}
	if priced.ContextLength == nil || *priced.ContextLength != 128000 {
		t.Errorf("priced ContextLength = %v, want 128000", priced.ContextLength)
	}
	if priced.MaxCompletionTokens == nil || *priced.MaxCompletionTokens != 16384 {
		t.Errorf("priced MaxCompletionTokens = %v, want 16384", priced.MaxCompletionTokens)
	}
	if priced.CacheReadUSD != nil {
		t.Errorf("priced CacheReadUSD = %v, want nil — an unreported cache rate must not read as free caching", *priced.CacheReadUSD)
	}
	if priced.CacheWriteUSD != nil {
		t.Errorf("priced CacheWriteUSD = %v, want nil", *priced.CacheWriteUSD)
	}

	// unpriced: prompt/completion absent too.
	unpriced := byID["openai/unpriced"]
	if unpriced.ID == "" {
		t.Fatal("unpriced model missing from Rows()")
	}
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
	if zeroed.ID == "" {
		t.Fatal("zeroed model missing from Rows()")
	}
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
		if *got != 0 {
			t.Errorf("reported-zero %s = %v, want 0", name, *got)
		}
	}
	if zeroed.ContextLength == nil || *zeroed.ContextLength != 0 {
		t.Errorf("reported-zero ContextLength = %v, want a pointer to 0", zeroed.ContextLength)
	}
	if zeroed.MaxCompletionTokens == nil || *zeroed.MaxCompletionTokens != 0 {
		t.Errorf("reported-zero MaxCompletionTokens = %v, want a pointer to 0", zeroed.MaxCompletionTokens)
	}
}

func TestCatalogDecodesToolSupportAndExpiry(t *testing.T) {
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
	if got := by["a/tools"].ExpiresAt; got != "2026-09-08" {
		t.Errorf("ExpiresAt = %q, want 2026-09-08", got)
	}
	if got := by["a/no-tools"].SupportedParameters; len(got) != 1 {
		t.Errorf("SupportedParameters = %v, want the one declared", got)
	}
	if got := by["a/no-tools"].ExpiresAt; got != "" {
		t.Errorf("ExpiresAt = %q, want empty for an entry with no expiry", got)
	}
	// An entry with NO list means UNKNOWN, not "supports nothing" -- the same
	// rule InputModalities follows. Three real catalog entries are like this,
	// and every locally-served model has no entry at all.
	if got := by["a/unknown"].SupportedParameters; got != nil {
		t.Errorf("SupportedParameters = %#v, want nil for unknown", got)
	}
	// created carries through so a list can be ordered newest-first.
	if got := by["a/unknown"].Created; got != 300 {
		t.Errorf("Created = %d, want 300", got)
	}
}

func TestCatalogDecodesCutoffAndAgenticScore(t *testing.T) {
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

	if got := by["a/scored"].KnowledgeCutoff; got != "2026-02-16" {
		t.Errorf("KnowledgeCutoff = %q, want 2026-02-16", got)
	}
	if got := by["a/scored"].AgenticIndex; got == nil || *got != 59.2 {
		t.Errorf("AgenticIndex = %v, want 59.2", got)
	}
	// Absent is UNSCORED, never zero: 62% of the live catalog carries no
	// benchmark at all, and a zero would sort as the worst model rather than
	// as no answer.
	if got := by["b/unscored"].AgenticIndex; got != nil {
		t.Errorf("AgenticIndex = %v, want nil for an unscored model", got)
	}
	if got := by["b/unscored"].KnowledgeCutoff; got != "" {
		t.Errorf("KnowledgeCutoff = %q, want empty", got)
	}
}
