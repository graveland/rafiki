package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// ─── loadBuiltins ──────────────────────────────────────────────────────────────

func TestLoadBuiltins_Count(t *testing.T) {
	got := loadBuiltins()
	want := len(knownModels) + len(routing.LatestFamilies())
	assert.NewCollecting(t).Len(got, want, "loadBuiltins returned %d entries, want %d (%d curated + %d family aliases)", len(got), want, len(knownModels), len(routing.LatestFamilies()))
}

// The "<family>-latest" aliases are what keeps completion current across a
// model release without anyone editing knownModels, so their presence is the
// contract rather than an implementation detail.
func TestLoadBuiltins_ContainsFamilyAliases(t *testing.T) {
	got := loadBuiltins()
	have := make(map[string]bool, len(got))
	for _, m := range got {
		have[m.ID] = true
	}
	for _, fam := range routing.LatestFamilies() {
		id := "anthropic/" + fam
		assert.NewCollecting(t).False(!have[id], "loadBuiltins is missing the family alias %s", id)
	}
}

func TestLoadBuiltins_Fields(t *testing.T) {
	c := assert.NewCollecting(t)
	got := loadBuiltins()
	// Every entry must have non-empty ID, Provider, Model, Source==builtin, no Name.
	for _, m := range got {
		c.NotEq("", m.ID, "empty ID for %+v", m)
		c.NotEq("", m.Provider, "empty Provider for ID=%s", m.ID)
		c.NotEq("", m.Model, "empty Model for ID=%s", m.ID)
		c.Eq(SourceBuiltin, m.Source, "wrong Source for ID=%s: got", m.ID)
		c.Eq("", m.Name, "unexpected Name for builtin ID=%s", m.ID)
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
	assert.NewCollecting(t).Nil(got, "expected nil for missing file, got")
}

func TestLoadUserConfig_MalformedJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	c.Require().NoError(os.MkdirAll(agentDir, 0o700))
	c.Require().NoError(os.WriteFile(filepath.Join(agentDir, "models.json"), []byte("{not json"), 0o600))
	t.Setenv("HOME", dir)
	got := loadUserConfig()
	c.Nil(got, "expected nil for malformed JSON, got")
}

func TestLoadUserConfig_ValidFile(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	c.Require().NoError(os.MkdirAll(agentDir, 0o700))
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
	c.Require().NoError(os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600))
	t.Setenv("HOME", dir)

	got := loadUserConfig()
	c.Require().Len(got, 2, "expected 2 entries, got %d", len(got))

	// Find the entry with a Name.
	var withName, withoutName *Model
	for i := range got {
		if got[i].Name != "" {
			withName = &got[i]
		} else {
			withoutName = &got[i]
		}
	}
	c.Require().NotNil(withName, "expected one entry with Name set")
	c.Eq("anthropic-work/claude-sonnet-4-5", withName.ID, "ID =")
	c.Eq("anthropic-work", withName.Provider, "Provider =")
	c.Eq("claude-sonnet-4-5", withName.Model, "Model =")
	c.Eq("Work Sonnet", withName.Name, "Name =")
	c.Eq(SourceUserConfig, withName.Source, "Source =")

	c.Require().NotNil(withoutName, "expected one entry without Name")
	c.Eq("anthropic-work/claude-opus-4-7", withoutName.ID, "no-name entry ID =")
}

func TestLoadUserConfig_Inherit(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	c.Require().NoError(os.MkdirAll(agentDir, 0o700))
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
	c.Require().NoError(os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600))
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
		c.False(!ids[want], "missing inherited entry %q", want)
	}

	// Explicit models still emit alongside inherited ones.
	c.False(!ids["anthropic-work/claude-custom"], "missing explicit entry anthropic-work/claude-custom")

	// Inheriting from an unknown provider silently produces nothing for that
	// provider (no bogus-inherit/* entries).
	for id := range ids {
		c.False(strings.HasPrefix(id, "bogus-inherit/"), "unexpected entry from unknown inherit target: %q", id)
	}
}

func TestLoadUserConfig_SkipsEmptyID(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	c.Require().NoError(os.MkdirAll(agentDir, 0o700))
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
	c.Require().NoError(os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600))
	t.Setenv("HOME", dir)

	got := loadUserConfig()
	c.Len(got, 1, "expected 1 entry (empty id skipped), got %d", len(got))
}

// ─── loadAliases ───────────────────────────────────────────────────────────────

