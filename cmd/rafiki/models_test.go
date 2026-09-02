package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestModelCompletionServesFromCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("models-fundi", completionEndpointKey(nil),
		[]string{"anthropic/claude-opus-5", "openai/gpt-4o"})

	got := completeModel(nil, "fundi", "anthropic/")
	if len(got) != 1 || got[0] != "anthropic/claude-opus-5" {
		t.Errorf("got %v, want the one anthropic id", got)
	}
}

// The two kinds have different model universes, so their caches must not share
// a file — a claude completion served from the fundi cache offers OpenRouter
// ids that Claude Code cannot resolve.
func TestModelCompletionCacheIsKeyedByKind(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("models-fundi", completionEndpointKey(nil), []string{"openai/gpt-4o"})

	if got := completeModel(nil, "claude", ""); len(got) != 0 {
		t.Errorf("claude completion read the fundi cache: %v", got)
	}
}

func TestModelCompletionOnAnUnreachableDaemonIsEmpty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://127.0.0.1:1")
	t.Setenv("RAFIKI_TOKEN", "t")

	if got := completeModel(nil, "fundi", ""); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// ─── renderModelRows ───────────────────────────────────────────────────────

func modelTestRows() []*rafikiv1.ModelRow {
	zero := int32(0)
	return []*rafikiv1.ModelRow{
		{Id: "anthropic/claude-opus-5", Provider: "anthropic", Source: "builtin", ContextWindow: &zero},
		{Id: "openrouter/openai/gpt-4o", Provider: "openrouter", Source: "openrouter"},
		{Id: "vmlx/qwen", Provider: "vmlx", Source: "local"},
	}
}

// The JSON arm is what `rafiki models | jq` reads. Optional fields that the
// daemon did not report must be ABSENT, not zero: a zero context window would
// sort every local model to the top of a cheapest-first filter, and a zero
// price reads as free.
func TestRenderModelRowsJSONOmitsUnreportedOptionals(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModelRows(&buf, modelTestRows(), "", outputJSON); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}

	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode JSON output: %v\nraw:\n%s", err, buf.String())
	}
	if len(out.Models) != 3 {
		t.Fatalf("got %d rows, want 3; output:\n%s", len(out.Models), buf.String())
	}

	byID := make(map[string]map[string]any, len(out.Models))
	for _, m := range out.Models {
		byID[m["id"].(string)] = m
	}

	// A set zero is a REAL value and must print as 0.
	if v, ok := byID["anthropic/claude-opus-5"]["context_window"]; !ok || v != float64(0) {
		t.Errorf("claude context_window = %v (%v), want an explicit 0", v, byID["anthropic/claude-opus-5"])
	}
	// Unreported optionals must be absent, not zero.
	for _, key := range []string{"context_window", "prompt_usd", "completion_usd", "input_modalities"} {
		if _, present := byID["vmlx/qwen"][key]; present {
			t.Errorf("local model vmlx/qwen carries %s; an unreported optional must be absent, not zero", key)
		}
	}
}

// The --source filter is a display filter and must apply in the JSON arm too —
// a jq pipeline filtering on source gets no help from the table renderer.
func TestRenderModelRowsJSONAppliesSourceFilter(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModelRows(&buf, modelTestRows(), "builtin", outputJSON); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	if strings.Contains(buf.String(), "openrouter/openai/gpt-4o") {
		t.Errorf("--source builtin leaked a non-builtin row; output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "anthropic/claude-opus-5") {
		t.Errorf("--source builtin dropped the builtin row; output:\n%s", buf.String())
	}
}

func TestRenderModelRowsTable(t *testing.T) {
	var buf bytes.Buffer
	if err := renderModelRows(&buf, modelTestRows(), "", outputTable); err != nil {
		t.Fatalf("renderModelRows: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"anthropic/claude-opus-5", "CONTEXT", "IN $/M", "VISION"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q; output:\n%s", want, out)
		}
	}
}
