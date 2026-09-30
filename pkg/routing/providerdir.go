// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"
)

const (
	openRouterProvidersURL = "https://openrouter.ai/api/v1/providers"
	providerDirTTL         = 24 * time.Hour
)

// ErrUnknownProvider is returned by ProviderDirectory.Resolve for a name the
// loaded directory does not list.
var ErrUnknownProvider = errors.New("unknown OpenRouter provider")

// ProviderDirectory maps OpenRouter provider display names to routing slugs.
// Responses report the display name ("OpenInference") and provider.only /
// provider.ignore take the slug ("open-inference"); nothing in a response
// links the two, and the lowercase-and-hyphenate guess is wrong for a real
// fraction of providers. OpenRouter's /api/v1/providers is the one source of
// the mapping.
//
// Loading is lazy: Slug triggers a background fetch when the directory is
// empty or stale and answers from what it has (the guess, before the first
// load), so no request path ever waits on OpenRouter. Resolve, the operator
// path, loads synchronously. A nil *ProviderDirectory is valid and always
// guesses.
type ProviderDirectory struct {
	http   *http.Client
	url    string
	logger *slog.Logger
	sf     singleflight.Group

	mu       sync.RWMutex
	slugs    map[string]string // compact(name) and compact(slug) -> slug
	names    map[string]string // slug -> display name
	fetched  time.Time
	lastFail time.Time
}

func NewProviderDirectory(httpClient *http.Client, logger *slog.Logger) *ProviderDirectory {
	return &ProviderDirectory{http: httpClient, url: openRouterProvidersURL, logger: logger}
}

// NewProviderDirectoryForTest returns a directory that fetches from url.
func NewProviderDirectoryForTest(httpClient *http.Client, url string, logger *slog.Logger) *ProviderDirectory {
	return &ProviderDirectory{http: httpClient, url: url, logger: logger}
}

// Slug returns provider's routing slug. provider may be a display name or
// already a slug, in any case. Unknown names, and every name before the
// first successful load, fall back to guessSlug.
func (d *ProviderDirectory) Slug(provider string) string {
	if d == nil {
		return guessSlug(provider)
	}
	if d.stale() {
		go d.refresh(context.Background())
	}
	if slug, ok := d.lookup(provider); ok {
		return slug
	}
	return guessSlug(provider)
}

// Resolve returns provider's slug for an operator action, loading the
// directory first if it is empty or stale. A name the loaded directory does
// not list is ErrUnknownProvider. When the directory cannot be loaded at all,
// Resolve degrades to guessSlug with degraded=true, so an OpenRouter outage
// never blocks a ban.
func (d *ProviderDirectory) Resolve(ctx context.Context, provider string) (slug string, degraded bool, err error) {
	if d == nil {
		return guessSlug(provider), true, nil
	}
	if d.stale() {
		d.refresh(ctx)
	}
	if !d.loaded() {
		return guessSlug(provider), true, nil
	}
	if slug, ok := d.lookup(provider); ok {
		return slug, false, nil
	}
	return "", false, fmt.Errorf("%w %q (slugs are listed at %s)", ErrUnknownProvider, provider, openRouterProvidersURL)
}

// Name returns slug's display name, or "" when the directory does not know it.
func (d *ProviderDirectory) Name(slug string) string {
	if d == nil {
		return ""
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.names[slug]
}

func (d *ProviderDirectory) lookup(provider string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	slug, ok := d.slugs[compactProvider(provider)]
	return slug, ok
}

func (d *ProviderDirectory) loaded() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return !d.fetched.IsZero()
}

// stale reports whether a fetch is due: never fetched or past the TTL, and
// not inside the failure backoff.
func (d *ProviderDirectory) stale() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.lastFail.IsZero() && time.Since(d.lastFail) < fetchBackoff {
		return false
	}
	return d.fetched.IsZero() || time.Since(d.fetched) >= providerDirTTL
}

func (d *ProviderDirectory) refresh(ctx context.Context) {
	d.sf.Do("refresh", func() (any, error) { //nolint:errcheck // fetch logs its own errors
		if !d.stale() {
			return nil, nil
		}
		d.fetch(ctx)
		return nil, nil
	})
}

func (d *ProviderDirectory) fetch(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fail := func(msg string, err error) {
		d.logger.Warn("provider directory: "+msg+" (keeping cached)", "error", err)
		d.mu.Lock()
		d.lastFail = time.Now()
		d.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		fail("build request failed", err)
		return
	}
	resp, err := d.http.Do(req)
	if err != nil {
		fail("fetch failed", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail("fetch failed", fmt.Errorf("status %s", resp.Status))
		return
	}
	var payload struct {
		Data []struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		fail("decode failed", err)
		return
	}
	if len(payload.Data) == 0 {
		fail("fetch failed", errors.New("empty provider list"))
		return
	}
	slugs := make(map[string]string, 2*len(payload.Data))
	names := make(map[string]string, len(payload.Data))
	// Slugs first, so a display name can never shadow another provider's slug.
	for _, p := range payload.Data {
		if p.Slug == "" {
			continue
		}
		slugs[compactProvider(p.Slug)] = p.Slug
		names[p.Slug] = p.Name
	}
	for _, p := range payload.Data {
		if k := compactProvider(p.Name); p.Slug != "" && k != "" {
			if _, taken := slugs[k]; !taken {
				slugs[k] = p.Slug
			}
		}
	}
	d.mu.Lock()
	d.slugs, d.names = slugs, names
	d.fetched, d.lastFail = time.Now(), time.Time{}
	d.mu.Unlock()
}

// compactProvider folds a provider name or slug to its letters and digits,
// lowercased, so "OpenInference", "Open Inference" and "open-inference" meet.
func compactProvider(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// guessSlug is the fallback when the directory cannot answer: "CoreWeave" ->
// "coreweave", "Amazon Bedrock" -> "amazon-bedrock". It is wrong for any
// provider whose slug is not its lowercased, hyphenated name.
func guessSlug(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}

// GuessProviderSlug is guessSlug for callers with no guard to ask.
func GuessProviderSlug(name string) string { return guessSlug(name) }
