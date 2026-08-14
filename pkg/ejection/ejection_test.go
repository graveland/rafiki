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

	s := NewEjectionStore(pool)
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

func containsProvider(recs []routing.EjectionRecord, provider string) bool {
	for _, r := range recs {
		if r.Provider == provider {
			return true
		}
	}
	return false
}
