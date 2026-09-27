package fundi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/agentloop"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

func TestThinkingBudgetFor(t *testing.T) {
	cases := []struct {
		level string
		want  int64
	}{
		{"", 0},
		{"off", 0},
		{"low", 4096},
		{"medium", 8192},
		{"high", 16384},
		{"xhigh", 32768},
	}
	for _, tc := range cases {
		got, err := ThinkingBudgetFor(tc.level)
		if err != nil {
			t.Errorf("ThinkingBudgetFor(%q): unexpected error: %v", tc.level, err)
			continue
		}
		assert.NewCollecting(t).Eq(tc.want, got, "ThinkingBudgetFor(%q) = %d, want", tc.level, got)
	}
}

func TestThinkingBudgetForUnknownLevel(t *testing.T) {
	_, err := ThinkingBudgetFor("turbo")
	assert.NewAborting(t).Error(err, "ThinkingBudgetFor(\"turbo\"): want error, got nil")
}

// writeFakeTurns writes bodies (pretty-printed JSON anthropic.Message values)
// as one ndjson file under t.TempDir and returns its path, mirroring
// scriptedSender (engine_test.go) but returning the path itself rather than a
// loaded Sender - Config.FakeTurns is a path, not a llm.Sender.
func writeFakeTurns(t *testing.T, bodies ...string) string {
	t.Helper()
	c := assert.NewAborting(t)
	var lines []string
	for _, b := range bodies {
		var compact bytes.Buffer
		c.NoError(json.Compact(&compact, []byte(b)), "compact scripted body")
		lines = append(lines, compact.String())
	}
	path := filepath.Join(t.TempDir(), "turns.ndjson")
	c.NoError(os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600), "write scripted turns")
	return path
}

// TestBuildEngineFakeTurnsEndToEnd drives Task 7's scripted tool-use loop
// (tool_use then end_turn) through Config.BuildEngine end to end: this is
// the --fake-turns test seam exercising the full construction path (client,
// conversation options, Engine, Frontend wiring) with no API key and no
// network.
func TestBuildEngineFakeTurnsEndToEnd(t *testing.T) {
	c := assert.NewAborting(t)
	silenceSlog(t)

	var ranCommand string
	tools := fakeToolSet{
		"bash": func(_ context.Context, in json.RawMessage) (string, error) {
			var input struct {
				Command string `json:"command"`
			}
			c.NoError(json.Unmarshal(in, &input), "unmarshal tool input")
			ranCommand = input.Command
			return "total 0", nil
		},
	}

	cfg := Config{
		Model:     "anthropic/claude-x",
		Cwd:       t.TempDir(),
		Name:      "w1",
		FakeTurns: writeFakeTurns(t, sampleResp, sampleEndTurn),
		Tools:     tools,
		Providers: providers.Default(),
	}

	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)

	eng, shutdown, err := cfg.BuildEngine(context.Background(), fe)
	c.NoError(err, "BuildEngine")
	defer shutdown()

	eng.HandlePrompt("list files")
	eng.Wait()
	eng.Close()

	c.Eq("ls", ranCommand, "ran command")
	types := frameTypes(t, out.String())
	c.False(len(types) == 0 || types[0] != "message_start", "frame types = %v, want to start with message_start (the user echo)", types)
	var sawToolStart, sawEnd bool
	for _, ty := range types {
		switch ty {
		case "tool_execution_start":
			sawToolStart = true
		case "agent_end":
			sawEnd = true
		}
	}
	c.True(sawToolStart, "frame types = %v, want a tool_execution_start frame (the scripted tool_use)", types)
	c.True(sawEnd, "frame types = %v, want an agent_end frame", types)

	if got := eng.State(); got.SessionName != "w1" || got.ModelID != "claude-x" || got.Provider != "anthropic" {
		t.Fatalf("State() = %+v, want SessionName=w1 ModelID=claude-x Provider=anthropic", got)
	}
}

func TestBuildEngineRequiresTools(t *testing.T) {
	cfg := Config{Model: "anthropic/claude-x", FakeTurns: writeFakeTurns(t, sampleEndTurn), Providers: providers.Default()}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	_, _, err := cfg.BuildEngine(context.Background(), fe)
	assert.NewAborting(t).Error(err, "BuildEngine with nil Tools: want error, got nil")
}

// TestBuildEngineMissingAPIKey an agent's config with no Providers set fails validation
// regardless of model (Providers is always required).
func TestBuildEngineMissingAPIKey(t *testing.T) {
	cfg := Config{Model: "anthropic/claude-x", Tools: fakeToolSet{}}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	_, _, err := cfg.BuildEngine(context.Background(), fe)
	assert.NewAborting(t).Error(err, "BuildEngine with no Providers: want error, got nil")
}

