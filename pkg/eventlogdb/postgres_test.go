// SPDX-License-Identifier: Apache-2.0

package eventlogdb_test

import (
	"context"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// insertRawEvent writes one row straight into conversations.event_log with a
// payload the caller controls verbatim — the only way to reproduce a row an
// older writer persisted with field names this build no longer knows.
func insertRawEvent(t *testing.T, pool *pgxpool.Pool, child, typeName, payload string, createdAt time.Time) {
	t.Helper()
	c := assert.NewAborting(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.event_log (child_id, ordinal, type, payload, created_at)
		 VALUES ($1, COALESCE((SELECT MAX(ordinal) + 1 FROM conversations.event_log WHERE child_id = $1), 0), $2, $3::jsonb, $4)`,
		child, typeName, payload, createdAt)
	c.NoError(err, "insert raw event")
}

// TestReadOldPayloadWithRemovedFieldsStillDecodes is the stored-history
// contract: a row written before ts/resume_at/duration existed carries the old
// field names (tsUnixMs, durationMs), and the read path must still decode it —
// dropping the unknown fields rather than failing — and fall back to the row's
// created_at for the missing ts. It FAILS against a reader without
// DiscardUnknown (see the report's pre-change evidence).
func TestReadOldPayloadWithRemovedFieldsStillDecodes(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	child := "c_" + ulid.Make().String()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.event_log WHERE child_id = $1`, child) })

	createdAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	insertRawEvent(t, pool, child, "tool_execution_end",
		`{"childId":"`+child+`","tsUnixMs":"1700000000000","toolExecutionEnd":{"toolUseId":"tu_1","durationMs":"12","isError":true}}`,
		createdAt)

	s := eventlogdb.New(pool)
	recs, err := s.Read(ctx, child, -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 1, "rows = %d, want 1", len(recs))

	ev, err := recs[0].Decode()
	c.NoError(err, "an old payload with removed fields must decode")
	c.Require().NotNil(ev.GetTs(), "ts must fall back to the row's created_at")
	c.True(ev.GetTs().AsTime().Equal(createdAt), "ts = %v, want the row's created_at %v", ev.GetTs().AsTime(), createdAt)

	te := ev.GetToolExecutionEnd()
	c.Require().NotNil(te, "payload type")
	c.Eq("tu_1", te.GetToolUseId(), "tool_use_id")
	c.True(te.GetIsError(), "is_error")
	c.Nil(te.GetDuration(), "the removed durationMs must not reappear as duration")
}

// TestDecodeDoesNotSwallowAMalformedNewPayload proves DiscardUnknown drops
// unknown FIELDS only: a known field carrying the wrong type still fails, so a
// genuinely malformed NEW payload is never decoded silently.
func TestDecodeDoesNotSwallowAMalformedNewPayload(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	child := "c_" + ulid.Make().String()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.event_log WHERE child_id = $1`, child) })

	// ts is a Timestamp message: a bare number is a known field with the wrong
	// type, not an unknown field.
	insertRawEvent(t, pool, child, "agent_status",
		`{"childId":"`+child+`","ts":123,"agentStatus":{"state":"idle"}}`,
		time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))

	s := eventlogdb.New(pool)
	recs, err := s.Read(ctx, child, -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 1, "rows = %d, want 1", len(recs))

	_, err = recs[0].Decode()
	c.Error(err, "a wrong-typed ts must fail rather than decode silently")
}

// TestNewEventRoundTripsTsAndDuration: a fresh event's Timestamp and Duration
// survive append + read + decode.
func TestNewEventRoundTripsTsAndDuration(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	child := "c_" + ulid.Make().String()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.event_log WHERE child_id = $1`, child) })

	ts := time.Date(2025, 5, 6, 7, 8, 9, 123000000, time.UTC)
	want := 1500 * time.Millisecond
	s := eventlogdb.New(pool)
	if _, err := s.Append(ctx, child, &rafikiv1.Event{
		ChildId: child,
		Ts:      timestamppb.New(ts),
		Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{
			ToolUseId: "tu_1", Duration: durationpb.New(want), IsError: true,
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	recs, err := s.Read(ctx, child, -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 1, "rows = %d, want 1", len(recs))
	ev, err := recs[0].Decode()
	c.NoError(err, "Decode")
	c.True(ev.GetTs().AsTime().Equal(ts), "ts = %v, want %v", ev.GetTs().AsTime(), ts)
	c.Eq(want, ev.GetToolExecutionEnd().GetDuration().AsDuration(), "duration")
}

// TestRetryResumeAtRoundTrips: a Retry's resume_at survives append + read +
// decode, and an unset resume_at stays nil (a resolution event names no
// schedule).
func TestRetryResumeAtRoundTrips(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	child := "c_" + ulid.Make().String()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM conversations.event_log WHERE child_id = $1`, child) })

	resumeAt := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	s := eventlogdb.New(pool)
	if _, err := s.Append(ctx, child, &rafikiv1.Event{
		ChildId: child,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt: 1, WillRetry: true, Reason: "rate limited",
			ResumeAt: timestamppb.New(resumeAt), MaxAttempts: 3,
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s.Append(ctx, child, &rafikiv1.Event{
		ChildId: child,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{Attempt: 1, Reason: "fired"}},
	}); err != nil {
		t.Fatalf("Append resolution: %v", err)
	}

	recs, err := s.Read(ctx, child, -1, 0)
	c.NoError(err, "Read")
	c.Require().Len(recs, 2, "rows = %d, want 2", len(recs))

	scheduled, err := recs[0].Decode()
	c.NoError(err, "Decode scheduled")
	c.True(scheduled.GetRetry().GetResumeAt().AsTime().Equal(resumeAt),
		"resume_at = %v, want %v", scheduled.GetRetry().GetResumeAt().AsTime(), resumeAt)

	resolved, err := recs[1].Decode()
	c.NoError(err, "Decode resolution")
	c.Nil(resolved.GetRetry().GetResumeAt(), "a resolution event must carry no resume_at")
}
