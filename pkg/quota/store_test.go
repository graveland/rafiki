// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"
)

func quotaTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newQuotaTestUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ('quota-test-'||gen_random_uuid()::text, gen_random_uuid()::text)
		 RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func TestStoreGetOnUncapturedUserIsNotFoundNotError(t *testing.T) {
	pool := quotaTestPool(t)
	s := NewStore(pool)
	userID := newQuotaTestUser(t, pool)

	_, ok, err := s.Get(context.Background(), userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get reported ok=true for a user with no captured snapshot")
	}
}

func TestStoreUpsertThenGetRoundTrips(t *testing.T) {
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
	if err := s.Upsert(ctx, userID, in); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := s.Get(ctx, userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get reported ok=false right after Upsert")
	}
	if got.OrganizationID != in.OrganizationID {
		t.Errorf("OrganizationID = %q, want %q", got.OrganizationID, in.OrganizationID)
	}
	if got.FiveH.Utilization == nil || *got.FiveH.Utilization != util5 {
		t.Errorf("FiveH.Utilization = %v, want %v", got.FiveH.Utilization, util5)
	}
	if got.FiveH.ResetAt == nil || !got.FiveH.ResetAt.Equal(reset5) {
		t.Errorf("FiveH.ResetAt = %v, want %v", got.FiveH.ResetAt, reset5)
	}
	if got.SevenD.Utilization != nil {
		t.Errorf("SevenD.Utilization = %v, want nil (never reported)", got.SevenD.Utilization)
	}
	if got.OverallStatus != in.OverallStatus {
		t.Errorf("OverallStatus = %q, want %q", got.OverallStatus, in.OverallStatus)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero after Upsert")
	}

	// A second Upsert overwrites in place -- latest-only, not a history.
	util5b := 0.55
	if err := s.Upsert(ctx, userID, Status{FiveH: Window{Utilization: &util5b, Status: "allowed"}, OverallStatus: "allowed"}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	got2, ok, err := s.Get(ctx, userID)
	if err != nil || !ok {
		t.Fatalf("Get after second Upsert: ok=%v err=%v", ok, err)
	}
	if got2.FiveH.Utilization == nil || *got2.FiveH.Utilization != util5b {
		t.Errorf("FiveH.Utilization after second Upsert = %v, want %v", got2.FiveH.Utilization, util5b)
	}
	if got2.OrganizationID != "" {
		t.Errorf("OrganizationID after second Upsert = %q, want empty (overwritten with unset)", got2.OrganizationID)
	}
}

func TestStoreNilIsSafeNoOp(t *testing.T) {
	var s *Store
	if err := s.Upsert(context.Background(), "whatever", Status{}); err != nil {
		t.Errorf("nil Store.Upsert returned an error: %v", err)
	}
	_, ok, err := s.Get(context.Background(), "whatever")
	if err != nil || ok {
		t.Errorf("nil Store.Get = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}
