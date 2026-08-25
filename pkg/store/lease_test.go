package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func leasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

func TestAcquireAndRenew(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !ok {
		t.Fatal("Acquire on a free conversation returned ok=false")
	}
	if lease.Token == "" {
		t.Error("Acquire returned an empty token")
	}

	renewed, err := ls.Renew(ctx, lease, 5*time.Minute)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !renewed {
		t.Error("Renew on a held lease returned false")
	}
}

// TestSecondHolderIsRefused is the core of the design: a live lease excludes a
// different daemon. Without this the shared child table lets two daemons resume
// the same child.
func TestSecondHolderIsRefused(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	if _, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute); err != nil || !ok {
		t.Fatalf("first Acquire: ok=%v err=%v", ok, err)
	}
	_, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if ok {
		t.Error("daemon-b acquired a lease daemon-a holds")
	}
}

// TestSameHolderReclaimsInstantly pins the OR holder = EXCLUDED.holder clause.
// It is what lets a restarted daemon reclaim its own leases without waiting out
// the TTL, and it is the reason the TTL can be long.
func TestSameHolderReclaimsInstantly(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	first, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("first Acquire: ok=%v err=%v", ok, err)
	}
	second, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if !ok {
		t.Fatal("a daemon could not reclaim its own lease")
	}
	if second.Token == first.Token {
		t.Error("reclaim reused the old token; each acquisition must mint a fresh one")
	}
	// The old token must now be dead.
	valid, err := ls.Valid(ctx, first)
	if err != nil {
		t.Fatalf("Valid: %v", err)
	}
	if valid {
		t.Error("the superseded token still validates")
	}
}

// TestExpiredLeaseIsTakeable proves the TTL actually gates takeover.
func TestExpiredLeaseIsTakeable(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	// A negative TTL writes an already-expired lease, which is a deterministic
	// barrier where a sleep would be a flake.
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute); err != nil || !ok {
		t.Fatalf("first Acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("daemon-b could not take an expired lease: ok=%v err=%v", ok, err)
	}
}

func TestRenewAfterTakeoverFails(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	if err != nil || !ok {
		t.Fatalf("first Acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}
	renewed, err := ls.Renew(ctx, stale, 5*time.Minute)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if renewed {
		t.Error("a superseded holder renewed its lease")
	}
}

func TestRelease(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	if err := ls.Release(ctx, lease); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Errorf("after Release, daemon-b could not acquire: ok=%v err=%v", ok, err)
	}
}
