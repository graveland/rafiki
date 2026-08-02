package fundi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/agentloop"
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
		if got != tc.want {
			t.Errorf("ThinkingBudgetFor(%q) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

func TestThinkingBudgetForUnknownLevel(t *testing.T) {
	if _, err := ThinkingBudgetFor("turbo"); err == nil {
		t.Fatal("ThinkingBudgetFor(\"turbo\"): want error, got nil")
	}
}

// writeFakeTurns writes bodies (pretty-printed JSON anthropic.Message values)
// as one ndjson file under t.TempDir and returns its path, mirroring
// scriptedSender (engine_test.go) but returning the path itself rather than a
// loaded Sender - Config.FakeTurns is a path, not a llm.Sender.
func writeFakeTurns(t *testing.T, bodies ...string) string {
	t.Helper()
	var lines []string
	for _, b := range bodies {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(b)); err != nil {
			t.Fatalf("compact scripted body: %v", err)
		}
		lines = append(lines, compact.String())
	}
	path := filepath.Join(t.TempDir(), "turns.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write scripted turns: %v", err)
	}
	return path
}

// TestBuildEngineFakeTurnsEndToEnd drives Task 7's scripted tool-use loop
// (tool_use then end_turn) through Config.BuildEngine end to end: this is
// the --fake-turns test seam exercising the full construction path (client,
// conversation options, Engine, Frontend wiring) with no API key and no
// network.
func TestBuildEngineFakeTurnsEndToEnd(t *testing.T) {
	silenceSlog(t)

	var ranCommand string
	tools := fakeToolSet{
		"bash": func(_ context.Context, in json.RawMessage) (string, error) {
			var input struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(in, &input); err != nil {
				t.Fatalf("unmarshal tool input: %v", err)
			}
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
	}

	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)

	eng, shutdown, err := cfg.BuildEngine(context.Background(), fe)
	if err != nil {
		t.Fatalf("BuildEngine: %v", err)
	}
	defer shutdown()

	eng.HandlePrompt("list files")
	eng.Wait()
	eng.Close()

	if ranCommand != "ls" {
		t.Fatalf("ran command = %q, want %q (tool_use from the scripted turn never dispatched)", ranCommand, "ls")
	}
	types := frameTypes(t, out.String())
	if len(types) == 0 || types[0] != "message_start" {
		t.Fatalf("frame types = %v, want to start with message_start (the user echo)", types)
	}
	var sawToolStart, sawEnd bool
	for _, ty := range types {
		switch ty {
		case "tool_execution_start":
			sawToolStart = true
		case "agent_end":
			sawEnd = true
		}
	}
	if !sawToolStart {
		t.Fatalf("frame types = %v, want a tool_execution_start frame (the scripted tool_use)", types)
	}
	if !sawEnd {
		t.Fatalf("frame types = %v, want an agent_end frame", types)
	}

	if got := eng.State(); got.SessionName != "w1" || got.ModelID != "claude-x" || got.Provider != "anthropic" {
		t.Fatalf("State() = %+v, want SessionName=w1 ModelID=claude-x Provider=anthropic", got)
	}
}

func TestBuildEngineRequiresTools(t *testing.T) {
	cfg := Config{Model: "anthropic/claude-x", FakeTurns: writeFakeTurns(t, sampleEndTurn)}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	if _, _, err := cfg.BuildEngine(context.Background(), fe); err == nil {
		t.Fatal("BuildEngine with nil Tools: want error, got nil")
	}
}

// TestBuildEngineMissingAPIKey covers the always-mandatory ANTHROPIC_API_KEY
// check: rafiki's llm.NewClient requires an Anthropic sender unconditionally,
// so this errors regardless of which model is configured (see
// TestBuildEngineMissingAPIKeyNonAnthropicModel for the non-anthropic case).
func TestBuildEngineMissingAPIKey(t *testing.T) {
	cfg := Config{Model: "anthropic/claude-x", Tools: fakeToolSet{}}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	if _, _, err := cfg.BuildEngine(context.Background(), fe); err == nil {
		t.Fatal("BuildEngine with no ANTHROPIC_API_KEY and no --fake-turns: want error, got nil")
	}
}

// TestBuildEngineMissingAPIKeyNonAnthropicModel confirms ANTHROPIC_API_KEY is
// required even for a non-anthropic (OpenRouter-routed) model - rafiki's
// llm.NewClient always needs an Anthropic sender, regardless of routing.
func TestBuildEngineMissingAPIKeyNonAnthropicModel(t *testing.T) {
	cfg := Config{Model: "deepseek/deepseek-chat", Tools: fakeToolSet{}, OpenRouterAPIKey: "sk-or-not-relevant"}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	if _, _, err := cfg.BuildEngine(context.Background(), fe); err == nil {
		t.Fatal("BuildEngine with no ANTHROPIC_API_KEY (non-anthropic model): want error, got nil")
	}
}

// TestBuildEngineNonAnthropicModelRequiresOpenRouterKey covers the other
// mandatory-key check: a model with no "anthropic/" prefix routes to
// OpenRouter, so OPENROUTER_API_KEY is required even though ANTHROPIC_API_KEY
// is (always) also required.
func TestBuildEngineNonAnthropicModelRequiresOpenRouterKey(t *testing.T) {
	cfg := Config{Model: "meta-llama/llama-3.1-70b", Tools: fakeToolSet{}, AnthropicAPIKey: "sk-anthropic-not-relevant"}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	if _, _, err := cfg.BuildEngine(context.Background(), fe); err == nil {
		t.Fatal("BuildEngine with a non-anthropic model and no OPENROUTER_API_KEY: want error, got nil")
	}
}

var _ agentloop.ToolSet = fakeToolSet{}
