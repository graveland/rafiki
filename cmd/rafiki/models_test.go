package main

import (
	"testing"
)

func TestModelCompletionServesFromCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_URL", "https://example.invalid")
	t.Setenv("RAFIKI_TOKEN", "t")

	cacheWrite("models-fundi", completionEndpointKey(),
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

	cacheWrite("models-fundi", completionEndpointKey(), []string{"openai/gpt-4o"})

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
