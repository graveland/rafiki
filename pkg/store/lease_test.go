package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
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

// TestFencedAppendSucceedsWithLiveLease is the baseline for the next test:
// with a valid lease the guard is invisible.
func TestFencedAppendSucceedsWithLiveLease(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}

	msgs := NewMessages(pool).WithLease(lease)
	if err := msgs.Append(ctx, conv, 0, userMessage("hello"), nil); err != nil {
		t.Fatalf("Append with a live lease: %v", err)
	}

	loaded, err := NewMessages(pool).Load(ctx, conv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d messages, want 1", len(loaded))
	}
}

// TestFencedAppendFailsAfterTakeover is the fencing test. A holder that stalled
// past expiry and woke up after another daemon took over must write NOTHING —
// this is what makes a TTL lease safe without a monotonic fencing token.
func TestFencedAppendFailsAfterTakeover(t *testing.T) {
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

	msgs := NewMessages(pool).WithLease(stale)
	err = msgs.Append(ctx, conv, 0, userMessage("should not land"), nil)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Append error = %v, want ErrLeaseLost", err)
	}

	loaded, lerr := NewMessages(pool).Load(ctx, conv)
	if lerr != nil {
		t.Fatalf("Load: %v", lerr)
	}
	if len(loaded) != 0 {
		t.Errorf("a superseded holder wrote %d messages; want 0", len(loaded))
	}
}

// TestUnfencedAppendStillWorks pins the escape hatch: a caller with no lease
// (the proxy path, a client-driven conversation) writes exactly as before.
func TestUnfencedAppendStillWorks(t *testing.T) {
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	if err := NewMessages(pool).Append(ctx, conv, 0, userMessage("unfenced"), nil); err != nil {
		t.Fatalf("unfenced Append: %v", err)
	}
}

// TestFencedAppendConflictIsNotLeaseLost proves the zero-rows path still tells
// an ordinal conflict (a Resume replay) apart from a lost lease. Collapsing the
// two would make every resume look like a takeover.
func TestFencedAppendConflictIsNotLeaseLost(t *testing.T) {
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	msgs := NewMessages(pool).WithLease(lease)

	if err := msgs.Append(ctx, conv, 0, userMessage("same"), nil); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	// Re-appending identical content at the same ordinal is a replay, not a
	// takeover, and must succeed.
	if err := msgs.Append(ctx, conv, 0, userMessage("same"), nil); err != nil {
		t.Errorf("replay Append: %v, want nil", err)
	}
	// Different content at the same ordinal is a diverged history.
	err = msgs.Append(ctx, conv, 0, userMessage("different"), nil)
	if err == nil {
		t.Error("diverging content at an existing ordinal was accepted")
	}
	if errors.Is(err, ErrLeaseLost) {
		t.Error("a content divergence was reported as a lost lease")
	}
}

func userMessage(text string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)},
	}
}
