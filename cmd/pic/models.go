package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// knownModels is a curated list of commonly-used "provider/model" identifiers
// for tab completion on `pic create --model`. Power users with exotic models
// can still pass anything; this is a starting point, not a whitelist.
//
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

// completeModel returns tab-completion candidates for the --model flag.
// It combines:
//   - A curated static list of common provider/model identifiers.
//   - Live enumeration of Ollama models (if Ollama is running locally).
//   - Live enumeration of LM Studio / OpenAI-compatible local endpoints
//     (best-effort; silently skipped if not reachable).
//
// Errors are swallowed so completion never blocks or errors out: missing
// providers just don't appear in the list.
func completeModel(toComplete string) []string {
	out := append([]string(nil), knownModels...)

	if models := ollamaModels(); len(models) > 0 {
		out = append(out, models...)
	}
	if models := lmStudioModels(); len(models) > 0 {
		out = append(out, models...)
	}

	// Deduplicate (a model could in principle be listed twice) and sort.
	seen := make(map[string]struct{}, len(out))
	dedup := out[:0]
	for _, m := range out {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		dedup = append(dedup, m)
	}
	sort.Strings(dedup)
	return dedup
}

// ollamaModels lists models from a local Ollama instance via its /api/tags
// endpoint. Honours OLLAMA_HOST (e.g. "http://localhost:11434"); defaults to
// localhost. 200ms timeout — fail fast so completion stays snappy.
func ollamaModels() []string {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	} else if !strings.HasPrefix(host, "http") {
		host = "http://" + host
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
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
			out = append(out, "ollama/"+m.Name)
		}
	}
	return out
}

// lmStudioModels lists models from a local LM Studio instance via its
// OpenAI-compatible /v1/models endpoint on port 1234. Honours LM_STUDIO_HOST.
// Mirrors ollamaModels' fail-fast behaviour.
func lmStudioModels() []string {
	host := os.Getenv("LM_STUDIO_HOST")
	if host == "" {
		host = "http://localhost:1234"
	} else if !strings.HasPrefix(host, "http") {
		host = "http://" + host
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/v1/models", nil)
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
			out = append(out, "lmstudio/"+m.ID)
		}
	}
	return out
}
