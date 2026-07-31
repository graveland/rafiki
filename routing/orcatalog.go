// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/timescale/rafiki/store"
	"github.com/timescale/savannah-common/go/tslogs"
	"golang.org/x/sync/singleflight"
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

// This maps a shorthand model name easily intuited by engineers to its OpenRouter "slash" id
var modelAliases = map[string]string{
	"kimi-k3":           "moonshotai/kimi-k3",
	"deepseek-v4-pro":   "deepseek/deepseek-v4-pro",
	"deepseek-v4-flash": "deepseek/deepseek-v4-flash",
	"glm-5.2":           "z-ai/glm-5.2",
}

// ProviderPrefs is OpenRouter's provider-routing request object, sent as the
// body's "provider" field. This includes settings for which providers we want
// to host the model, giving us control over things like data governance policies
// and quantization settings
type ProviderPrefs struct {
	// Only restricts serving to these OpenRouter provider slugs.
	Only []string `json:"only,omitempty"`
}

// providerPins maps an OpenRouter model-line prefix (same line semantics as
// modelAliases/inModelLine) to the provider preferences required when routing
// that line. Some open-weight models are served by dozens of providers of
// wildly varying quantization/retention policy; a pin here restricts routing
// to vetted ones. Applied wherever a request is sent to OpenRouter — alias or
// explicit slash id — unless the caller supplied its own provider object.
var providerPins = map[string]ProviderPrefs{
	// GLM 5.2's cheap default endpoints are fp4 and/or prompt-retaining;
	// Fireworks is the vetted no-retention host.
	"z-ai/glm-5.2": {Only: []string{"fireworks"}},
}

// ProviderPrefsFor returns the provider-routing preferences pinned for an
// OpenRouter model id, matching by model line ("z-ai/glm-5.2" and a stamped
// point release like "z-ai/glm-5.2-0905" both match the "z-ai/glm-5.2" pin).
// ok=false when the model has no pin.
func ProviderPrefsFor(orID string) (ProviderPrefs, bool) {
	for prefix, prefs := range providerPins {
		if inModelLine(orID, prefix) {
			return prefs, true
		}
	}
	return ProviderPrefs{}, false
}

