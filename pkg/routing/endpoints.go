// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	openRouterEndpointsURLFmt = "https://openrouter.ai/api/v1/models/%s/endpoints"
	endpointCatalogTTL        = 30 * time.Minute
)

// Endpoint is one hosting endpoint of a model, from OpenRouter's
// /models/{id}/endpoints. Pricing is USD per token (as OpenRouter reports it).
type Endpoint struct {
	Provider        string   // provider_name, display name, e.g. "DeepInfra"
	Slug            string   // provider slug: Tag up to the first "/"
	Tag             string   // e.g. "deepinfra/fp8"
	Quantization    string   // "fp8", "unknown", …
	PromptPrice     float64  // USD/token; -1 when unparseable
	CompletionPrice float64  // USD/token; -1 when unparseable
	ContextLength   int      //
	MaxCompletion   int      // 0 when null
	Uptime30m       *float64 // nil when null
	Tools           bool     // "tools" in supported_parameters
	Status          int      //
}

// endpointEntry is one model's cached endpoint list and per-model fetch state.
// Backoff is per entry so one model's outage never suppresses another's fetch.
type endpointEntry struct {
	eps      []Endpoint
	fetched  time.Time
	lastFail time.Time
}

// EndpointCatalog lists a model's hosting endpoints, fetched from OpenRouter's
// /api/v1/models/{id}/endpoints. It is populated on demand: Endpoints loads
// synchronously when the model is missing or its entry is past
// endpointCatalogTTL, and serves the last good entry when a refetch fails. A
// failed fetch backs off PER MODEL for fetchBackoff; the backoff also covers the
// cold path, so a model whose API is down is not refetched on every call. A nil
// *EndpointCatalog is invalid.
type EndpointCatalog struct {
	http   *http.Client
	urlFmt string
	logger *slog.Logger
	sf     singleflight.Group

	now func() time.Time // injectable clock for tests

	mu      sync.Mutex
	entries map[string]*endpointEntry
}

func NewEndpointCatalog(httpClient *http.Client, logger *slog.Logger) *EndpointCatalog {
	return &EndpointCatalog{http: httpClient, urlFmt: openRouterEndpointsURLFmt, logger: logger, now: time.Now, entries: map[string]*endpointEntry{}}
}

// NewEndpointCatalogForTest returns a catalog that fetches from urlFmt, a
// fmt format with one %s for the model id.
func NewEndpointCatalogForTest(httpClient *http.Client, urlFmt string, logger *slog.Logger) *EndpointCatalog {
	return &EndpointCatalog{http: httpClient, urlFmt: urlFmt, logger: logger, now: time.Now, entries: map[string]*endpointEntry{}}
}

// Endpoints returns the model's endpoints, loading synchronously when the
// model is not cached or its entry is past endpointCatalogTTL. On a fetch
// failure it returns the stale entry if one exists (err nil, stale true), else
// the error. Within fetchBackoff of that model's last failure no request is
// made: a stale entry is served (stale true), or, cold, the error
// `endpoints <model>: unavailable (last fetch failed)` is returned. model is the
// OpenRouter id WITHOUT any "openrouter/" prefix and without a routing bracket,
// e.g. "deepseek/deepseek-v4.1-flash".
func (c *EndpointCatalog) Endpoints(ctx context.Context, model string) (eps []Endpoint, stale bool, err error) {
	if eps, fresh, ok := c.cached(model); ok {
		return eps, !fresh, nil
	}
	v, err, _ := c.sf.Do(model, func() (any, error) {
		if eps, fresh, ok := c.cached(model); ok { // a caller queued behind a just-finished load needn't refetch
			return endpointResult{eps: eps, stale: !fresh}, nil
		}
		if c.backingOff(model) {
			return nil, fmt.Errorf("endpoints %s: unavailable (last fetch failed)", model)
		}
		fresh, ferr := c.fetch(ctx, model)
		c.mu.Lock()
		defer c.mu.Unlock()
		e := c.entries[model]
		if e == nil {
			e = &endpointEntry{}
			c.entries[model] = e
		}
		if ferr != nil {
			e.lastFail = c.now()
			if e.eps != nil {
				c.logf("endpoint catalog: fetch failed (serving stale)", "model", model, "error", ferr)
				return endpointResult{eps: e.eps, stale: true}, nil
			}
			return nil, ferr
		}
		e.eps, e.fetched, e.lastFail = fresh, c.now(), time.Time{}
		return endpointResult{eps: fresh}, nil
	})
	if err != nil {
		return nil, false, err
	}
	r := v.(endpointResult)
	return r.eps, r.stale, nil
}

