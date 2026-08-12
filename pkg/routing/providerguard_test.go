// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testGuard() *ProviderGuard {
	return NewProviderGuard(DefaultEjectTTL, slog.New(slog.DiscardHandler))
}

// hit and miss build the two observations the guard cares about: a turn whose
// prompt was large enough to be cacheable, served from cache or not.
func miss(conv, provider string) Observation {
	return Observation{Provider: provider, Model: "deepseek/deepseek-v4-pro", Conversation: conv,
		PrefixHash: "h1", InputTokens: 50000, CacheReadTokens: 0}
}

func hit(conv, provider string) Observation {
	return Observation{Provider: provider, Model: "deepseek/deepseek-v4-pro", Conversation: conv,
		PrefixHash: "h1", InputTokens: 500, CacheReadTokens: 49500}
}

// TestGuardEjectsAfterFiveMisses proves the streak threshold: four consecutive
// qualifying misses leave routing untouched, the fifth ejects. The first
// observation in a conversation is never evidence (no previous turn to compare
// against), so six calls are needed to produce five qualifying misses.
func TestGuardEjectsAfterFiveMisses(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for i := range 5 {
		g.Observe(now, miss("c1", "CoreWeave"))
		if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 0 {
			t.Fatalf("ejected after %d observations (%d qualifying), want none yet: %v", i+1, i, got)
		}
	}
	g.Observe(now, miss("c1", "CoreWeave"))
	got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro")
	if len(got) != 1 || got[0] != "coreweave" {
		t.Fatalf("IgnoredFor = %v, want [coreweave]", got)
	}
}

// TestGuardHitResetsStreak proves a single cache hit clears the streak, so an
// intermittently-missing provider never accumulates its way to an ejection.
func TestGuardHitResetsStreak(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for range 20 {
		g.Observe(now, miss("c1", "Novita"))
		g.Observe(now, miss("c1", "Novita"))
		g.Observe(now, miss("c1", "Novita"))
		g.Observe(now, miss("c1", "Novita"))
		g.Observe(now, hit("c1", "Novita"))
	}
	if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 0 {
		t.Errorf("IgnoredFor = %v, want none", got)
	}
}

// TestGuardDisqualification proves each of the five qualification rules. In
// every case the provider misses 20 times in a row and must NOT be ejected,
// because the turns are not evidence about the provider.
func TestGuardDisqualification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(i int, o *Observation)
	}{
		{"unknown provider", func(_ int, o *Observation) { o.Provider = "" }},
		{"no prefix hash", func(_ int, o *Observation) { o.PrefixHash = "" }},
		{"no conversation", func(_ int, o *Observation) { o.Conversation = "" }},
		{"prefix changes every turn", func(i int, o *Observation) { o.PrefixHash = string(rune('a' + i)) }},
		{"provider changes every turn", func(i int, o *Observation) {
			if i%2 == 0 {
				o.Provider = "Novita"
			}
		}},
		{"prompt below the cacheable floor", func(_ int, o *Observation) { o.InputTokens = 1000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGuard()
			now := time.Now()
			for i := range 20 {
				o := miss("c1", "CoreWeave")
				tc.mutate(i, &o)
				g.Observe(now, o)
			}
			if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 0 {
				t.Errorf("IgnoredFor = %v, want none", got)
			}
		})
	}
}

// TestGuardEjectionExpires proves an ejection lapses after the TTL, so a
// provider that fixes its cache is not blacklisted forever.
func TestGuardEjectionExpires(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	if got := g.IgnoredFor(now.Add(23*time.Hour), "deepseek/deepseek-v4-pro"); len(got) != 1 {
		t.Errorf("at 23h IgnoredFor = %v, want [coreweave]", got)
	}
	if got := g.IgnoredFor(now.Add(25*time.Hour), "deepseek/deepseek-v4-pro"); len(got) != 0 {
		t.Errorf("at 25h IgnoredFor = %v, want none", got)
	}
}

// TestGuardIgnoreListCapped proves the safety valve: no matter how many
// providers break, at most three are excluded for one model line, so the guard
// cannot blacklist a model into unroutability.
func TestGuardIgnoreListCapped(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for i, p := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} {
		conv := string(rune('a' + i))
		for range 6 {
			g.Observe(now, miss(conv, p))
		}
	}
	if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 3 {
		t.Errorf("IgnoredFor = %v, want 3 entries", got)
	}
}

// TestGuardScopedToModelLine proves an ejection blames one model line only: the
// same provider stays eligible for every other model.
func TestGuardScopedToModelLine(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	if got := g.IgnoredFor(now, "z-ai/glm-5.2"); len(got) != 0 {
		t.Errorf("glm IgnoredFor = %v, want none", got)
	}
	if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 1 {
		t.Errorf("deepseek IgnoredFor = %v, want [coreweave]", got)
	}
}

