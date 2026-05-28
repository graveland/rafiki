// Package models enumerates available LLM models from all configured sources:
// the user's models.json, a hardcoded builtin list, and live local inference
// servers (Ollama, LM Studio).
//
// All sources are best-effort: errors are swallowed and missing or unreachable
// sources simply produce no entries.  The package is used both by the daemon
// (ctrl_list_models RPC) and by the CLI (tab completion for --model).
package models

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source identifies which discovery mechanism produced a Model entry.
type Source string

const (
	SourceUserConfig Source = "user-config" // ~/.pi/agent/models.json
	SourceBuiltin    Source = "builtin"     // hardcoded common list
	SourceOllama     Source = "ollama"      // live Ollama server
	SourceLMStudio   Source = "lmstudio"   // live LM Studio server
)

// Model is a single enumerated model entry.
type Model struct {
	ID       string `json:"id"`             // "provider/model"
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Name     string `json:"name,omitempty"` // display name from models.json
	Source   Source `json:"source"`
}

// knownModels is a curated list of commonly-used "provider/model" identifiers.
// Keep additions alphabetically grouped by provider for readability.
var knownModels = []string{
	// Anthropic
	"anthropic/claude-opus-4-7",
	"anthropic/claude-sonnet-4-5",
	"anthropic/claude-haiku-4-5",
	"anthropic/claude-3-5-sonnet-latest",
	"anthropic/claude-3-5-haiku-latest",

	// OpenAI
	"openai/gpt-4o",
	"openai/gpt-4o-mini",
	"openai/gpt-4.1",
	"openai/gpt-4.1-mini",
	"openai/o1",
	"openai/o1-mini",
	"openai/o3",
	"openai/o3-mini",

	// Google
	"google/gemini-2.5-pro",
	"google/gemini-2.5-flash",
	"google/gemini-2.0-flash-exp",

	// xAI
	"xai/grok-4",
	"xai/grok-4-mini",
}

// List returns the union of all sources, deduped on ID.  First occurrence wins
// (user-config > builtin > ollama > lmstudio), so user-configured models
// shadow builtin entries with the same ID and carry the user's display name.
//
// ctx is used as the parent for per-source HTTP timeouts (200 ms each), so
// callers can bound overall wall time by passing a context with a deadline.
func List(ctx context.Context) []Model {
	var all []Model
	all = append(all, loadUserConfig()...)
	all = append(all, loadBuiltins()...)
	all = append(all, loadOllama(ctx)...)
	all = append(all, loadLMStudio(ctx)...)

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

// loadBuiltins returns the hardcoded knownModels list as Model entries.
func loadBuiltins() []Model {
	out := make([]Model, 0, len(knownModels))
	for _, id := range knownModels {
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

// loadOllama lists models from a local Ollama instance via its /api/tags
// endpoint. Honours OLLAMA_HOST; defaults to http://localhost:11434.
func loadOllama(ctx context.Context) []Model {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	} else if !strings.HasPrefix(host, "http") {
		host = "http://" + host
	}

	reqCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, host+"/api/tags", nil)
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

	out := make([]Model, 0, len(payload.Models))
	for _, m := range payload.Models {
		if m.Name != "" {
			out = append(out, Model{
				ID:       "ollama/" + m.Name,
				Provider: "ollama",
				Model:    m.Name,
				Source:   SourceOllama,
			})
		}
	}
	return out
}

// loadLMStudio lists models from a local LM Studio instance via its
// OpenAI-compatible /v1/models endpoint. Honours LM_STUDIO_HOST; defaults
// to http://localhost:1234.
func loadLMStudio(ctx context.Context) []Model {
	host := os.Getenv("LM_STUDIO_HOST")
	if host == "" {
		host = "http://localhost:1234"
	} else if !strings.HasPrefix(host, "http") {
		host = "http://" + host
	}

	reqCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, host+"/v1/models", nil)
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

	out := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, Model{
				ID:       "lmstudio/" + m.ID,
				Provider: "lmstudio",
				Model:    m.ID,
				Source:   SourceLMStudio,
			})
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