// TestBuildEngineMissingAPIKeyNonAnthropicModel verifies that a config
// with Providers but a model naming no configured provider fails validation.
func TestBuildEngineMissingAPIKeyNonAnthropicModel(t *testing.T) {
	cfg := Config{Model: "deepseek/deepseek-chat", Tools: fakeToolSet{}, Providers: providers.Default()}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	_, _, err := cfg.BuildEngine(context.Background(), fe)
	assert.NewAborting(t).Error(err, "BuildEngine with a model naming no configured provider: want error, got nil")
}

// TestBuildEngineNonAnthropicModelRequiresOpenRouterKey verifies a
// provider-qualified model routed to OpenRouter works with the default config.
func TestBuildEngineNonAnthropicModelRequiresOpenRouterKey(t *testing.T) {
	cfg := Config{Model: "openrouter/meta-llama/llama-3.1-70b", Tools: fakeToolSet{}, Providers: providers.Default()}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	_, _, err := cfg.BuildEngine(context.Background(), fe)
	assert.NewAborting(t).NoError(err, "BuildEngine with a configured openrouter model")
}

var _ agentloop.ToolSet = fakeToolSet{}

func TestBuildEngineNeedsNoAnthropicKeyForLocalModel(t *testing.T) {
	ck := assert.NewAborting(t)
	set, err := providers.Parse([]byte(`
default_provider = "vmlx"

[providers.vmlx]
kind = "anthropic"
base_url = "http://127.0.0.1:1"
`))
	ck.NoError(err, "Parse")
	c := Config{Model: "vmlx/qwen3", Providers: set}
	ck.NoError(c.Validate(), "Validate: a keyless local provider must need no ANTHROPIC_API_KEY")
}

func TestValidateRejectsUnknownProvider(t *testing.T) {
	ck := assert.NewCollecting(t)
	set := providers.Default()
	c := Config{Model: "deepseek/deepseek-chat", Providers: set}
	err := c.Validate()
	ck.Require().Error(err, "Validate accepted a model naming no configured provider")
	ck.StrContains(err.Error(), "unknown provider", "error = %q, want \"unknown provider\"", err.Error())
}

func TestValidateRejectsMissingRegistry(t *testing.T) {
	c := Config{Model: "anthropic/claude-sonnet-5"}
	assert.NewAborting(t).Error(c.Validate(), "Validate accepted a Config with no provider registry")
}

// buildCatalogEngine is the shared construction helper for the catalog tests:
// it builds one engine through Config.BuildEngine exactly as the daemon's
// in-process runner does (fake turns, no network), so the test observes what
// client — and therefore which model catalog — the engine really ended up
// with. The caller owns closing the engine.
func buildCatalogEngine(t *testing.T, cat *routing.ModelCatalog) *Engine {
	t.Helper()
	silenceSlog(t)
	cfg := Config{
		Model:     "anthropic/claude-x",
		Cwd:       t.TempDir(),
		Name:      "catalog-under-test",
		FakeTurns: writeFakeTurns(t, sampleEndTurn),
		Tools:     fakeToolSet{},
		Providers: providers.Default(),
		Catalog:   cat,
	}
	eng, shutdown, err := cfg.BuildEngine(context.Background(), NewFrontend(strings.NewReader(""), &syncBuffer{}, nil))
	assert.NewAborting(t).NoError(err, "BuildEngine")
	t.Cleanup(shutdown)
	return eng
}

// TestBuildEngineUsesSharedCatalog proves Config.Catalog reaches the engine's
// llm.Client as THE instance, not a copy or a re-defaulted one. Every
// in-process fundi child used to build its own catalog, fetching OpenRouter's
// whole model list per child; threading the daemon's shared instance through
// Config.Catalog is what makes that one fetch and one copy daemon-wide. The
// seeded entry also pins that pricing lookups answer from the shared data.
func TestBuildEngineUsesSharedCatalog(t *testing.T) {
	cat := routing.NewModelCatalog(nil, time.Hour, nil)
	cat.SeedForTest([]routing.CatalogEntry{{ID: "claude-x", Name: "shared catalog model"}})

	eng := buildCatalogEngine(t, cat)
	defer eng.Close()

	got := catalogOf(eng.client)
	assert.NewAborting(t).Eq(cat, got, "engine's client uses catalog")
}

// TestBuildEngineWithoutSharedCatalogBuildsOwn pins the standalone `rafikid
// fundi` contract behind the shared-catalog change: with no Catalog handed in,
// each engine's client still defaults its own (non-nil) catalog, and two
// engines never share one instance — nil must mean "build your own", not
// "reuse something process-global".
func TestBuildEngineWithoutSharedCatalogBuildsOwn(t *testing.T) {
	c := assert.NewAborting(t)
	a := buildCatalogEngine(t, nil)
	b := buildCatalogEngine(t, nil)
	defer a.Close()
	defer b.Close()

	catA, catB := catalogOf(a.client), catalogOf(b.client)
	c.False(catA == nil || catB == nil, "standalone engines' catalogs = %p, %p; llm.NewClient was expected to default one in for each", catA, catB)
	c.NotEq(catB, catA, "two standalone engines share one catalog instance (")
}
