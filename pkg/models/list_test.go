package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── loadBuiltins ──────────────────────────────────────────────────────────────

func TestLoadBuiltins_Count(t *testing.T) {
	got := loadBuiltins()
	if len(got) != len(knownModels) {
		t.Errorf("loadBuiltins returned %d entries, want %d", len(got), len(knownModels))
	}
}

func TestLoadBuiltins_Fields(t *testing.T) {
	got := loadBuiltins()
	// Every entry must have non-empty ID, Provider, Model, Source==builtin, no Name.
	for _, m := range got {
		if m.ID == "" {
			t.Errorf("empty ID for %+v", m)
		}
		if m.Provider == "" {
			t.Errorf("empty Provider for ID=%s", m.ID)
		}
		if m.Model == "" {
			t.Errorf("empty Model for ID=%s", m.ID)
		}
		if m.Source != SourceBuiltin {
			t.Errorf("wrong Source for ID=%s: got %s", m.ID, m.Source)
		}
		if m.Name != "" {
			t.Errorf("unexpected Name for builtin ID=%s: %s", m.ID, m.Name)
		}
	}
}

func TestLoadBuiltins_ContainsAnthropicSonnet(t *testing.T) {
	got := loadBuiltins()
	for _, m := range got {
		if m.ID == "anthropic/claude-sonnet-4-5" {
			return // found
		}
	}
	t.Error("loadBuiltins does not contain anthropic/claude-sonnet-4-5")
}

// ─── loadUserConfig ────────────────────────────────────────────────────────────

func TestLoadUserConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got := loadUserConfig()
	if got != nil {
		t.Errorf("expected nil for missing file, got %v", got)
	}
}

func TestLoadUserConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	got := loadUserConfig()
	if got != nil {
		t.Errorf("expected nil for malformed JSON, got %v", got)
	}
}

func TestLoadUserConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"providers": map[string]any{
			"anthropic-work": map[string]any{
				"models": []any{
					map[string]any{"id": "claude-sonnet-4-5", "name": "Work Sonnet"},
					map[string]any{"id": "claude-opus-4-7"},
				},
			},
		},
	}
	b, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	got := loadUserConfig()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}

	// Find the entry with a Name.
	var withName, withoutName *Model
	for i := range got {
		if got[i].Name != "" {
			withName = &got[i]
		} else {
			withoutName = &got[i]
		}
	}
	if withName == nil {
		t.Fatal("expected one entry with Name set")
	}
	if withName.ID != "anthropic-work/claude-sonnet-4-5" {
		t.Errorf("ID = %q", withName.ID)
	}
	if withName.Provider != "anthropic-work" {
		t.Errorf("Provider = %q", withName.Provider)
	}
	if withName.Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q", withName.Model)
	}
	if withName.Name != "Work Sonnet" {
		t.Errorf("Name = %q", withName.Name)
	}
	if withName.Source != SourceUserConfig {
		t.Errorf("Source = %q", withName.Source)
	}

	if withoutName == nil {
		t.Fatal("expected one entry without Name")
	}
	if withoutName.ID != "anthropic-work/claude-opus-4-7" {
		t.Errorf("no-name entry ID = %q", withoutName.ID)
	}
}

func TestLoadUserConfig_Inherit(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"providers": map[string]any{
			"anthropic-work": map[string]any{
				"inherit": "anthropic",
				"models": []any{
					map[string]any{"id": "claude-custom"},
				},
			},
			"bogus-inherit": map[string]any{
				"inherit": "no-such-provider",
			},
		},
	}
	b, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	got := loadUserConfig()

	ids := make(map[string]bool, len(got))
	for _, m := range got {
		ids[m.ID] = true
	}

	// Inherited anthropic models should be re-prefixed under anthropic-work.
	for _, want := range []string{
		"anthropic-work/claude-opus-4-7",
		"anthropic-work/claude-sonnet-4-5",
		"anthropic-work/claude-haiku-4-5",
	} {
		if !ids[want] {
			t.Errorf("missing inherited entry %q", want)
		}
	}

	// Explicit models still emit alongside inherited ones.
	if !ids["anthropic-work/claude-custom"] {
		t.Errorf("missing explicit entry anthropic-work/claude-custom")
	}

	// Inheriting from an unknown provider silently produces nothing for that
	// provider (no bogus-inherit/* entries).
	for id := range ids {
		if strings.HasPrefix(id, "bogus-inherit/") {
			t.Errorf("unexpected entry from unknown inherit target: %q", id)
		}
	}
}