// ModelAliases returns the short model aliases, sorted, for completion.
func ModelAliases() []string {
	out := make([]string, 0, len(modelAliases))
	for a := range modelAliases {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ResolveModel maps a requested model to the concrete id to send upstream:
// empty -> defaultModel; a "<family>-latest" alias -> the catalog's newest for
// that family; a short model alias (kimi-k3) -> the catalog's newest release
// of that model line; anything else (a concrete Anthropic id or a slash
// OpenRouter id) unchanged. An alias errors when the catalog can't resolve it
// (nil catalog, or offline with a cold cache) rather than sending an unusable
// alias upstream — there is no hardcoded fallback list.
func ResolveModel(cat *ModelCatalog, defaultModel, requested string) (string, error) {
	if requested == "" {
		requested = defaultModel
	}
	if fam, ok := LatestAlias(requested); ok {
		if cat != nil {
			if antID, _, resolved := cat.ResolveLatest(fam); resolved {
				return antID, nil
			}
		}
		return "", fmt.Errorf("cannot resolve %q: OpenRouter model catalog unavailable", requested)
	}
	if prefix, ok := modelAliases[requested]; ok {
		if cat != nil {
			if orID, resolved := cat.ResolveNewest(prefix); resolved {
				return orID, nil
			}
		}
		return "", fmt.Errorf("cannot resolve %q: OpenRouter model catalog unavailable", requested)
	}
	// A slash id routes to OpenRouter unchanged — except OpenRouter's auto-latest
	// aliases carry a leading "~" (e.g. ~openai/gpt-latest) that AllIDs strips for
	// shell-safety and users drop when copy-pasting, which OpenRouter 400s. Re-add
	// it when the catalog confirms the tilde form is the real id and the bare one
	// isn't, so both the completion-suggested and hand-typed bare forms resolve.
	if cat != nil && strings.Contains(requested, "/") && !strings.HasPrefix(requested, "~") {
		if norm, changed := cat.normalizeTilde(requested); changed {
			return norm, nil
		}
	}
	return requested, nil
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
	ID          string `json:"id"`
	Created     int64  `json:"created"`
	ContextLen  int    `json:"context_length"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	// Pricing is OpenRouter's per-token USD price object. A cached snapshot
	// written before this field existed decodes with empty strings, so its
	// models resolve as unpriced until the next successful refresh repopulates
	// pricing — acceptable, since the stale snapshot's ids still resolve.
	Pricing orPricing `json:"pricing"`
}

// orPricing is OpenRouter's per-token USD price strings. prompt/completion are
// always present; the cache fields are omitted for models without prompt
// caching. Values are decimal USD-per-token (e.g. "0.000002").
type orPricing struct {
	Prompt            string `json:"prompt"`
	Completion        string `json:"completion"`
	InputCacheRead    string `json:"input_cache_read"`
	InputCacheWrite   string `json:"input_cache_write"`
	InputCacheWrite1h string `json:"input_cache_write_1h"`
}

// ModelPricing is a parsed per-token USD price for a model. Cache fields are 0
// when the model or the OpenRouter entry doesn't price them.
type ModelPricing struct {
	PromptUSD       float64
	CompletionUSD   float64
	CacheReadUSD    float64
	CacheWriteUSD   float64
	CacheWrite1hUSD float64
}

// pricingFromOR parses an orPricing into ModelPricing. ok=false unless both the
// prompt and completion prices parse (a model with no usable base price is
// treated as unpriced); cache fields parse best-effort to 0.
func pricingFromOR(p orPricing) (ModelPricing, bool) {
	prompt, err := strconv.ParseFloat(p.Prompt, 64)
	if err != nil {
		return ModelPricing{}, false
	}
	completion, err := strconv.ParseFloat(p.Completion, 64)
	if err != nil {
		return ModelPricing{}, false
	}
	return ModelPricing{
		PromptUSD:       prompt,
		CompletionUSD:   completion,
		CacheReadUSD:    parsePriceOrZero(p.InputCacheRead),
		CacheWriteUSD:   parsePriceOrZero(p.InputCacheWrite),
		CacheWrite1hUSD: parsePriceOrZero(p.InputCacheWrite1h),
	}, true
}

func parsePriceOrZero(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
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
	store  SnapshotStore // optional cross-process cache; nil = memory-only

	sf singleflight.Group // coalesces concurrent refreshes

	mu       sync.Mutex
	models   []orModel
	fetched  time.Time
	lastFail time.Time // last failed fetch; gates refresh backoff (see fetchBackoff)
}

// fetchBackoff is how long refresh() waits after a failed fetch before trying
// again. Without it, a cold cache during an OpenRouter outage would fire a
// fresh GET on every single call (each resolve/ContextWindow/Pricing), hammering
// a dead endpoint; the backoff caps that to one attempt per interval.
const fetchBackoff = 30 * time.Second

// SnapshotStore persists the catalog's JSON snapshot between processes so a
// short-lived host (a CLI launch) reuses a warm catalog instead of re-fetching.
// The host supplies the storage; the catalog stays free of filesystem policy.
// Load returns the last saved bytes, or an error — any error (including a
// missing entry) is treated as a cold cache, so a "not found" need not be
// distinguished. The bytes are opaque to the host (the catalog owns the schema).
type SnapshotStore interface {
	Load() ([]byte, error)
	Save([]byte) error
}

func NewModelCatalog(httpClient *http.Client, ttl time.Duration, logger *tslogs.Logger) *ModelCatalog {
	return &ModelCatalog{http: httpClient, url: openRouterModelsURL, ttl: ttl, logger: logger}
}

// WithCache wires a SnapshotStore and loads any existing snapshot immediately, so
// a short-lived process reuses a warm catalog across invocations. A snapshot
// within the catalog's ttl satisfies fresh() and skips the network entirely; a
// stale one is still loaded so an offline fetch keeps serving it. Best-effort: a
// missing or unreadable snapshot just leaves a cold in-memory cache. Returns the
// receiver for chaining.
func (c *ModelCatalog) WithCache(store SnapshotStore) *ModelCatalog {
	c.store = store
	c.loadCache()
	return c
}

type catalogSnapshot struct {
	Fetched time.Time `json:"fetched"`
	Models  []orModel `json:"models"`
}

func (c *ModelCatalog) loadCache() {
	if c.store == nil {
		return
	}
	data, err := c.store.Load()
	if err != nil || len(data) == 0 {
		return // cold cache
	}
	var snap catalogSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		c.logger.Warn("model catalog: ignoring unreadable cache snapshot", "error", err)
		return
	}
	if len(snap.Models) == 0 {
		return // empty/old-schema snapshot: treat as cold so refresh() refetches
	}
	c.mu.Lock()
	c.models = snap.Models
	c.fetched = snap.Fetched
	c.mu.Unlock()
}

func (c *ModelCatalog) saveCache() {
	if c.store == nil {
		return
	}
	c.mu.Lock()
	snap := catalogSnapshot{Fetched: c.fetched, Models: c.models}
	c.mu.Unlock()
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := c.store.Save(data); err != nil {
		c.logger.Warn("model catalog: cache save failed", "error", err)
	}
}

// refresh reloads the catalog when the cache is empty or stale. Concurrent
// refreshes are coalesced via singleflight: the shared server catalog is hit by
// every diagnose request and proxy resolve, so without this a cold/expired cache
// under load would fire N parallel GETs to OpenRouter. A fetch error is logged
// and the previous snapshot is kept (best-effort).
func (c *ModelCatalog) refresh() {
	if c == nil {
		return
	}
	if c.fresh() || c.backingOff() {
		return
	}
	c.sf.Do("refresh", func() (any, error) { //nolint:errcheck // fetch logs its own errors
		if c.fresh() || c.backingOff() { // a caller queued behind a just-finished refresh needn't fetch
			return nil, nil
		}
		c.fetch()
		return nil, nil
	})
}

// backingOff reports whether the last fetch failed within fetchBackoff, in
// which case refresh() skips the network and keeps serving the current (stale
// or cold) snapshot.
func (c *ModelCatalog) backingOff() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.lastFail.IsZero() && time.Since(c.lastFail) < fetchBackoff
}

// Warm triggers a best-effort catalog refresh. Call it (in a goroutine) at
// startup so the first request doesn't pay the fetch latency and a snapshot is
// populated before it's needed — narrowing the cold-cache window in which a
// "<family>-latest" default can't resolve. A fetch failure is logged and left
// for the next lazy refresh; there is deliberately no hardcoded fallback.
//
// Nil-receiver safe, like entryFor: Warm is reached through the
// store.PriceSource interface, where a typed-nil *ModelCatalog makes the
// interface value itself non-nil, so the caller's `src == nil` check cannot
// catch it. The sync runs in a bare goroutine with no recover, so a panic here
// would take the server down.
func (c *ModelCatalog) Warm() { c.refresh() }

// fresh and backingOff need no nil guard of their own: refresh is their only
// caller and returns before reaching them on a nil receiver.
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
		c.recordFailure()
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Warn("model catalog: fetch failed (keeping cached)", "error", err)
		c.recordFailure()
		return
	}
	defer resp.Body.Close()
	var payload struct {
		Data []orModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		c.logger.Warn("model catalog: decode failed (keeping cached)", "error", err)
		c.recordFailure()
		return
	}
	c.mu.Lock()
	c.models = payload.Data
	c.fetched = time.Now()
	c.lastFail = time.Time{} // success clears the backoff
	c.mu.Unlock()
	c.saveCache() // best-effort; persists the snapshot for the next process
}

func (c *ModelCatalog) recordFailure() {
	c.mu.Lock()
	c.lastFail = time.Now()
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

// ResolveNewest returns the newest catalog model in the line named by an OR
// id prefix (see modelAliases): the prefix itself or a stamped point release
// of it, excluding ~aliases. ok=false if unresolved.
func (c *ModelCatalog) ResolveNewest(prefix string) (string, bool) {
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	var best orModel
	for _, m := range c.models {
		if strings.HasPrefix(m.ID, "~") || !inModelLine(m.ID, prefix) {
			continue
		}
		if best.ID == "" || m.Created > best.Created {
			best = m
		}
	}
	return best.ID, best.ID != ""
}

// inModelLine reports whether id is prefix itself or a stamped point release
// of it ("<prefix>-0905"). A letter after the dash is a variant fork
// (-thinking, -code, -fast) and a dot starts a new line (kimi-k3.5), not a
// release of this one — both vendors stamp point releases as "-<digits>".
func inModelLine(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	return len(rest) >= 2 && rest[0] == '-' && rest[1] >= '0' && rest[1] <= '9'
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

// normalizeTilde re-adds the leading "~" to a slash id when the catalog contains
// the tilde form but not the bare one — rescuing an OpenRouter auto-latest alias
// (~openai/gpt-latest) requested without its tilde. Conservative: returns
// (id, false) unless the tilde form is present and the bare one absent, so a
// stale or partial cache never rewrites a genuinely valid id.
func (c *ModelCatalog) normalizeTilde(id string) (string, bool) {
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	tilde := "~" + id
	var hasBare, hasTilde bool
	for _, m := range c.models {
		switch m.ID {
		case id:
			hasBare = true
		case tilde:
			hasTilde = true
		}
	}
	if hasTilde && !hasBare {
		return tilde, true
	}
	return id, false
}

// AllIDs returns every catalog model id (leading "~" auto-alias marker
// stripped, for shell-safe completion; the proxy re-adds it at routing time via
// normalizeTilde), sorted + de-duplicated. Empty if the catalog hasn't loaded.
// Nil-receiver safe for the same reason Warm is: it is reached through
// store.PriceSource, where a typed nil arrives as a non-nil interface.
func (c *ModelCatalog) AllIDs() []string {
	if c == nil {
		return nil
	}
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

// ContextWindow returns the OpenRouter-reported context length and max
// completion tokens for a requested model, resolving it the same way the proxy
// does (empty->default is not applied here; "<family>-latest" and bare/slash
// ids resolve to the concrete OpenRouter entry). ok is false on a nil receiver,
// an unresolvable model, a cold/stale cache without the entry, or an entry that
// doesn't report a context length.
func (c *ModelCatalog) ContextWindow(model string) (contextLen, maxCompletion int, ok bool) {
	m, found := c.entryFor(model)
	if !found {
		return 0, 0, false
	}
	return m.ContextLen, m.TopProvider.MaxCompletionTokens, m.ContextLen > 0
}

// Pricing returns the OpenRouter list price for a requested model, resolved the
// same way ContextWindow does. ok is false on a nil receiver, an unresolvable
// model, a cold/stale cache without the entry, or an entry with no parseable
// base price.
func (c *ModelCatalog) Pricing(model string) (ModelPricing, bool) {
	m, found := c.entryFor(model)
	if !found {
		return ModelPricing{}, false
	}
	return pricingFromOR(m.Pricing)
}

// ResolveID returns the OpenRouter id a requested model resolves to, using the
// same rules as Pricing and ContextWindow. Callers that need to record which
// catalog entry a model string mapped to should use this rather than
// OpenRouterModel, which is Anthropic-specific and best-effort: it would turn an
// unknown "gpt-5.6" into "anthropic/gpt-5.6". ok is false when the model does
// not resolve to an entry in the current snapshot.
func (c *ModelCatalog) ResolveID(model string) (string, bool) {
	m, found := c.entryFor(model)
	if !found {
		return "", false
	}
	return m.ID, true
}

// Price reports the catalog's list price in the shape store.SyncModelPricing
// consumes. Unlike Pricing it reads the raw entry, so it can distinguish a
// cache price OpenRouter omitted from one it reports as zero — see
// optionalPrice. ok is false under the same conditions as Pricing.
func (c *ModelCatalog) Price(model string) (store.ModelPrice, bool) {
	m, found := c.entryFor(model)
	if !found {
		return store.ModelPrice{}, false
	}
	return priceFromEntry(m)
}

// Lookup resolves a model once and reports everything store.SyncModelPricing
// records about it, letting *ModelCatalog satisfy store.PriceSource directly.
// It is one entryFor call by design: the syncer runs over every catalog id plus
// every observed model string, and each entryFor locks and scans a snapshot
// shared with the live proxy's request path, so separate id/price accessors
// multiplied that cost per key.
func (c *ModelCatalog) Lookup(model string) (store.ModelInfo, bool) {
	m, found := c.entryFor(model)
	if !found {
		return store.ModelInfo{}, false
	}
	price, priced := priceFromEntry(m)
	return store.ModelInfo{ORID: m.ID, Price: price, Priced: priced}, true
}

// priceFromEntry projects a catalog entry's raw price strings into the store
// shape. ok=false unless both base prices parse (a model with no usable base
// price is unpriced), matching pricingFromOR; the cache prices differ in
// keeping absent distinct from zero.
func priceFromEntry(m orModel) (store.ModelPrice, bool) {
	prompt, err := strconv.ParseFloat(m.Pricing.Prompt, 64)
	if err != nil {
		return store.ModelPrice{}, false
	}
	completion, err := strconv.ParseFloat(m.Pricing.Completion, 64)
	if err != nil {
		return store.ModelPrice{}, false
	}
	return store.ModelPrice{
		PromptUSD:     prompt,
		CompletionUSD: completion,
		CacheReadUSD:  optionalPrice(m.Pricing.InputCacheRead),
		CacheWriteUSD: optionalPrice(m.Pricing.InputCacheWrite),
	}, true
}

// optionalPrice parses a price OpenRouter omits for models without prompt
// caching, returning nil when the field is empty or unparseable. The pointer
// exists so "the source does not price this" cannot be recorded as a zero
// price: a zero cache-read price makes the dashboard's cache savings the cache
// tokens at full prompt price, overstating it by the whole discount.
func optionalPrice(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// var _ store.PriceSource ensures *ModelCatalog keeps satisfying the interface
// store.SyncModelPricing depends on; a signature drift here fails the build
// instead of surfacing as a runtime type error in Task 3's caller.
var _ store.PriceSource = (*ModelCatalog)(nil)

// entryFor resolves a requested model to its catalog orModel, shared by
// ContextWindow and Pricing so both apply identical resolution and staleness
// semantics. It mirrors the proxy's rules ("<family>-latest", bare Anthropic,
// slash ids) via lookup, and falls back to the base model for a dated Anthropic
// snapshot id (claude-haiku-4-5-20251001), which OpenRouter doesn't list. ok is
// false on a nil receiver or no match.
func (c *ModelCatalog) entryFor(model string) (orModel, bool) {
	if c == nil {
		return orModel{}, false
	}
	if m, ok := c.lookup(model); ok {
		return m, true
	}
	if base, ok := stripSnapshotDate(model); ok {
		return c.lookup(base)
	}
	return orModel{}, false
}

// lookup resolves model to a concrete OpenRouter id and returns the matching
// catalog entry, refreshing the snapshot first. ok=false when the model can't
// be resolved or isn't present in the current snapshot.
func (c *ModelCatalog) lookup(model string) (orModel, bool) {
	resolved, err := ResolveModel(c, "", model)
	if err != nil || resolved == "" {
		return orModel{}, false
	}
	orID := c.OpenRouterModel(resolved)
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.models {
		if m.ID == orID {
			return m, true
		}
	}
	return orModel{}, false
}

// stripSnapshotDate cuts a trailing Anthropic snapshot date ("-20251001") off a
// model id. ok=false when the id carries no date suffix.
func stripSnapshotDate(id string) (string, bool) {
	i := strings.LastIndex(id, "-")
	if i <= 0 || len(id)-i-1 != 8 {
		return "", false
	}
	suffix := id[i+1:]
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	if !strings.HasPrefix(suffix, "20") {
		return "", false
	}
	return id[:i], true
}

// AutoCompactWindow computes a CLAUDE_CODE_AUTO_COMPACT_WINDOW token threshold
// from a model's context length: the full window minus a reply reserve. The
// reserve is the model's max completion tokens, clamped to [5%, 10%] of the
// window — a floor so a model that reports no (or a tiny) max output still keeps
// headroom for the compaction summary and a reply, and a cap so a huge-output
// model doesn't compact absurdly early. Returns 0 when contextLen is
// non-positive, so callers skip the variable and Claude Code keeps its default.
func AutoCompactWindow(contextLen, maxCompletion int) int {
	if contextLen <= 0 {
		return 0
	}
	minReserve := contextLen / 20 // 5% floor
	maxReserve := contextLen / 10 // 10% cap
	reserve := maxCompletion
	if reserve < minReserve {
		reserve = minReserve
	}
	if reserve > maxReserve {
		reserve = maxReserve
	}
	return contextLen - reserve
}

// CatalogEntry is the exported shape for test seeding. Pricing is nil for an
// unpriced model (Pricing() then reports ok=false); when set, its floats are
// formatted into the wire price strings so they round-trip back through
// pricingFromOR.
type CatalogEntry struct {
	ID                  string
	Created             int64
	ContextLength       int
	MaxCompletionTokens int
	Pricing             *ModelPricing
}

// SeedForTest injects catalog entries without a network fetch (tests only).
func (c *ModelCatalog) SeedForTest(entries []CatalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = make([]orModel, len(entries))
	for i, e := range entries {
		c.models[i] = orModel{ID: e.ID, Created: e.Created, ContextLen: e.ContextLength}
		c.models[i].TopProvider.MaxCompletionTokens = e.MaxCompletionTokens
		if e.Pricing != nil {
			c.models[i].Pricing = orPricing{
				Prompt:            formatPrice(e.Pricing.PromptUSD),
				Completion:        formatPrice(e.Pricing.CompletionUSD),
				InputCacheRead:    optionalPriceString(e.Pricing.CacheReadUSD),
				InputCacheWrite:   optionalPriceString(e.Pricing.CacheWriteUSD),
				InputCacheWrite1h: optionalPriceString(e.Pricing.CacheWrite1hUSD),
			}
		}
	}
	c.fetched = time.Now().Add(c.ttl) // never refresh in tests
}

func formatPrice(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// optionalPriceString seeds a cache price the way OpenRouter sends it: omitted
// (empty) for a model that doesn't price it. A zero in a CatalogEntry therefore
// seeds an ABSENT cache price, which is what lets a test exercise the
// absent-vs-zero distinction Price preserves. A genuine zero cache rate is not
// a thing OpenRouter publishes, so nothing needs to seed one.
func optionalPriceString(v float64) string {
	if v == 0 {
		return ""
	}
	return formatPrice(v)
}