// TestModelLine proves a stamped point release folds into its line, so an
// ejection recorded against one stamp still applies after OpenRouter bumps it.
func TestModelLine(t *testing.T) {
	for in, want := range map[string]string{
		"deepseek/deepseek-v4-pro-20260423": "deepseek/deepseek-v4-pro",
		"deepseek/deepseek-v4-pro":          "deepseek/deepseek-v4-pro",
		"z-ai/glm-5.2-0905":                 "z-ai/glm-5.2",
		"z-ai/glm-5.2":                      "z-ai/glm-5.2",
		"openai/gpt-4":                      "openai/gpt-4",
		"claude-haiku-4-5":                  "claude-haiku-4-5",
	} {
		if got := ModelLine(in); got != want {
			t.Errorf("ModelLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGuardNilSafe proves a nil guard is inert rather than a panic, which is
// what RAFIKI_PROVIDER_GUARD=off leaves behind at every call site.
func TestGuardNilSafe(t *testing.T) {
	var g *ProviderGuard
	g.Observe(time.Now(), miss("c1", "CoreWeave"))
	if got := g.IgnoredFor(time.Now(), "deepseek/deepseek-v4-pro"); got != nil {
		t.Errorf("IgnoredFor = %v, want nil", got)
	}
}

// replayTurn mirrors one row of the testdata fixtures.
type replayTurn struct {
	Conversation    string `json:"conversation"`
	Ordinal         int    `json:"ordinal"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	PrefixHash      string `json:"prefix_hash"`
	InputTokens     int64  `json:"input_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
}

func loadReplay(t *testing.T, name string) []replayTurn {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var turns []replayTurn
	if err := json.Unmarshal(b, &turns); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("fixture is empty")
	}
	return turns
}

// replay feeds a fixture through a guard one turn per second and returns the
// index of the turn that triggered an ejection, or -1.
func replay(t *testing.T, name string) (int, []replayTurn, *ProviderGuard) {
	t.Helper()
	turns := loadReplay(t, name)
	g := testGuard()
	base := time.Now()
	ejectedAt := -1
	for i, tn := range turns {
		now := base.Add(time.Duration(i) * time.Second)
		g.Observe(now, Observation{
			Provider: tn.Provider, Model: tn.Model, Conversation: tn.Conversation,
			PrefixHash: tn.PrefixHash, InputTokens: tn.InputTokens, CacheReadTokens: tn.CacheReadTokens,
		})
		if ejectedAt < 0 && len(g.IgnoredFor(now, tn.Model)) > 0 {
			ejectedAt = i
		}
	}
	return ejectedAt, turns, g
}

// TestReplayHealthyNovitaNeverEjects is the false-positive guard. This is 13
// hours of real traffic that was working correctly (98-99% of input tokens
// served from cache). Twelve of its turns did miss the cache, but never twice
// in a row. If a change to the qualification rules starts ejecting here, that
// change would have blacklisted a healthy provider in production.
func TestReplayHealthyNovitaNeverEjects(t *testing.T) {
	ejectedAt, turns, _ := replay(t, "novita_healthy.json")
	if ejectedAt >= 0 {
		t.Fatalf("ejected at turn %d (conversation %s, ordinal %d) replaying healthy traffic; want no ejection",
			ejectedAt, turns[ejectedAt].Conversation, turns[ejectedAt].Ordinal)
	}
}

// TestReplayBrokenCoreWeaveEjects is the true-positive test. This is the real
// 2026-08-12 incident: 214 turns, 201 of them cache misses, 15.6x the cost per
// input token. The guard must catch it within a handful of turns rather than
// after 207 of them.
func TestReplayBrokenCoreWeaveEjects(t *testing.T) {
	ejectedAt, turns, g := replay(t, "coreweave_broken.json")
	if ejectedAt < 0 {
		t.Fatal("replaying the CoreWeave incident produced no ejection")
	}
	if ejectedAt > 10 {
		t.Errorf("ejected at turn %d, want within the first 10 — detection cost is the uncached turns before it fires", ejectedAt)
	}
	if got := g.IgnoredFor(time.Now(), "deepseek/deepseek-v4-pro"); len(got) != 1 || got[0] != "coreweave" {
		t.Errorf("IgnoredFor = %v, want [coreweave]", got)
	}
	// Quantify what the guard saved, so a regression that delays detection is
	// visible as a number rather than a boolean.
	var wasted int64
	for _, tn := range turns[:ejectedAt+1] {
		wasted += tn.InputTokens
	}
	t.Logf("ejected at turn %d/%d after %d uncached input tokens", ejectedAt, len(turns), wasted)
}
