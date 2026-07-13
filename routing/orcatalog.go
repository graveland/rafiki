package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/timescale/savannah-common/go/tslogs"
)

const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// latestFamilies are the Anthropic family names for which "<family>-latest"
// resolves. This is a set of family *names*, not model versions: it changes only
// when Anthropic introduces a new family, and unlike concrete model ids it does
// not drift — the actual newest version per family is resolved live from the
// OpenRouter catalog. No concrete model ids are hardcoded anywhere.
var latestFamilies = map[string]bool{"haiku": true, "sonnet": true, "opus": true, "fable": true}

// LatestFamilies returns the "<family>-latest" aliases, sorted, for completion.
func LatestFamilies() []string {
	out := make([]string, 0, len(latestFamilies))
	for fam := range latestFamilies {
		out = append(out, fam+"-latest")
	}
	sort.Strings(out)
	return out
}

// ResolveModel maps a requested model to the concrete id to send upstream:
// empty -> defaultModel; a "<family>-latest" alias -> the catalog's newest for
// that family; anything else (a concrete Anthropic id or a slash OpenRouter id)
// unchanged. A "<family>-latest" alias errors when the catalog can't resolve it
// (nil catalog, or offline with a cold cache) rather than sending an unusable
// alias upstream — there is no hardcoded fallback list.
func ResolveModel(cat *ModelCatalog, defaultModel, requested string) (string, error) {
	if requested == "" {
		requested = defaultModel
	}
	fam, ok := LatestAlias(requested)
	if !ok {
		return requested, nil
	}
	if cat != nil {
		if antID, _, resolved := cat.ResolveLatest(fam); resolved {
			return antID, nil
		}
	}
	return "", fmt.Errorf("cannot resolve %q: OpenRouter model catalog unavailable", requested)
}

// LatestAlias reports whether model is a "<family>-latest" alias (bare,
// "claude-<family>-latest", or OR's "~anthropic/claude-<family>-latest") and
// returns the family.
func LatestAlias(model string) (string, bool) {
	m := strings.TrimPrefix(model, "~")
	m = strings.TrimPrefix(m, "anthropic/")
	m = strings.TrimPrefix(m, "claude-")
	fam, isLatest := strings.CutSuffix(m, "-latest")
	if !isLatest || !latestFamilies[fam] {
		return "", false
	}
	return fam, true
}

type orModel struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
}

// ModelCatalog fetches and caches the OpenRouter model list, and resolves the
// newest Anthropic model per family. It is the failover-safe source of truth
// for "latest": whatever it returns is present on OpenRouter. Callers get a
// TTL-cached snapshot from the last successful refresh — it is not
// re-verified on every call, so during an OpenRouter outage it can keep
// serving a stale (but previously valid) id until the next successful fetch.
type ModelCatalog struct {
	http   *http.Client
	url    string
	ttl    time.Duration
	logger *tslogs.Logger

	sf singleflight.Group // coalesces concurrent refreshes

	mu      sync.Mutex
	models  []orModel
	fetched time.Time
}

func NewModelCatalog(httpClient *http.Client, ttl time.Duration, logger *tslogs.Logger) *ModelCatalog {
	return &ModelCatalog{http: httpClient, url: openRouterModelsURL, ttl: ttl, logger: logger}
}

// refresh reloads the catalog when the cache is empty or stale. Concurrent
// refreshes are coalesced via singleflight: the shared server catalog is hit by
// every diagnose request and proxy resolve, so without this a cold/expired cache
// under load would fire N parallel GETs to OpenRouter. A fetch error is logged
// and the previous snapshot is kept (best-effort).
func (c *ModelCatalog) refresh() {
	if c.fresh() {
		return
	}
	c.sf.Do("refresh", func() (any, error) { //nolint:errcheck // fetch logs its own errors
		if c.fresh() { // a caller queued behind a just-finished refresh needn't fetch
			return nil, nil
		}
		c.fetch()
		return nil, nil
	})
}

