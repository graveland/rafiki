// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
)

// seedTurns creates a conversation on the given path (owner uniquely derived
// from drivenBy) with a single complete turn carrying the supplied token counts.
func seedTurns(t *testing.T, pool *pgxpool.Pool, drivenBy string, inTok, cacheRead int64) string {
	t.Helper()
	convID := insertConversation(t, pool, drivenBy, "owner-"+drivenBy)
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 0, model: "claude-fable-5", source: "claude", upstream: "anthropic",
		inTok: inTok, outTok: 50, cacheRead: cacheRead, latencyMS: 1000,
		prefixHash: "hash-" + drivenBy,
	})
	return convID
}

func TestGlobalStats_CacheHitByPath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedTurns(t, pool, "client", 100, 900) // proxy: 900/(100+900) = 0.9
	seedTurns(t, pool, "server", 100, 0)   // direct: 0/(100+0) = 0.0

	s, err := New(pool).GlobalStats(ctx, StatsFilter{})
	if err != nil {
		t.Fatalf("global stats: %v", err)
	}
	if got := s.ByPath[string(PathProxy)].CacheHitRatio; !inDelta(got, 0.9, 0.01) {
		t.Errorf("proxy cache-hit ratio = %v, want ~0.9", got)
	}
	if got := s.ByPath[string(PathDirect)].CacheHitRatio; !inDelta(got, 0.0, 0.01) {
		t.Errorf("direct cache-hit ratio = %v, want ~0.0", got)
	}
	if s.Adoption.DistinctOwners < 2 {
		t.Errorf("distinct owners = %d, want >= 2", s.Adoption.DistinctOwners)
	}
	if s.Volume.Conversations != 2 || s.Volume.Turns != 2 {
		t.Errorf("volume = %d conversations / %d turns, want 2/2", s.Volume.Conversations, s.Volume.Turns)
	}
	// Overall cache-hit ratio: 900 / (200 + 900) ≈ 0.818.
	if got := s.Tokens.CacheHitRatio; !inDelta(got, 900.0/1100.0, 0.01) {
		t.Errorf("overall cache-hit ratio = %v, want ~0.818", got)
	}
}

func TestGlobalStats_PathFilterAndFacets(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedTurns(t, pool, "client", 100, 900)
	seedTurns(t, pool, "server", 100, 0)

	proxy, err := New(pool).GlobalStats(ctx, StatsFilter{Path: PathProxy})
	if err != nil {
		t.Fatalf("proxy stats: %v", err)
	}
	if proxy.Volume.Conversations != 1 {
		t.Errorf("proxy conversations = %d, want 1", proxy.Volume.Conversations)
	}
	if proxy.Failures.Turns != 1 || proxy.Failures.Errors != 0 {
		t.Errorf("proxy failures = %d/%d, want 1 turns / 0 errors", proxy.Failures.Turns, proxy.Failures.Errors)
	}
	if proxy.Latency.P50 <= 0 {
		t.Errorf("proxy p50 latency = %v, want > 0", proxy.Latency.P50)
	}
	if len(proxy.Cost) != 1 || proxy.Cost[0].Model != "claude-fable-5" {
		t.Errorf("proxy cost rows = %+v, want one claude-fable-5 row", proxy.Cost)
	}
}

