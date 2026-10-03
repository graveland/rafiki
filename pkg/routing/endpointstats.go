// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	openRouterModelPageURLFmt = "https://openrouter.ai/%s"
	pageStatsTTL              = 30 * time.Minute
)

// pageStatsTimeout bounds one model-page fetch. The page is a large Next.js
// RSC stream (roughly 2 MB), so a scrape is slower than a JSON API call.
const pageStatsTimeout = 15 * time.Second

// pageStatsMaxBytes caps how much of a model page is read. A well-formed page
// is ~2 MB; a response over the cap is treated as an error rather than parsed.
const pageStatsMaxBytes = 8 << 20

// Markers in the un-escaped RSC stream. The page embeds each endpoint as an
// object starting with {"id":"<endpoint_id>" and carrying its measured
// performance in a trailing "stats":{...,"endpoint_id":"<endpoint_id>",...}.
// endpointObjectMarker locates the object head (where provider_slug and
// quantization live); statsObjectMarker locates the stats object itself.
const (
	endpointObjectMarker = `{"id":"`
	statsObjectMarker    = `"stats":{"endpoint_id":"`
	statsPrefix          = `"stats":`
)

// EndpointStats is OpenRouter's measured performance for one endpoint over its
// reporting window. Throughput is output tokens/second.
type EndpointStats struct {
	ProviderSlug  string
	Quantization  string
	P50Throughput float64
	P90Throughput float64
	P50LatencyMs  float64
	Requests      int
	WindowMinutes int
}

// StatsSource yields reported performance stats for a model. A failure means
// "no stats", never "no routing": callers render a dash and carry on.
type StatsSource interface {
	Stats(ctx context.Context, model string) ([]EndpointStats, error)
}

// PageStats scrapes OpenRouter's per-model page for endpoint performance. It is
// fetched only on demand — a caller that asks for stats pays the fetch latency,
// no request path ever does — and cached per model for pageStatsTTL. A failed
// fetch keeps serving the last successful scrape (stale-on-failure); with no
// cache it returns the error, and a fetch failure backs off for fetchBackoff so
// a dead page is not hammered. Concurrent scrapes of one model are coalesced.
//
// The page is an untrusted upstream payload: parsing never panics, a malformed
// endpoint is skipped, and a 200 body with zero endpoints is an error the caller
// renders as a dash.
type PageStats struct {
	http   *http.Client
	urlFmt string
	logger *slog.Logger
	sf     singleflight.Group

	mu      sync.Mutex
	entries map[string]*pageStatsEntry
}

type pageStatsEntry struct {
	stats    []EndpointStats
	fetched  time.Time
	lastFail time.Time
}

// NewPageStats returns a scrape-backed StatsSource against OpenRouter's model
// pages.
func NewPageStats(httpClient *http.Client, logger *slog.Logger) *PageStats {
	return &PageStats{http: httpClient, urlFmt: openRouterModelPageURLFmt, logger: logger, entries: map[string]*pageStatsEntry{}}
}

// NewPageStatsForTest returns a PageStats whose model path is appended to urlFmt
// instead of OpenRouter's, so tests can point it at a fixture server.
func NewPageStatsForTest(httpClient *http.Client, urlFmt string, logger *slog.Logger) *PageStats {
	return &PageStats{http: httpClient, urlFmt: urlFmt, logger: logger, entries: map[string]*pageStatsEntry{}}
}

