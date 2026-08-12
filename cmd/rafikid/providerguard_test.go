// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// TestBuildProviderGuardRehydrates exercises the daemon's own assembly of the
// provider cache guard against a real database: an ejection written by one
// process must be in force for the next one that boots. Nothing else covers
// this wiring — the guard, the store and the env gate each have unit tests, but
// only startProxyFace puts them together, and a guard that silently forgot
// every ejection on restart would pass all of those.
func TestBuildProviderGuardRehydrates(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A model line unique to this test, so a real ejection recorded by the
	// daemon can never make this pass or fail for the wrong reason.
	const line = "vendor/wiring-test-model"
	if err := routing.NewEjectionStore(pool).Append(ctx, routing.EjectionRecord{
		Provider:  "WiringTestProvider",
		ModelLine: line,
		Reason:    routing.ReasonNoCache,
		ExpiresAt: time.Now().Add(time.Hour),
		Evidence:  []byte(`{"streak":5}`),
	}); err != nil {
		t.Fatalf("seed ejection: %v", err)
	}

	guard := buildProviderGuard(ctx, pool, slog.New(slog.DiscardHandler))
	if guard == nil {
		t.Fatal("buildProviderGuard returned nil with the guard enabled")
	}
	got := guard.IgnoredFor(time.Now(), line)
	if len(got) != 1 || got[0] != "wiringtestprovider" {
		t.Errorf("IgnoredFor after rehydrate = %v, want [wiringtestprovider]", got)
	}
}

// TestBuildProviderGuardDisabled proves the off-switch reaches all the way
// through the constructor, and that a nil pool still yields a working
// memory-only guard rather than nothing.
func TestBuildProviderGuardDisabled(t *testing.T) {
	t.Setenv(paths.ProviderGuard, "off")
	if g := buildProviderGuard(context.Background(), nil, slog.New(slog.DiscardHandler)); g != nil {
		t.Errorf("guard = %v with RAFIKI_PROVIDER_GUARD=off, want nil", g)
	}
	t.Setenv(paths.ProviderGuard, "")
	if g := buildProviderGuard(context.Background(), nil, slog.New(slog.DiscardHandler)); g == nil {
		t.Error("guard = nil with no pool; want a working memory-only guard")
	}
}
