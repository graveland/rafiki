// Package models enumerates available LLM models from all configured sources:
// the user's models.json, a curated builtin list, the live OpenRouter catalog,
// and live probes of any configured providers with explicit base_urls.
//
// All sources are best-effort: errors are swallowed and missing or unreachable
// sources simply produce no entries.  The package is used both by the daemon
// (ctrl_list_models RPC) and by the CLI (tab completion for --model).
package models

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// Source identifies which discovery mechanism produced a Model entry.
type Source string

const (
	SourceUserConfig Source = "user-config" // ~/.pi/agent/models.json
	SourceBuiltin    Source = "builtin"     // curated list + <family>-latest aliases
	SourceOpenRouter Source = "openrouter"  // live OpenRouter catalog (non-anthropic ids)
	SourceLocal      Source = "local"       // live probe of a configured local provider
)

// openRouterCatalogTTL bounds how long the on-disk /models snapshot is trusted
// before a refetch. The id set changes rarely; a day is plenty.
const openRouterCatalogTTL = 24 * time.Hour

// openRouterTimeout caps the catalog fetch. Completion runs on a TAB press, so
// a slow or unreachable OpenRouter must degrade to the cached snapshot (or to
// no OpenRouter entries at all) rather than making the shell appear to hang.
const openRouterTimeout = 2 * time.Second

// Model is a single enumerated model entry.
type Model struct {
	ID       string `json:"id"` // "provider/model"
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Name     string `json:"name,omitempty"` // display name from models.json
	Source   Source `json:"source"`
}

// knownModels is a curated list of commonly-used "provider/model" identifiers.
// Keep additions alphabetically grouped by provider for readability.
//
// Only the Anthropic entries here are load-bearing: every other provider's ids
// also arrive from the live OpenRouter catalog, so these are a warm-start for a
// cold cache rather than the only way to reach them. Anthropic ids cannot come
// from that catalog (see the package doc), so this list plus the "<family>-latest"
// aliases is the whole of what completion offers for the native path.
var knownModels = []string{
	// Anthropic. Concrete ids date quickly — prefer the "<family>-latest"
	// aliases loadBuiltins appends, which resolve live against the catalog.
	"anthropic/claude-opus-5",
	"anthropic/claude-sonnet-5",
	"anthropic/claude-fable-5",
	"anthropic/claude-opus-4-7",
	"anthropic/claude-sonnet-4-5",
	"anthropic/claude-haiku-4-5",
	"anthropic/claude-3-5-sonnet-latest",
	"anthropic/claude-3-5-haiku-latest",

	// OpenAI
	"openrouter/openai/gpt-4o",
	"openrouter/openai/gpt-4o-mini",
	"openrouter/openai/gpt-4.1",
	"openrouter/openai/gpt-4.1-mini",
	"openrouter/openai/o1",
	"openrouter/openai/o1-mini",
	"openrouter/openai/o3",
	"openrouter/openai/o3-mini",

	// Google
	"openrouter/google/gemini-2.5-pro",
	"openrouter/google/gemini-2.5-flash",
	"openrouter/google/gemini-2.0-flash-exp",

	// xAI
	"openrouter/xai/grok-4",
	"openrouter/xai/grok-4-mini",
}

// List returns the union of all sources, deduped on ID.  First occurrence wins
// (user-config > builtin > openrouter > ollama > lmstudio), so user-configured
// models shadow builtin entries with the same ID and carry the user's display
// name, and the curated builtin entries keep their ordering ahead of the ~300
// the live catalog contributes.
//
// ctx is used as the parent for per-source HTTP timeouts (200 ms each), so
// callers can bound overall wall time by passing a context with a deadline.
func List(ctx context.Context, set *providers.Set) []Model {
	return ListSources(ctx, set, nil)
}

// ListSources is List restricted to the given sources. A nil or empty set
// means all of them.
//
// This is not merely a filter over List: a source the caller does not want is
// never consulted at all. That matters because the sources have very different
// costs — OpenRouter may fetch over the network, and local providers are
// probed with a 200ms timeout each — and the caller usually knows it cannot use
// most of them. Tab completion for a pi child has no use for OpenRouter ids and
// should not pay a network round trip to discard them.
func ListSources(ctx context.Context, set *providers.Set, want map[Source]bool) []Model {
	enabled := func(s Source) bool { return len(want) == 0 || want[s] }

	var all []Model
	if enabled(SourceUserConfig) {
		all = append(all, loadUserConfig()...)
	}
	if enabled(SourceBuiltin) {
		all = append(all, loadBuiltins()...)
	}
	if enabled(SourceOpenRouter) {
		all = append(all, loadOpenRouter(ctx)...)
	}
	if enabled(SourceLocal) {
		all = append(all, loadLocal(ctx, set)...)
	}

	// Deduplicate by ID, preserving first-seen order.
	seen := make(map[string]struct{}, len(all))
	out := all[:0]
	for _, m := range all {
		if _, ok := seen[m.ID]; ok {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, m)
	}
	return out
}