func TestGlobalStats_CacheWasteAndPrefix(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	// A large-prompt turn with zero cache_read is waste; give it a shared prefix
	// across two conversations and one owner so cross-user reuse is 0.
	c1 := insertConversation(t, pool, "client", "dave")
	insertTurn(t, pool, c1, seedTurn{ordinal: 0, model: "m", inTok: 10000, cacheRead: 0, prefixHash: "shared"})
	c2 := insertConversation(t, pool, "client", "dave")
	insertTurn(t, pool, c2, seedTurn{ordinal: 0, model: "m", inTok: 10000, cacheRead: 500, prefixHash: "shared"})

	s, err := New(pool).GlobalStats(ctx, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.CacheWaste.WastedTurns != 1 {
		t.Errorf("wasted turns = %d, want 1", s.CacheWaste.WastedTurns)
	}
	if s.CacheWaste.WastedInputTokens != 10000 {
		t.Errorf("wasted input tokens = %d, want 10000", s.CacheWaste.WastedInputTokens)
	}
	if s.Prefix.DistinctPrefixes != 1 {
		t.Errorf("distinct prefixes = %d, want 1", s.Prefix.DistinctPrefixes)
	}
	if s.Prefix.TurnsWithPrefix != 2 {
		t.Errorf("turns with prefix = %d, want 2", s.Prefix.TurnsWithPrefix)
	}
	if s.Prefix.CrossUserPrefixes != 0 {
		t.Errorf("cross-user prefixes = %d, want 0 (single owner)", s.Prefix.CrossUserPrefixes)
	}
}

func TestConversationStats_Scoped(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversation(t, pool, "client", "erin") // two turns, in 100+120, cacheRead 0+80
	seedConversation(t, pool, "server", "frank")          // must be excluded

	s, err := New(pool).ConversationStats(ctx, convID)
	if err != nil {
		t.Fatalf("conversation stats: %v", err)
	}
	if s.Volume.Conversations != 1 || s.Volume.Turns != 2 {
		t.Errorf("volume = %d/%d, want 1 conversation / 2 turns", s.Volume.Conversations, s.Volume.Turns)
	}
	if s.Tokens.InputTokens != 220 || s.Tokens.CacheReadTokens != 80 {
		t.Errorf("tokens = in %d / cache %d, want 220/80", s.Tokens.InputTokens, s.Tokens.CacheReadTokens)
	}
	// Both turns share prefix hash-a → no drift, no cross-user facet.
	if s.Prefix.DriftedConversations != 0 {
		t.Errorf("drifted = %d, want 0", s.Prefix.DriftedConversations)
	}
	if s.Prefix.CrossUserPrefixes != 0 {
		t.Errorf("cross-user prefixes = %d, want 0 (not computed for a single conversation)", s.Prefix.CrossUserPrefixes)
	}
}

func inDelta(got, want, delta float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= delta
}

func TestGlobalStats_CostFromPricer(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "grace")
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 0, model: "claude-sonnet-5", upstream: "anthropic",
		inTok: 1000, outTok: 200, cacheRead: 5000, cacheCreate: 300,
	})

	// Fake pricer: prices only claude-sonnet-5, mirroring OR per-token USD.
	pricer := func(model string) (routing.ModelPricing, bool) {
		if model != "claude-sonnet-5" {
			return routing.ModelPricing{}, false
		}
		return routing.ModelPricing{
			PromptUSD: 0.000002, CompletionUSD: 0.00001,
			CacheReadUSD: 0.0000002, CacheWriteUSD: 0.0000025,
		}, true
	}
	ins := New(pool).WithPricer(pricer)

	s, err := ins.GlobalStats(ctx, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(s.Cost) != 1 {
		t.Fatalf("cost rows = %d, want 1", len(s.Cost))
	}
	// 1000*2e-6 + 200*1e-5 + 5000*2e-7 + 300*2.5e-6 = 0.002 + 0.002 + 0.001 + 0.00075 = 0.00575
	want := 1000*0.000002 + 200*0.00001 + 5000*0.0000002 + 300*0.0000025
	if !inDelta(s.Cost[0].CostUSD, want, 1e-9) {
		t.Errorf("cost_usd = %g, want %g", s.Cost[0].CostUSD, want)
	}
}

func TestGlobalStats_UnpricedWithoutPricer(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "heidi")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "claude-sonnet-5", inTok: 1000, outTok: 200})

	// No pricer, and a pricer that never resolves, both leave CostUSD at 0.
	for name, ins := range map[string]*Insights{
		"no-pricer":      New(pool),
		"nil-pricer":     New(pool).WithPricer(nil),
		"unknown-pricer": New(pool).WithPricer(func(string) (routing.ModelPricing, bool) { return routing.ModelPricing{}, false }),
	} {
		s, err := ins.GlobalStats(ctx, StatsFilter{})
		if err != nil {
			t.Fatalf("%s: stats: %v", name, err)
		}
		if len(s.Cost) != 1 || s.Cost[0].CostUSD != 0 {
			t.Errorf("%s: cost_usd = %v, want 0 (unpriced)", name, s.Cost)
		}
	}
}

func TestStats_InvalidPathErrors(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ins := New(pool)

	if _, err := ins.GlobalStats(ctx, StatsFilter{Path: Path("server")}); err == nil {
		t.Error("GlobalStats with an invalid path must error")
	}
	if _, err := ins.Search(ctx, SearchFilter{Path: Path("client")}); err == nil {
		t.Error("Search with an invalid path (raw driven_by value) must error")
	}
	// The valid aliases still work.
	if _, err := ins.GlobalStats(ctx, StatsFilter{Path: PathProxy}); err != nil {
		t.Errorf("GlobalStats(proxy) = %v, want nil", err)
	}
	if _, err := ins.Search(ctx, SearchFilter{Path: PathAny}); err != nil {
		t.Errorf("Search(any) = %v, want nil", err)
	}
}

func TestConversationStats_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	_, err := New(pool).ConversationStats(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ConversationStats on a missing conversation err = %v, want ErrNotFound", err)
	}
}

func TestGlobalStats_NullOwnerAndNullUpstream(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	// A NULL-owner conversation whose turn has no recorded upstream, next to a
	// normal conversation served by openrouter.
	c1 := insertConversation(t, pool, "server", "")
	insertTurn(t, pool, c1, seedTurn{ordinal: 0, model: "m", inTok: 100})
	c2 := insertConversation(t, pool, "client", "zoe")
	insertTurn(t, pool, c2, seedTurn{ordinal: 0, model: "m", upstream: "openrouter", inTok: 100})

	s, err := New(pool).GlobalStats(ctx, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	// The NULL owner must count as one distinct owner, matching its '' per-owner row.
	if s.Adoption.DistinctOwners != 2 {
		t.Errorf("distinct owners = %d, want 2 (NULL owner counts)", s.Adoption.DistinctOwners)
	}
	var sawEmpty bool
	for _, oc := range s.Adoption.PerOwner {
		if oc.Owner == "" {
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Errorf("per-owner rows %+v missing the ''-owner row", s.Adoption.PerOwner)
	}
	// Failover rate is over ALL turns: 1 openrouter turn / 2 total, the
	// NULL-upstream turn stays in the denominator.
	if !inDelta(s.Failures.FailoverRate, 0.5, 1e-9) {
		t.Errorf("failover rate = %v, want 0.5", s.Failures.FailoverRate)
	}
}