func TestLoadUserConfig_SkipsEmptyID(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"providers": map[string]any{
			"test": map[string]any{
				"models": []any{
					map[string]any{"id": ""},
					map[string]any{"id": "valid-model"},
				},
			},
		},
	}
	b, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)

	got := loadUserConfig()
	if len(got) != 1 {
		t.Errorf("expected 1 entry (empty id skipped), got %d", len(got))
	}
}

// ─── loadOllama ───────────────────────────────────────────────────────────────

func TestLoadOllama_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.1:8b"},{"name":"mistral:7b"}]}`))
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_HOST", srv.URL)
	got := loadOllama(context.Background())
	if len(got) != 2 {
		t.Fatalf("expected 2 ollama models, got %d", len(got))
	}
	if got[0].ID != "ollama/llama3.1:8b" {
		t.Errorf("ID = %q", got[0].ID)
	}
	if got[0].Provider != "ollama" {
		t.Errorf("Provider = %q", got[0].Provider)
	}
	if got[0].Source != SourceOllama {
		t.Errorf("Source = %q", got[0].Source)
	}
}

func TestLoadOllama_Unreachable(t *testing.T) {
	// Point at a port that (almost certainly) has nothing listening.
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:19991")
	got := loadOllama(context.Background())
	if got != nil {
		t.Errorf("expected nil for unreachable server, got %v", got)
	}
}

func TestLoadOllama_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	got := loadOllama(context.Background())
	if got != nil {
		t.Errorf("expected nil for non-200 status, got %v", got)
	}
}

// ─── loadLMStudio ─────────────────────────────────────────────────────────────

func TestLoadLMStudio_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-3.1-8b-instruct"},{"id":"gemma-2-9b"}]}`))
	}))
	defer srv.Close()

	t.Setenv("LM_STUDIO_HOST", srv.URL)
	got := loadLMStudio(context.Background())
	if len(got) != 2 {
		t.Fatalf("expected 2 lmstudio models, got %d", len(got))
	}
	if got[0].ID != "lmstudio/llama-3.1-8b-instruct" {
		t.Errorf("ID = %q", got[0].ID)
	}
	if got[0].Provider != "lmstudio" {
		t.Errorf("Provider = %q", got[0].Provider)
	}
	if got[0].Source != SourceLMStudio {
		t.Errorf("Source = %q", got[0].Source)
	}
}

func TestLoadLMStudio_Unreachable(t *testing.T) {
	t.Setenv("LM_STUDIO_HOST", "http://127.0.0.1:19992")
	got := loadLMStudio(context.Background())
	if got != nil {
		t.Errorf("expected nil for unreachable server, got %v", got)
	}
}

// ─── List (deduplication) ─────────────────────────────────────────────────────

func TestList_DedupesByID(t *testing.T) {
	// Set up a user-config file that contains "anthropic/claude-sonnet-4-5"
	// (also present in builtins).  The user-config version should win.
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := map[string]any{
		"providers": map[string]any{
			"anthropic": map[string]any{
				"models": []any{
					map[string]any{
						"id":   "claude-sonnet-4-5",
						"name": "My Sonnet",
					},
				},
			},
		},
	}
	b, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Ensure ollama / lmstudio don't accidentally connect to anything real.
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:19993")
	t.Setenv("LM_STUDIO_HOST", "http://127.0.0.1:19994")
	t.Setenv("HOME", dir)

	got := List(context.Background())

	// Count how many times "anthropic/claude-sonnet-4-5" appears.
	count := 0
	var found *Model
	for i := range got {
		if got[i].ID == "anthropic/claude-sonnet-4-5" {
			count++
			found = &got[i]
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry for anthropic/claude-sonnet-4-5, got %d", count)
	}
	if found == nil {
		t.Fatal("anthropic/claude-sonnet-4-5 not found at all")
	}
	// User-config wins: Name should be set and Source should be user-config.
	if found.Name != "My Sonnet" {
		t.Errorf("Name = %q, want user-config name (user-config wins)", found.Name)
	}
	if found.Source != SourceUserConfig {
		t.Errorf("Source = %q, want user-config", found.Source)
	}
}

func TestList_NoDuplicatesInBuiltins(t *testing.T) {
	// Builtins themselves must not have duplicate IDs.
	builtins := loadBuiltins()
	seen := make(map[string]int)
	for _, m := range builtins {
		seen[m.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("builtin ID %q appears %d times", id, n)
		}
	}
}