// loadUserConfig reads ~/.pi/agent/models.json. Returns nil on any error
// (missing file, parse failure) since we treat this as best-effort.
func loadUserConfig() []Model {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".pi", "agent", "models.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var payload struct {
		Providers map[string]struct {
			Inherit string `json:"inherit,omitempty"`
			Models  []struct {
				ID   string `json:"id"`
				Name string `json:"name,omitempty"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil
	}

	var out []Model
	for providerName, p := range payload.Providers {
		if p.Inherit != "" {
			prefix := p.Inherit + "/"
			for _, id := range knownModels {
				if !strings.HasPrefix(id, prefix) {
					continue
				}
				model := id[len(prefix):]
				out = append(out, Model{
					ID:       providerName + "/" + model,
					Provider: providerName,
					Model:    model,
					Source:   SourceUserConfig,
				})
			}
		}
		for _, m := range p.Models {
			if m.ID == "" {
				continue
			}
			out = append(out, Model{
				ID:       providerName + "/" + m.ID,
				Provider: providerName,
				Model:    m.ID,
				Name:     m.Name,
				Source:   SourceUserConfig,
			})
		}
	}
	return out
}

// loadBuiltins returns the curated knownModels list plus the Anthropic
// "<family>-latest" aliases, as Model entries.
//
// The aliases are the answer to this list going stale: routing.LatestFamilies
// returns family *names*, and the newest version per family is resolved live at
// request time, so "anthropic/opus-latest" keeps working across a model release
// with nobody editing anything. Concrete ids stay listed because people do pin
// deliberately, and because a pinned id is what makes a captured conversation
// reproducible later.
func loadBuiltins() []Model {
	ids := make([]string, 0, len(knownModels)+4)
	ids = append(ids, knownModels...)
	for _, fam := range routing.LatestFamilies() {
		ids = append(ids, "anthropic/"+fam)
	}
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		provider, model := splitID(id)
		out = append(out, Model{
			ID:       id,
			Provider: provider,
			Model:    model,
			Source:   SourceBuiltin,
		})
	}
	return out
}

// loadOpenRouter returns the live OpenRouter catalog's ids, excluding the
// "anthropic/" ones (see the package doc: those route to the native Anthropic
// sender, whose id spelling differs).
//
// The catalog is disk-cached, so the common case costs no network at all: a
// warm snapshot inside its TTL is read straight off disk, and the cache is
// shared with anything else on the machine pointed at the same path. On a cold
// or expired cache this fetches, bounded by openRouterTimeout; on failure
// ModelCatalog keeps serving whatever snapshot it has, and a cold failure just
// yields no entries. Errors are logged nowhere on purpose — this is completion,
// and the discard logger keeps a slow network from printing over the prompt.
func loadOpenRouter(ctx context.Context) []Model {
	store := routing.FileSnapshotStore{Path: filepath.Join(paths.CacheDir(), "openrouter_catalog.json")}
	cat := routing.NewModelCatalog(
		&http.Client{Timeout: openRouterTimeout},
		openRouterCatalogTTL,
		slog.New(slog.DiscardHandler),
	).WithCache(store)

	// AllIDs refreshes synchronously; bound it on the caller's context too so a
	// deadline passed to List actually caps this source.
	done := make(chan []string, 1)
	go func() { done <- cat.AllIDs() }()
	select {
	case ids := <-done:
		return openRouterModels(ids)
	case <-ctx.Done():
		return nil
	}
}

// openRouterModels converts catalog ids to Model entries, dropping the ones
// fundi cannot route from this source. Split out from loadOpenRouter so the
// filtering rule is testable without a network or a cache on disk.
func openRouterModels(ids []string) []Model {
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		// Anthropic ids from the OpenRouter catalog spell versions with dots
		// ("claude-opus-4.7") where the native API uses dashes, and they now
		// need the openrouter/ prefix like every other non-Anthropic id.
		if strings.HasPrefix(id, "anthropic/") {
			continue
		}
		provider, model := splitID(id)
		if provider == "" || model == "" {
			continue // fundi requires a provider-qualified id
		}
		out = append(out, Model{
			ID:       "openrouter/" + id,
			Provider: "openrouter",
			Model:    id,
			Source:   SourceOpenRouter,
		})
	}
	return out
}

// loadLocal probes every configured provider that has an explicit base_url,
// asking it what models it serves. Ids are emitted under the PROVIDER's name,
// so what completion offers is exactly what --model accepts.
//
// Two list endpoints are tried because local servers disagree about which they
// expose; both are listing endpoints and neither says anything about which
// protocol the messages path speaks. A provider that answers neither
// contributes nothing, which is the same best-effort contract every other
// source here has.
func loadLocal(ctx context.Context, set *providers.Set) []Model {
	if set == nil {
		return nil
	}
	var out []Model
	for _, name := range set.Names() {
		p := set.Providers[name]
		if p.BaseURL == "" {
			continue // a hosted provider; its catalog is not a local probe
		}
		for _, id := range probeLocal(ctx, p.BaseURL) {
			out = append(out, Model{
				ID:       name + "/" + id,
				Provider: name,
				Model:    id,
				Source:   SourceLocal,
			})
		}
	}
	return out
}

// probeLocal tries Ollama's /api/tags then the OpenAI-shaped /v1/models,
// returning the first non-empty answer. Each probe is bounded at 200ms:
// completion runs on a TAB press and an unreachable host must not hang the
// shell.
func probeLocal(ctx context.Context, base string) []string {
	if ids := probeOllamaTags(ctx, base); len(ids) > 0 {
		return ids
	}
	return probeOpenAIModels(ctx, base)
}

// probeOllamaTags lists models from an Ollama-style /api/tags endpoint.
func probeOllamaTags(ctx context.Context, base string) []string {
	reqCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out
}

// probeOpenAIModels lists models from an OpenAI-shaped /v1/models endpoint.
func probeOpenAIModels(ctx context.Context, base string) []string {
	reqCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out
}

// splitID splits "provider/model" into its two parts.
// When no slash is present, provider is empty and model is the full input.
func splitID(id string) (provider, model string) {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[:i], id[i+1:]
	}
	return "", id
}
