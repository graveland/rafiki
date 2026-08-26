// SPDX-License-Identifier: Apache-2.0

package eventlogdb_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"go.graveland.dev/rafiki/pkg/eventlog"
	"go.graveland.dev/rafiki/pkg/eventlog/eventlogtest"
	"go.graveland.dev/rafiki/pkg/eventlogdb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func statusEvent(childID, state string) *rafikiv1.Event {
	return &rafikiv1.Event{
		ChildId: childID,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}},
	}
}

func TestPostgresConformance(t *testing.T) {
	pool := testPool(t)
	eventlogtest.RunConformance(t, func(t *testing.T) (eventlog.Store, string) {
		child := "c_" + ulid.Make().String()
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM conversations.event_log WHERE child_id LIKE $1`, child+"%")
		})
		return eventlogdb.New(pool), child
	})
}

// TestConcurrentAppendDoesNotDuplicateAnOrdinal is the test the shared
// conformance suite structurally cannot be: the memory store is atomic under
// its own mutex, so it passes this trivially and proves nothing about
// Postgres. Two explicit transactions make the race deterministic where a
// sleep would not.
func TestConcurrentAppendDoesNotDuplicateAnOrdinal(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	child := "c_" + ulid.Make().String()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.event_log WHERE child_id = $1`, child) })

	s := eventlogdb.New(pool)
	if _, err := s.Append(ctx, child, statusEvent(child, "idle")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	ords := make([]int32, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ords[idx], errs[idx] = s.Append(ctx, child, statusEvent(child, "streaming"))
		}(i)
	}
	wg.Wait()

	seen := map[int32]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("append %d: %v", i, errs[i])
		}
		if seen[ords[i]] {
			t.Fatalf("ordinal %d issued twice", ords[i])
		}
		seen[ords[i]] = true
	}

	// Gap-free: seeded 0 plus n more means 0..n with nothing missing.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.event_log WHERE child_id = $1`, child).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n+1 {
		t.Fatalf("row count = %d, want %d", count, n+1)
	}
	for want := range int32(n + 1) {
		if !seen[want] && want != 0 {
			t.Fatalf("ordinal %d missing; the sequence has a gap", want)
		}
	}
}