func TestLoadAliases_Fields(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte(`
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window = 16384
`))
	c.Require().NoError(err, "Parse")

	got := loadAliases(set)
	c.Require().Len(got, 1, "expected 1 alias, got %d", len(got))
	m := got[0]
	c.Eq("vmlx/qwen", m.ID, "ID")
	c.Eq("vmlx", m.Provider, "Provider =")
	c.Eq("models/Qwen3.8-27B-Abliterated-MLX-4bit", m.Model, "Model")
	c.Eq(SourceAlias, m.Source, "Source")
}

func TestLoadAliases_NoAliasesDeclared(t *testing.T) {
	got := loadAliases(providers.Default())
	assert.NewCollecting(t).Nil(got, "expected nil with no aliases declared, got")
}

func TestLoadAliases_NilSet(t *testing.T) {
	assert.NewCollecting(t).Nil(loadAliases(nil), "expected nil for a nil set, got")
}

// A declared alias must reach both ListModels and --model completion —
// List/ListSources is the single function that serves both. base_url is
// deliberately omitted so loadLocal skips this provider entirely (it only
// probes providers with an explicit base_url); the alias must still surface
// without a reachable server, since it's config, not a live probe.
func TestList_IncludesAliases(t *testing.T) {
	set, err := providers.Parse([]byte(`
default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"

[providers.vmlx]
kind = "anthropic"

[providers.vmlx.models.qwen]
id = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window = 16384
`))
	assert.NewAborting(t).NoError(err, "Parse")

	got := List(context.Background(), set)
	for _, m := range got {
		if m.ID == "vmlx/qwen" && m.Source == SourceAlias {
			return
		}
	}
	t.Errorf("vmlx/qwen (source=alias) not found in List() output: %+v", got)
}

// ─── loadLocal ─────────────────────────────────────────────────────────────────

func TestLoadLocal_Success(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3.1:8b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	set, err := providers.Parse([]byte("default_provider = \"workstation\"\n\n[providers.workstation]\nkind = \"anthropic\"\nbase_url = \"" + srv.URL + "\"\n"))
	c.Require().NoError(err, "Parse")
	got := loadLocal(context.Background(), set)
	c.Require().Len(got, 1, "expected 1 local model, got %d", len(got))
	c.Eq("workstation/llama3.1:8b", got[0].ID, "ID =")
	c.Eq("workstation", got[0].Provider, "Provider =")
	c.Eq(SourceLocal, got[0].Source, "Source =")
}

func TestLoadLocal_Unreachable(t *testing.T) {
	c := assert.NewCollecting(t)
	set, err := providers.Parse([]byte("default_provider = \"dead\"\n\n[providers.dead]\nkind = \"anthropic\"\nbase_url = \"http://127.0.0.1:19999\"\n"))
	c.Require().NoError(err, "Parse")
	got := loadLocal(context.Background(), set)
	c.Nil(got, "expected nil for unreachable server, got")
}

func TestLoadLocal_NonOKStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	set, err := providers.Parse([]byte("default_provider = \"bad\"\n\n[providers.bad]\nkind = \"anthropic\"\nbase_url = \"" + srv.URL + "\"\n"))
	c.Require().NoError(err, "Parse")
	got := loadLocal(context.Background(), set)
	c.Nil(got, "expected nil for non-200 status, got")
}

// ─── List (deduplication) ─────────────────────────────────────────────────────

func TestList_DedupesByID(t *testing.T) {
	c := assert.NewCollecting(t)
	// Set up a user-config file that contains "anthropic/claude-sonnet-4-5"
	// (also present in builtins).  The user-config version should win.
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".pi", "agent")
	c.Require().NoError(os.MkdirAll(agentDir, 0o700))
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
	c.Require().NoError(os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o600))
	// Ensure ollama / lmstudio don't accidentally connect to anything real.
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:19993")
	t.Setenv("LM_STUDIO_HOST", "http://127.0.0.1:19994")
	t.Setenv("HOME", dir)

	got := List(context.Background(), providers.Default())

	// Count how many times "anthropic/claude-sonnet-4-5" appears.
	count := 0
	var found *Model
	for i := range got {
		if got[i].ID == "anthropic/claude-sonnet-4-5" {
			count++
			found = &got[i]
		}
	}
	c.Eq(1, count, "expected exactly 1 entry for anthropic/claude-sonnet-4-5, got")
	c.Require().NotNil(found, "anthropic/claude-sonnet-4-5 not found at all")
	// User-config wins: Name should be set and Source should be user-config.
	c.Eq("My Sonnet", found.Name, "Name")
	c.Eq(SourceUserConfig, found.Source, "Source")
}

