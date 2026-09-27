package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

func leasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pool")
	c.NoError(Migrate(context.Background(), pool), "migrate")
	t.Cleanup(pool.Close)
	return pool
}

func newConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation")
	return id
}

func TestAcquireAndRenew(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.Require().NoError(err, "Acquire")
	c.Require().True(ok, "Acquire on a free conversation returned ok=false")
	c.NotEq("", lease.Token, "Acquire returned an empty token")

	renewed, err := ls.Renew(ctx, lease, 5*time.Minute)
	c.Require().NoError(err, "Renew")
	c.True(renewed, "Renew on a held lease returned false")
}

// TestSecondHolderIsRefused is the core of the design: a live lease excludes a
// different daemon. Without this the shared child table lets two daemons resume
// the same child.
func TestSecondHolderIsRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	if _, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute); err != nil || !ok {
		t.Fatalf("first Acquire: ok=%v err=%v", ok, err)
	}
	_, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute)
	c.Require().NoError(err, "second Acquire")
	c.False(ok, "daemon-b acquired a lease daemon-a holds")
}

// TestSameHolderReclaimsInstantly pins the OR holder = EXCLUDED.holder clause.
// It is what lets a restarted daemon reclaim its own leases without waiting out
// the TTL, and it is the reason the TTL can be long.
func TestSameHolderReclaimsInstantly(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	first, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.Require().False(err != nil || !ok, "first Acquire: ok=%v err=%v", ok, err)
	second, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.Require().NoError(err, "second Acquire")
	c.Require().True(ok, "a daemon could not reclaim its own lease")
	c.NotEq(first.Token, second.Token, "reclaim reused the old token; each acquisition must mint a fresh one")
	// The old token must now be dead.
	valid, err := ls.Valid(ctx, first)
	c.Require().NoError(err, "Valid")
	c.False(valid, "the superseded token still validates")
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
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	c.Require().False(err != nil || !ok, "first Acquire: ok=%v err=%v", ok, err)
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}
	renewed, err := ls.Renew(ctx, stale, 5*time.Minute)
	c.Require().NoError(err, "Renew")
	c.False(renewed, "a superseded holder renewed its lease")
}

func TestRelease(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.False(err != nil || !ok, "Acquire: ok=%v err=%v", ok, err)
	c.NoError(ls.Release(ctx, lease), "Release")
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Errorf("after Release, daemon-b could not acquire: ok=%v err=%v", ok, err)
	}
}

// TestFencedAppendSucceedsWithLiveLease is the baseline for the next test:
// with a valid lease the guard is invisible.
func TestFencedAppendSucceedsWithLiveLease(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.False(err != nil || !ok, "Acquire: ok=%v err=%v", ok, err)

	msgs := NewMessages(pool).WithLease(lease)
	c.NoError(msgs.Append(ctx, conv, 0, userMessage("hello"), nil), "Append with a live lease")

	loaded, err := NewMessages(pool).Load(ctx, conv)
	c.NoError(err, "Load")
	c.Len(loaded, 1, "loaded %d messages, want 1", len(loaded))
}

// TestFencedAppendFailsAfterTakeover is the fencing test. A holder that stalled
// past expiry and woke up after another daemon took over must write NOTHING —
// this is what makes a TTL lease safe without a monotonic fencing token.
func TestFencedAppendFailsAfterTakeover(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	c.Require().False(err != nil || !ok, "first Acquire: ok=%v err=%v", ok, err)
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}

	msgs := NewMessages(pool).WithLease(stale)
	err = msgs.Append(ctx, conv, 0, userMessage("should not land"), nil)
	c.Require().ErrorIs(err, ErrLeaseLost, "Append error")

	loaded, lerr := NewMessages(pool).Load(ctx, conv)
	c.Require().NoError(lerr, "Load")
	c.Empty(loaded, "a superseded holder wrote %d messages; want 0", len(loaded))
}

// TestUnfencedAppendStillWorks pins the escape hatch: a caller with no lease
// (the proxy path, a client-driven conversation) writes exactly as before.
func TestUnfencedAppendStillWorks(t *testing.T) {
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	assert.NewAborting(t).NoError(NewMessages(pool).Append(ctx, conv, 0, userMessage("unfenced"), nil), "unfenced Append")
}

// TestFencedAppendConflictIsNotLeaseLost proves the zero-rows path still tells
// an ordinal conflict (a Resume replay) apart from a lost lease. Collapsing the
// two would make every resume look like a takeover.
func TestFencedAppendConflictIsNotLeaseLost(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := ls.Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.Require().False(err != nil || !ok, "Acquire: ok=%v err=%v", ok, err)
	msgs := NewMessages(pool).WithLease(lease)

	c.Require().NoError(msgs.Append(ctx, conv, 0, userMessage("same"), nil), "first Append")
	// Re-appending identical content at the same ordinal is a replay, not a
	// takeover, and must succeed.
	c.NoError(msgs.Append(ctx, conv, 0, userMessage("same"), nil), "replay Append")
	// Different content at the same ordinal is a diverged history.
	err = msgs.Append(ctx, conv, 0, userMessage("different"), nil)
	c.Error(err, "diverging content at an existing ordinal was accepted")
	c.False(errors.Is(err, ErrLeaseLost), "a content divergence was reported as a lost lease")
}

func userMessage(text string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)},
	}
}

// TestLiveConversations pins the read recovery scoping depends on: which
// conversations are under an UNEXPIRED lease, whoever holds them. A lease that
// has lapsed must not appear — that is what lets a peer daemon adopt an
// abandoned child instead of leaving it stranded forever.
func TestLiveConversations(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := leasePool(t)
	ls := NewLeases(pool)
	ctx := context.Background()

	live := newConversation(t, pool)
	lapsed := newConversation(t, pool)
	unleased := newConversation(t, pool)

	if _, ok, err := ls.Acquire(ctx, live, "daemon-a", 5*time.Minute); err != nil || !ok {
		t.Fatalf("acquire live: ok=%v err=%v", ok, err)
	}
	// A negative TTL writes an already-expired lease, which is the boundary
	// this function exists to get right. A sleep would make the test slow and
	// flaky for no extra coverage.
	if _, ok, err := ls.Acquire(ctx, lapsed, "daemon-b", -1*time.Second); err != nil || !ok {
		t.Fatalf("acquire lapsed: ok=%v err=%v", ok, err)
	}

	got, err := ls.LiveConversations(ctx)
	c.Require().NoError(err, "LiveConversations")
	c.False(!got[live], "live conversation %s missing from the live set", live)
	c.False(got[lapsed], "expired lease on %s must not count as live", lapsed)
	c.False(got[unleased], "never-leased conversation %s must not count as live", unleased)
}