// Warm triggers a best-effort catalog refresh. Call it (in a goroutine) at
// startup so the first request doesn't pay the fetch latency and a snapshot is
// populated before it's needed — narrowing the cold-cache window in which a
// "<family>-latest" default can't resolve. A fetch failure is logged and left
// for the next lazy refresh; there is deliberately no hardcoded fallback.
func (c *ModelCatalog) Warm() { c.refresh() }

func (c *ModelCatalog) fresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.fetched.IsZero() && time.Since(c.fetched) < c.ttl
}

func (c *ModelCatalog) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		c.logger.Warn("model catalog: build request failed", "error", err)
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Warn("model catalog: fetch failed (keeping cached)", "error", err)
		return
	}
	defer resp.Body.Close()
	var payload struct {
		Data []orModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		c.logger.Warn("model catalog: decode failed (keeping cached)", "error", err)
		return
	}
	c.mu.Lock()
	c.models = payload.Data
	c.fetched = time.Now()
	c.mu.Unlock()
}

// ResolveLatest returns the newest anthropic/claude-<family>-* model (excluding
// ~aliases and -fast variants) as (anthropicID, orID). ok=false if unresolved.
func (c *ModelCatalog) ResolveLatest(family string) (string, string, bool) {
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := "anthropic/claude-" + family
	var best orModel
	for _, m := range c.models {
		if strings.HasPrefix(m.ID, "~") || strings.HasSuffix(m.ID, "-fast") {
			continue
		}
		if !strings.HasPrefix(m.ID, prefix) {
			continue
		}
		if best.ID == "" || m.Created > best.Created {
			best = m
		}
	}
	if best.ID == "" {
		return "", "", false
	}
	return anthropicIDFromOR(best.ID), best.ID, true
}

// anthropicIDFromOR derives the Anthropic API id from an OR id:
// "anthropic/claude-opus-4.8" -> "claude-opus-4-8".
func anthropicIDFromOR(orID string) string {
	return strings.ReplaceAll(strings.TrimPrefix(orID, "anthropic/"), ".", "-")
}

// OpenRouterModel returns the OpenRouter id for an Anthropic model id, used on
// failover. A slash id (already OpenRouter-native) passes through unchanged.
// Otherwise the catalog is the source of truth: the anthropic/* entry whose
// derived Anthropic id matches is returned by its real OR id, so failover never
// depends on a hand-maintained reverse map. Falls back to a best-effort
// "anthropic/"+id only when the catalog can't resolve it (nil receiver, or
// offline with a cold cache).
func (c *ModelCatalog) OpenRouterModel(anthropicModel string) string {
	if strings.Contains(anthropicModel, "/") {
		return anthropicModel
	}
	if c != nil {
		c.refresh()
		c.mu.Lock()
		for _, m := range c.models {
			if strings.HasPrefix(m.ID, "~") {
				continue
			}
			if strings.HasPrefix(m.ID, "anthropic/") && anthropicIDFromOR(m.ID) == anthropicModel {
				id := m.ID
				c.mu.Unlock()
				return id
			}
		}
		c.mu.Unlock()
	}
	return "anthropic/" + anthropicModel
}

// AllIDs returns every catalog model id (leading "~" auto-alias marker
// stripped), sorted + de-duplicated, for client-side completion. Empty if the
// catalog hasn't loaded.
func (c *ModelCatalog) AllIDs() []string {
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]bool, len(c.models))
	ids := make([]string, 0, len(c.models))
	for _, m := range c.models {
		id := strings.TrimPrefix(m.ID, "~")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// CatalogEntry is the exported shape for test seeding.
type CatalogEntry struct {
	ID      string
	Created int64
}

// SeedForTest injects catalog entries without a network fetch (tests only).
func (c *ModelCatalog) SeedForTest(entries []CatalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = make([]orModel, len(entries))
	for i, e := range entries {
		c.models[i] = orModel{ID: e.ID, Created: e.Created}
	}
	c.fetched = time.Now().Add(c.ttl) // never refresh in tests
}
