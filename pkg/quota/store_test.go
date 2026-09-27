// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func quotaTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pool")
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
	t.Cleanup(pool.Close)
	return pool
}

func newQuotaTestUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ('quota-test-'||gen_random_uuid()::text, gen_random_uuid()::text)
		 RETURNING id::text`).Scan(&id), "insert user")
	return id
}

func TestStoreGetOnUncapturedUserIsNotFoundNotError(t *testing.T) {
	c := assert.NewAborting(t)
	pool := quotaTestPool(t)
	s := NewStore(pool)
	userID := newQuotaTestUser(t, pool)

	_, ok, err := s.Get(context.Background(), userID)
	c.NoError(err, "Get")
	c.False(ok, "Get reported ok=true for a user with no captured snapshot")
}

func TestStoreUpsertThenGetRoundTrips(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := quotaTestPool(t)
	s := NewStore(pool)
	userID := newQuotaTestUser(t, pool)
	ctx := context.Background()

	util5 := 0.42
	reset5 := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	in := Status{
		OrganizationID: "org_123",
		FiveH:          Window{Utilization: &util5, ResetAt: &reset5, Status: "allowed"},
		SevenD:         Window{Status: "allowed_warning"}, // no utilization/reset reported
		OverallStatus:  "allowed_warning",
	}
	c.Require().NoError(s.Upsert(ctx, userID, in), "Upsert")

	got, ok, err := s.Get(ctx, userID)
	c.Require().NoError(err, "Get")
	c.Require().True(ok, "Get reported ok=false right after Upsert")
	c.Eq(in.OrganizationID, got.OrganizationID, "OrganizationID")
	c.False(got.FiveH.Utilization == nil || *got.FiveH.Utilization != util5, "FiveH.Utilization = %v, want %v", got.FiveH.Utilization, util5)
	c.False(got.FiveH.ResetAt == nil || !got.FiveH.ResetAt.Equal(reset5), "FiveH.ResetAt = %v, want %v", got.FiveH.ResetAt, reset5)
	c.Nil(got.SevenD.Utilization, "SevenD.Utilization")
	c.Eq(in.OverallStatus, got.OverallStatus, "OverallStatus")
	c.False(got.UpdatedAt.IsZero(), "UpdatedAt is zero after Upsert")

	// A second Upsert overwrites in place -- latest-only, not a history.
	util5b := 0.55
	c.Require().NoError(s.Upsert(ctx, userID, Status{FiveH: Window{Utilization: &util5b, Status: "allowed"}, OverallStatus: "allowed"}), "second Upsert")
	got2, ok, err := s.Get(ctx, userID)
	c.Require().False(err != nil || !ok, "Get after second Upsert: ok=%v err=%v", ok, err)
	c.False(got2.FiveH.Utilization == nil || *got2.FiveH.Utilization != util5b, "FiveH.Utilization after second Upsert = %v, want %v", got2.FiveH.Utilization, util5b)
	c.Eq("", got2.OrganizationID, "OrganizationID after second Upsert")
}

func TestStoreNilIsSafeNoOp(t *testing.T) {
	c := assert.NewCollecting(t)
	var s *Store
	c.NoError(s.Upsert(context.Background(), "whatever", Status{}), "nil Store.Upsert returned an error")
	_, ok, err := s.Get(context.Background(), "whatever")
	c.False(err != nil || ok, "nil Store.Get = ok=%v err=%v, want ok=false err=nil", ok, err)
}