func TestList_NoDuplicatesInBuiltins(t *testing.T) {
	// Builtins themselves must not have duplicate IDs.
	builtins := loadBuiltins()
	seen := make(map[string]int)
	for _, m := range builtins {
		seen[m.ID]++
	}
	for id, n := range seen {
		assert.NewCollecting(t).LessOrEqual(1, n, "builtin ID %q appears %d times", id, n)
	}
}

// ─── loadOpenRouter ────────────────────────────────────────────────────────────

// Anthropic ids must never come from the OpenRouter catalog. fundi routes an
// "anthropic/" prefix to the native Anthropic sender, which spells versions
// with dashes ("claude-opus-4-7") where this catalog uses dots
// ("claude-opus-4.7") — so an imported id would complete cleanly and then fail
// at call time, which is the worst of both. See the package doc.
func TestOpenRouterModels_ExcludesAnthropic(t *testing.T) {
	c := assert.NewCollecting(t)
	got := openRouterModels([]string{
		"anthropic/claude-opus-4.7",
		"anthropic/claude-sonnet-5",
		"openai/gpt-4o",
		"moonshotai/kimi-k3",
	})
	for _, m := range got {
		c.False(strings.HasPrefix(m.ID, "anthropic/"), "anthropic id leaked from the OpenRouter catalog: %s", m.ID)
	}
	c.Len(got, 2, "got %d entries, want 2 (the non-anthropic ids)", len(got))
}

func TestOpenRouterModels_Fields(t *testing.T) {
	c := assert.NewCollecting(t)
	got := openRouterModels([]string{"moonshotai/kimi-k3"})
	c.Require().Len(got, 1, "got %d entries, want 1", len(got))
	want := Model{ID: "openrouter/moonshotai/kimi-k3", Provider: "openrouter", Model: "moonshotai/kimi-k3", Source: SourceOpenRouter}
	c.Eq(want, got[0], "got")
}

// fundi requires a provider-qualified id (pkg/agent/config.go's splitModel
// returns an empty provider otherwise, and the agent rejects it), so a bare id
// in the catalog must not be offered as a completion.
func TestOpenRouterModels_SkipsUnqualified(t *testing.T) {
	got := openRouterModels([]string{"gpt-4o", "", "/leading", "trailing/", "openai/gpt-4o"})
	assert.NewCollecting(t).False(len(got) != 1 || got[0].ID != "openrouter/openai/gpt-4o", "got %+v, want only openrouter/openai/gpt-4o", got)
}

// A cancelled context must abandon the catalog rather than block: completion
// runs on a TAB press, and a hung fetch would look like a wedged shell.
func TestLoadOpenRouter_RespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := loadOpenRouter(ctx)
	assert.NewCollecting(t).Nil(got, "got %d entries on a cancelled context, want none", len(got))
}

// ─── ListSources ───────────────────────────────────────────────────────────────

// An unwanted source must not merely be filtered out of the result — it must
// never be consulted, or a pi-kind completion still pays OpenRouter's network
// round trip and the local-server probes to discard them.
func TestListSources_ConsultsOnlyRequestedSources(t *testing.T) {
	c := assert.NewCollecting(t)
	// Point a local provider at a server that records whether it was contacted.
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	set, err := providers.Parse([]byte("default_provider = \"local\"\n\n[providers.local]\nkind = \"anthropic\"\nbase_url = \"" + srv.URL + "\"\n"))
	c.Require().NoError(err, "Parse")

	got := ListSources(context.Background(), set, map[Source]bool{SourceBuiltin: true})
	c.False(hit.Load(), "Local provider was probed despite not being requested")
	for _, m := range got {
		c.Eq(SourceBuiltin, m.Source, "unrequested source in result: %s (%s)", m.Source, m.ID)
	}
	c.NotEmpty(got, "builtin source produced nothing")
}

func TestListSources_NilMeansAll(t *testing.T) {
	c := assert.NewCollecting(t)
	all := ListSources(context.Background(), providers.Default(), nil)
	viaList := List(context.Background(), providers.Default())
	c.Len(all, len(viaList), "ListSources(nil) returned %d, List returned %d — they must agree", len(all), len(viaList))
	c.Require().NotEmpty(all, "no models at all")
}
