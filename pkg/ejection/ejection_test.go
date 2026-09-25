// SPDX-License-Identifier: Apache-2.0

package ejection

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// TestEjectionStoreRoundTrip proves an appended ejection comes back from
// Active while unexpired, and does not once it has expired — the two behaviours
// the startup rehydrate depends on. Requires RAFIKI_TEST_DSN, like every other
// DB test in this package.
func TestEjectionStoreRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	now := time.Now()

	// A provider distinct enough that a parallel run can't collide with it.
	provider := "TestProvider-" + t.Name()
	rec := routing.EjectionRecord{
		Provider:  provider,
		ModelLine: "vendor/test-model",
		Reason:    routing.ReasonNoCache,
		ExpiresAt: now.Add(time.Hour),
		Evidence:  []byte(`{"streak":5}`),
	}
	if err := s.Append(ctx, rec); err != nil {
		t.Fatalf("Append: %v", err)
	}

	active, err := s.Active(ctx, now)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if !containsProvider(active, provider) {
		t.Errorf("Active at now omitted the unexpired ejection for %s", provider)
	}

	future, err := s.Active(ctx, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Active (future): %v", err)
	}
	if containsProvider(future, provider) {
		t.Errorf("Active after expiry still returned %s", provider)
	}
}

// TestEjectionStoreLiftSupersedesBan is the resurrection regression: a lift
// row is itself already expired, so filtering on expiry before picking the
// latest row per key would skip it and hand the lifted ban back to the
// startup rehydrate.
func TestEjectionStoreLiftSupersedesBan(t *testing.T) {
	s, ctx := testStore(t)
	now := time.Now()
	provider := "testprovider-" + t.Name()
	ban := routing.EjectionRecord{Provider: provider, ModelLine: routing.AllModelLines,
		Reason: routing.ReasonOperator, Note: "spinning"}
	if err := s.Append(ctx, ban); err != nil {
		t.Fatalf("Append ban: %v", err)
	}
	active, err := s.Active(ctx, now.Add(100*365*24*time.Hour))
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	got, ok := findProvider(active, provider)
	if !ok {
		t.Fatal("an unexpiring ban was not active a century later")
	}
	if !got.ExpiresAt.IsZero() || got.Note != "spinning" || got.Reason != routing.ReasonOperator {
		t.Errorf("ban round-tripped as %+v", got)
	}

	lift := routing.EjectionRecord{Provider: provider, ModelLine: routing.AllModelLines,
		Reason: routing.ReasonLift, ExpiresAt: now}
	if err := s.Append(ctx, lift); err != nil {
		t.Fatalf("Append lift: %v", err)
	}
	active, err = s.Active(ctx, now)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if _, ok := findProvider(active, provider); ok {
		t.Error("the lifted ban came back from Active")
	}
}

func testStore(t *testing.T) (*EjectionStore, context.Context) {
	t.Helper()
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
	return NewEjectionStore(pool), ctx
}

func findProvider(recs []routing.EjectionRecord, provider string) (routing.EjectionRecord, bool) {
	for _, r := range recs {
		if r.Provider == provider {
			return r, true
		}
	}
	return routing.EjectionRecord{}, false
}

func containsProvider(recs []routing.EjectionRecord, provider string) bool {
	for _, r := range recs {
		if r.Provider == provider {
			return true
		}
	}
	return false
}