// Stats returns the reported performance for model, scraping and caching the
// model's page when the cache is empty, stale or previously failed. The model
// is OpenRouter's id ("deepseek/deepseek-v4.1-flash").
func (p *PageStats) Stats(ctx context.Context, model string) ([]EndpointStats, error) {
	if p == nil {
		return nil, errors.New("page stats: not configured")
	}
	if stats, ok := p.cached(model); ok {
		return stats, nil
	}
	v, err, _ := p.sf.Do(model, func() (any, error) {
		if stats, ok := p.cached(model); ok { // a caller queued behind a just-finished scrape needn't refetch
			return stats, nil
		}
		if p.backingOff(model) {
			return nil, fmt.Errorf("model page for %s: stats unavailable (last fetch failed)", model)
		}
		stats, ferr := p.fetch(ctx, model)
		p.mu.Lock()
		defer p.mu.Unlock()
		e := p.entries[model]
		if e == nil {
			e = &pageStatsEntry{}
			p.entries[model] = e
		}
		if ferr != nil {
			e.lastFail = time.Now()
			if e.stats != nil {
				p.logf("endpoint stats: scrape failed (serving stale)", "model", model, "error", ferr)
				return e.stats, nil
			}
			return nil, ferr
		}
		e.stats, e.fetched, e.lastFail = stats, time.Now(), time.Time{}
		return stats, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]EndpointStats), nil
}

// cached reports whether the entry for model can answer without a fetch: fresh
// within the TTL, or stale but within the failure backoff with a prior scrape.
func (p *PageStats) cached(model string) ([]EndpointStats, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[model]
	if e == nil {
		return nil, false
	}
	if !e.fetched.IsZero() && time.Since(e.fetched) < pageStatsTTL {
		return e.stats, true
	}
	if e.stats != nil && !e.lastFail.IsZero() && time.Since(e.lastFail) < fetchBackoff {
		return e.stats, true
	}
	return nil, false
}

// backingOff reports whether the last fetch for model failed within fetchBackoff.
func (p *PageStats) backingOff(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[model]
	return e != nil && !e.lastFail.IsZero() && time.Since(e.lastFail) < fetchBackoff
}

func (p *PageStats) logf(msg string, args ...any) {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(msg, args...)
}

// fetch scrapes and parses one model page. A response over pageStatsMaxBytes is
// an error, as is a body with no endpoint stats (the page-format-changed
// signal); a malformed individual endpoint is skipped, not fatal.
func (p *PageStats) fetch(ctx context.Context, model string) ([]EndpointStats, error) {
	ctx, cancel := context.WithTimeout(ctx, pageStatsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(p.urlFmt, model), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model page for %s: status %s", model, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, pageStatsMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("model page for %s: read failed: %w", model, err)
	}
	if len(body) > pageStatsMaxBytes {
		return nil, fmt.Errorf("model page for %s: response exceeds %d bytes", model, pageStatsMaxBytes)
	}
	stats, err := parseModelPage(ctx, string(body))
	if err != nil {
		return nil, fmt.Errorf("model page for %s: %w", model, err)
	}
	if len(stats) == 0 {
		p.logf("endpoint stats: model page yielded no endpoint stats (page format may have changed)", "model", model)
		return nil, fmt.Errorf("model page for %s: no endpoint stats found (page format may have changed)", model)
	}
	return stats, nil
}

// statsObjectScanCap bounds the bytes handed to the JSON decoder for one stats
// object. A real stats object is a few hundred bytes; the cap stops a malformed
// object with a valid-looking prefix from scanning past its own object.
const statsObjectScanCap = 4096