// endpointResult carries a singleflight load outcome, including whether it is
// the stale entry kept after a failed refetch.
type endpointResult struct {
	eps   []Endpoint
	stale bool
}

// cached reports whether model can answer without a fetch: fresh within the TTL,
// or the last good list while within fetchBackoff of a failed refetch.
func (c *EndpointCatalog) cached(model string) (eps []Endpoint, fresh bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[model]
	if e == nil {
		return nil, false, false
	}
	if !e.fetched.IsZero() && c.now().Sub(e.fetched) < endpointCatalogTTL {
		return e.eps, true, true
	}
	if e.eps != nil && !e.lastFail.IsZero() && c.now().Sub(e.lastFail) < fetchBackoff {
		return e.eps, false, true
	}
	return nil, false, false
}

// backingOff reports whether model's last fetch failed within fetchBackoff.
func (c *EndpointCatalog) backingOff(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[model]
	return e != nil && !e.lastFail.IsZero() && c.now().Sub(e.lastFail) < fetchBackoff
}

func (c *EndpointCatalog) logf(msg string, args ...any) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(msg, args...)
}

// fetch loads a model's endpoints. It always returns a fresh, non-empty list
// or an error; the caller decides whether to keep a stale entry.
func (c *EndpointCatalog) fetch(ctx context.Context, model string) ([]Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(c.urlFmt, model), nil)
	if err != nil {
		return nil, fmt.Errorf("endpoints %s: %w", model, err)
	}
	req.Header.Set("User-Agent", "rafiki")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("endpoints %s: %w", model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoints %s: status %s", model, resp.Status)
	}
	var payload struct {
		Data struct {
			Endpoints []struct {
				Name         string `json:"name"`
				ModelID      string `json:"model_id"`
				ProviderName string `json:"provider_name"`
				Tag          string `json:"tag"`
				Quantization string `json:"quantization"`
				Pricing      struct {
					Prompt     string `json:"prompt"`
					Completion string `json:"completion"`
				} `json:"pricing"`
				ContextLength       int      `json:"context_length"`
				MaxCompletionTokens *int     `json:"max_completion_tokens"`
				SupportedParameters []string `json:"supported_parameters"`
				Status              int      `json:"status"`
				UptimeLast30m       *float64 `json:"uptime_last_30m"`
			} `json:"endpoints"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("endpoints %s: %w", model, err)
	}
	if len(payload.Data.Endpoints) == 0 {
		return nil, fmt.Errorf("endpoints %s: no endpoints listed", model)
	}
	eps := make([]Endpoint, 0, len(payload.Data.Endpoints))
	for _, e := range payload.Data.Endpoints {
		slug, _, _ := strings.Cut(e.Tag, "/")
		if slug == "" {
			slug = guessSlug(e.ProviderName)
		}
		ep := Endpoint{
			Provider:        e.ProviderName,
			Slug:            slug,
			Tag:             e.Tag,
			Quantization:    e.Quantization,
			PromptPrice:     parsePrice(e.Pricing.Prompt),
			CompletionPrice: parsePrice(e.Pricing.Completion),
			ContextLength:   e.ContextLength,
			Tools:           slices.Contains(e.SupportedParameters, "tools"),
			Status:          e.Status,
			Uptime30m:       e.UptimeLast30m,
		}
		if e.MaxCompletionTokens != nil {
			ep.MaxCompletion = *e.MaxCompletionTokens
		}
		eps = append(eps, ep)
	}
	return eps, nil
}

// parsePrice parses an OpenRouter price string, returning -1 when it cannot
// understand it. A free endpoint's "0" stays 0 so a free price and an unknown
// price remain distinguishable.
func parsePrice(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return f
}
