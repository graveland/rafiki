// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/ejection"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestBuildProviderGuardRehydrates exercises the daemon's own assembly of the
// provider cache guard against a real database: an ejection written by one
// process must be in force for the next one that boots. Nothing else covers
// this wiring — the guard, the store and the env gate each have unit tests, but
// only startProxyFace puts them together, and a guard that silently forgot
// every ejection on restart would pass all of those.
func TestBuildProviderGuardRehydrates(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Require().Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.Require().NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.Require().NoError(store.Migrate(ctx, pool), "Migrate")

	// A model line unique to this test, so a real ejection recorded by the
	// daemon can never make this pass or fail for the wrong reason.
	const line = "vendor/wiring-test-model"
	c.Require().NoError(ejection.NewEjectionStore(pool).Append(ctx, routing.EjectionRecord{
		Provider:  "WiringTestProvider",
		ModelLine: line,
		Reason:    routing.ReasonNoCache,
		ExpiresAt: time.Now().Add(time.Hour),
		Evidence:  []byte(`{"streak":5}`),
	}), "seed ejection")

	guard := buildProviderGuard(ctx, pool, slog.New(slog.DiscardHandler))
	c.Require().NotNil(guard, "buildProviderGuard returned nil with the guard enabled")
	got := guard.IgnoredFor(time.Now(), line)
	c.False(len(got) != 1 || got[0] != "wiringtestprovider", "IgnoredFor after rehydrate = %v, want [wiringtestprovider]", got)
}

// TestBuildProviderGuardDisabled proves the off-switch reaches all the way
// through the constructor as "no automatic ejection" — NOT a nil guard, which
// would silently drop operator bans — and that a nil pool still yields a
// working memory-only guard rather than nothing.
func TestBuildProviderGuardDisabled(t *testing.T) {
	t.Setenv(paths.ProviderGuard, "off")
	c := assert.NewCollecting(t)
	g := buildProviderGuard(context.Background(), nil, slog.New(slog.DiscardHandler))
	c.Require().NotNil(g, "guard = nil with RAFIKI_PROVIDER_GUARD=off; operator bans need it")
	now := time.Now()
	for range 20 {
		g.Observe(now, routing.Observation{Provider: "CoreWeave", Model: "vendor/m", Conversation: "c",
			PrefixHash: "h", InputTokens: 50000})
	}
	_, err := g.Ban(context.Background(), now, "banned", 0, "")
	c.Require().NoError(err)
	if got := g.IgnoredFor(now, "vendor/m"); len(got) != 1 || got[0] != "banned" {
		t.Errorf("IgnoredFor = %v with the guard off, want the ban only", got)
	}
	t.Setenv(paths.ProviderGuard, "")
	c.NotNil(buildProviderGuard(context.Background(), nil, slog.New(slog.DiscardHandler)), "guard = nil with no pool; want a working memory-only guard")
}