// parseModelPage extracts the endpoint stats from an OpenRouter model page. The
// page is a Next.js RSC stream whose JSON quotes are backslash-escaped; un-escape
// once, then scan, rather than running a regexp over megabytes. Each endpoint
// appears twice in the payload, so rows are deduped by endpoint id — provider
// slug is not unique (two endpoints can share one with different quantization).
//
// The scan is linear in the body: the backward search for an endpoint head is
// bounded to the region since the previous stats marker (a head never spans an
// earlier stats object), a failed id is recorded so a repeated one is never
// re-scanned, and the JSON decode is capped. ctx is checked each iteration so an
// adversarially long body still observes the caller's deadline.
func parseModelPage(ctx context.Context, body string) ([]EndpointStats, error) {
	text := strings.ReplaceAll(body, `\"`, `"`)
	var out []EndpointStats
	seen := make(map[string]bool)
	prevMarker := 0
	idx := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := strings.Index(text[idx:], statsObjectMarker)
		if rel < 0 {
			break
		}
		marker := idx + rel
		idStart := marker + len(statsObjectMarker)
		idEnd := strings.IndexByte(text[idStart:], '"')
		if idEnd < 0 {
			break
		}
		endpointID := text[idStart : idStart+idEnd]
		idx = idStart + idEnd // resume past this marker's id
		headSearch := prevMarker
		prevMarker = marker
		if endpointID == "" || seen[endpointID] {
			continue
		}
		// Record the attempt before any work: a repeated id is never scanned or
		// decoded twice, whether it matched or not.
		seen[endpointID] = true
		// The endpoint object opens with {"id":"<endpoint_id>"; provider_slug
		// and quantization live in its head, before the stats object. Bounding
		// the backward search to since the previous marker keeps it linear.
		objStart := strings.LastIndex(text[headSearch:marker], endpointObjectMarker)
		if objStart < 0 {
			continue
		}
		objStart += headSearch
		if !endpointIDMatches(text, objStart+len(endpointObjectMarker), endpointID) {
			continue
		}
		head := text[objStart:marker]
		scanEnd := marker + len(statsPrefix) + statsObjectScanCap
		if scanEnd > len(text) {
			scanEnd = len(text)
		}
		st, ok := decodeEndpointStats(text[marker+len(statsPrefix) : scanEnd])
		if !ok {
			continue
		}
		out = append(out, EndpointStats{
			ProviderSlug:  jsonStringField(head, "provider_slug"),
			Quantization:  jsonStringField(head, "quantization"),
			P50Throughput: statFloat(st.P50Throughput),
			P90Throughput: statFloat(st.P90Throughput),
			P50LatencyMs:  statFloat(st.P50Latency),
			Requests:      statInt(st.RequestCount),
			WindowMinutes: statInt(st.WindowMinutes),
		})
	}
	return out, nil
}

// endpointIDMatches reports whether text[pos:] begins with exactly id followed
// by the closing quote of the id string field — so id "ab" never matches a head
// whose id is "abc" and stats are never mis-attributed.
func endpointIDMatches(text string, pos int, id string) bool {
	if pos < 0 || pos+len(id) >= len(text) {
		return false
	}
	return text[pos:pos+len(id)] == id && text[pos+len(id)] == '"'
}

// rawEndpointStats is the subset of a stats object the scraper reads. Every
// percentile is a pointer so a null or absent value stays distinguishable from
// a reported zero, and json.Number keeps the wire value's precision.
type rawEndpointStats struct {
	EndpointID    string       `json:"endpoint_id"`
	P50Latency    *json.Number `json:"p50_latency"`
	P50Throughput *json.Number `json:"p50_throughput"`
	P90Throughput *json.Number `json:"p90_throughput"`
	RequestCount  *int         `json:"request_count"`
	WindowMinutes *int         `json:"window_minutes"`
}

// decodeEndpointStats decodes the single stats object that begins s (an
// un-escaped JSON object). It reports false for a malformed object so the caller
// skips that endpoint rather than failing the whole page.
func decodeEndpointStats(s string) (rawEndpointStats, bool) {
	var st rawEndpointStats
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&st); err != nil {
		return rawEndpointStats{}, false
	}
	if st.EndpointID == "" {
		return rawEndpointStats{}, false
	}
	return st, true
}

// statFloat converts a reported percentile, representing an absent or null
// value as -1 so callers can render a dash.
func statFloat(n *json.Number) float64 {
	if n == nil {
		return -1
	}
	v, err := n.Float64()
	if err != nil {
		return -1
	}
	return v
}

// statInt converts a reported count, representing an absent or null value as -1.
func statInt(n *int) int {
	if n == nil {
		return -1
	}
	return *n
}

// jsonStringField returns the value of the first `"key":"…"` in s, or "" when
// the key is absent. s is a single endpoint object's head, small enough that a
// plain scan is cheap; no regexp touches the whole page.
func jsonStringField(s, key string) string {
	prefix := `"` + key + `":"`
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
