// SPDX-License-Identifier: Apache-2.0

package eventlogdb_test

import (
	"context"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"go.graveland.dev/rafiki/pkg/eventlog"
	"go.graveland.dev/rafiki/pkg/eventlog/eventlogtest"
	"go.graveland.dev/rafiki/pkg/eventlogdb"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
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

// TestAppendManyConcurrentPublishersOneChild is the scale the two tests above
// structurally cannot reach: the memory store is atomic under its own mutex,
// and TestConcurrentAppendDoesNotDuplicateAnOrdinal fires one burst, not a
// sustained storm. Burst after burst of publishers for ONE child is what the
// daemon actually does (controller publishes for every child event), and
// before per-child serialization the losers retried immediately against the
// rest of the storm, exhausted maxAppendAttempts, and DROPPED the event.
func TestAppendManyConcurrentPublishersOneChild(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	child := "c_" + ulid.Make().String()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.event_log WHERE child_id = $1`, child) })

	const publishers = 64
	const perPublisher = 8
	const total = publishers * perPublisher

	s := eventlogdb.New(pool)
	ords := make([][]int32, publishers)
	errs := make([]error, publishers)
	var wg sync.WaitGroup
	for i := range publishers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for range perPublisher {
				ord, err := s.Append(ctx, child, statusEvent(child, "streaming"))
				if err != nil {
					errs[idx] = err
					return
				}
				ords[idx] = append(ords[idx], ord)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		c.NoError(err, "publisher %d", i)
	}

	got := make([]int32, 0, total)
	for _, o := range ords {
		got = append(got, o...)
	}
	c.Len(got, total, "collected %d ordinals, want", len(got))
	sort.Slice(got, func(a, b int) bool { return got[a] < got[b] })
	for want := range int32(total) {
		c.Eq(want, got[want], "sorted ordinal[")
	}

	var count int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.event_log WHERE child_id = $1`, child).Scan(&count), "count")
	c.Eq(total, count, "row count = %d, want %d — %d events were dropped", count, total, total-count)
}

// TestConcurrentAppendDoesNotDuplicateAnOrdinal is the test the shared
// conformance suite structurally cannot be: the memory store is atomic under
// its own mutex, so it passes this trivially and proves nothing about
// Postgres. Two explicit transactions make the race deterministic where a
// sleep would not.
func TestConcurrentAppendDoesNotDuplicateAnOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
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
		c.NoError(errs[i], "append %d", i)
		if seen[ords[i]] {
			t.Fatalf("ordinal %d issued twice", ords[i])
		}
		seen[ords[i]] = true
	}

	// Gap-free: seeded 0 plus n more means 0..n with nothing missing.
	var count int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.event_log WHERE child_id = $1`, child).Scan(&count), "count")
	c.Eq(n+1, count, "row count")
	for want := range int32(n + 1) {
		c.False(!seen[want] && want != 0, "ordinal %d missing; the sequence has a gap", want)
	}
}
