// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The behaviour that must never regress: an unavailable catalog returns 0,
// which leaves Claude Code's own compaction default alone and lets the session
// start. `rafiki claude` working with the daemon down is the difference
// between "the daemon is down" and "I cannot start a coding session".
//
// The current implementation fetches the OpenRouter catalog over HTTPS when
// the cache is cold, so "unavailable" is simulated hermetically by pointing
// the process proxy at a dead address — the fetch fails fast and the catalog
// stays empty, which is exactly the offline state. (Nothing else in this test
// binary makes an HTTP call, so the proxy config is read fresh.)
func TestAutoCompactWindowReturnsZeroWhenTheCatalogIsUnavailable(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	got := claudeAutoCompactWindow(context.Background(), "anthropic/claude-opus-5", t.TempDir())
	if got != 0 {
		t.Fatalf("an empty cache dir with an unreachable network must yield 0, got %d", got)
	}
}

func TestAutoCompactWindowReturnsZeroForAnUnknownModel(t *testing.T) {
	dir := seedCatalog(t, map[string]int{"anthropic/claude-opus-5": 200000})
	if got := claudeAutoCompactWindow(context.Background(), "who/knows", dir); got != 0 {
		t.Fatalf("an unknown model must yield 0, got %d", got)
	}
}

// Bounded: a slow lookup must not delay the launch. The current code races the
// warm against a budget; whatever replaces it must keep that property.
func TestAutoCompactWindowIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead
	start := time.Now()
	if got := claudeAutoCompactWindow(ctx, "anthropic/claude-opus-5", t.TempDir()); got != 0 {
		t.Fatalf("got %d", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s; the lookup must never delay a launch", d)
	}
}

// A known model yields a window below its context length, reserving between 5%
// and 10% — the AutoCompactWindow contract. Asserted here so the client keeps
// getting the same answer after the computation moves to the daemon.
func TestAutoCompactWindowReservesBetweenFiveAndTenPercent(t *testing.T) {
	const ctxLen = 200000
	dir := seedCatalog(t, map[string]int{"anthropic/claude-opus-5": ctxLen})
	got := claudeAutoCompactWindow(context.Background(), "anthropic/claude-opus-5", dir)
	if got == 0 {
		t.Fatal("a seeded catalog must produce a window")
	}
	reserve := ctxLen - got
	if reserve < ctxLen/20 || reserve > ctxLen/10 {
		t.Fatalf("reserve %d is outside the 5%%–10%% band for a %d window", reserve, ctxLen)
	}
}

// seedCatalog writes an openrouter_catalog.json snapshot in the shape
// routing.ModelCatalog.loadCache reads (catalogSnapshot{Models []orModel}),
// so claudeAutoCompactWindow can resolve the given models without a network
// fetch. Returns the directory containing the snapshot.
func seedCatalog(t *testing.T, entries map[string]int) string {
	t.Helper()
	dir := t.TempDir()

	type topProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	type pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	}
	type orModel struct {
		ID          string      `json:"id"`
		Created     int64       `json:"created"`
		ContextLen  int         `json:"context_length"`
		TopProvider topProvider `json:"top_provider"`
		Pricing     pricing     `json:"pricing"`
	}
	snapshot := struct {
		Fetched time.Time `json:"fetched"`
		Models  []orModel `json:"models"`
	}{Fetched: time.Now()}

	for id, ctxLen := range entries {
		snapshot.Models = append(snapshot.Models, orModel{
			ID:         id,
			Created:    1,
			ContextLen: ctxLen,
			Pricing:    pricing{Prompt: "1", Completion: "1"},
		})
	}

	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "openrouter_catalog.json"), b, 0o644); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return dir
}
