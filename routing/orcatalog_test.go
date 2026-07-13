package routing

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timescale/savannah-common/go/tslogs"
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
	c := NewModelCatalog(srv.Client(), time.Minute, tslogs.NewDiscardingLogger())
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
	c := NewModelCatalog(nil, time.Minute, tslogs.NewDiscardingLogger())
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

func TestResolveModel(t *testing.T) {
	c := NewModelCatalog(nil, time.Minute, tslogs.NewDiscardingLogger())
	c.SeedForTest([]CatalogEntry{
		{ID: "anthropic/claude-haiku-4.5", Created: 1},
		{ID: "anthropic/claude-sonnet-5", Created: 2},
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
	mustResolve("haiku-latest", "openai/gpt-4o", "openai/gpt-4o")     // slash passthrough

	// A "<family>-latest" the catalog can't resolve errors — there is no hardcoded
	// fallback list to silently paper over it with a stale id.
	if _, err := ResolveModel(c, "", "opus-latest"); err == nil {
		t.Error("opus-latest absent from catalog must error, not fall back to a hardcoded id")
	}
	if _, err := ResolveModel(nil, "", "haiku-latest"); err == nil {
		t.Error("nil catalog + -latest must error")
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
	c := NewModelCatalog(srv.Client(), time.Minute, tslogs.NewDiscardingLogger())
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
	c := NewModelCatalog(nil, time.Minute, tslogs.NewDiscardingLogger())
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
