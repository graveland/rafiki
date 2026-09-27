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

	"github.com/multigres/testkit/assert"
)

// TestEjectionStoreRoundTrip proves an appended ejection comes back from
// Active while unexpired, and does not once it has expired — the two behaviours
// the startup rehydrate depends on. Requires RAFIKI_TEST_DSN, like every other
// DB test in this package.
func TestEjectionStoreRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(s.Append(ctx, rec), "Append")

	active, err := s.Active(ctx, now)
	c.Require().NoError(err, "Active")
	c.True(containsProvider(active, provider), "Active at now omitted the unexpired ejection for %s", provider)

	future, err := s.Active(ctx, now.Add(2*time.Hour))
	c.Require().NoError(err, "Active (future)")
	c.False(containsProvider(future, provider), "Active after expiry still returned %s", provider)
}

// TestEjectionStoreLiftSupersedesBan is the resurrection regression: a lift
// row is itself already expired, so filtering on expiry before picking the
// latest row per key would skip it and hand the lifted ban back to the
// startup rehydrate.
func TestEjectionStoreLiftSupersedesBan(t *testing.T) {
	c := assert.NewCollecting(t)
	s, ctx := testStore(t)
	now := time.Now()
	provider := "testprovider-" + t.Name()
	ban := routing.EjectionRecord{Provider: provider, ModelLine: routing.AllModelLines,
		Reason: routing.ReasonOperator, Note: "spinning"}
	c.Require().NoError(s.Append(ctx, ban), "Append ban")
	active, err := s.Active(ctx, now.Add(100*365*24*time.Hour))
	c.Require().NoError(err, "Active")
	got, ok := findProvider(active, provider)
	c.Require().True(ok, "an unexpiring ban was not active a century later")
	c.False(!got.ExpiresAt.IsZero() || got.Note != "spinning" || got.Reason != routing.ReasonOperator, "ban round-tripped as %+v", got)

	lift := routing.EjectionRecord{Provider: provider, ModelLine: routing.AllModelLines,
		Reason: routing.ReasonLift, ExpiresAt: now}
	c.Require().NoError(s.Append(ctx, lift), "Append lift")
	active, err = s.Active(ctx, now)
	c.Require().NoError(err, "Active")
	if _, ok := findProvider(active, provider); ok {
		t.Error("the lifted ban came back from Active")
	}
}

func testStore(t *testing.T) (*EjectionStore, context.Context) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
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
