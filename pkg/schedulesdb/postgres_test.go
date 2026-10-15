// SPDX-License-Identifier: Apache-2.0

package schedulesdb_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/schedules"
	"go.graveland.dev/rafiki/pkg/schedules/schedulestest"
	"go.graveland.dev/rafiki/pkg/schedulesdb"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// testPool mirrors pkg/inboxdb/postgres_test.go: skip without RAFIKI_TEST_DSN,
// then migrate the shared test database. The whole suite shares it and it does
// not reset between runs, so every row a test writes is namespaced by a
// per-test prefix and deleted by that test's cleanup.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pgxpool.New")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
	return pool
}

// testPrefix is a child-id/job-id prefix unique to this test and this run, so
// a shared database's residue can never collide with a fresh run.
func testPrefix(t *testing.T) string {
	t.Helper()
	id, err := schedules.NewID()
	assert.NewAborting(t).NoError(err, "NewID")
	return "c_" + t.Name() + "_" + id + "_"
}

func TestPostgresConformance(t *testing.T) {
	pool := testPool(t)
	schedulestest.RunConformance(t, func(t *testing.T) (schedules.Store, string) {
		pfx := testPrefix(t)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				"DELETE FROM conversations.schedule WHERE child_id LIKE $1", pfx+"%")
		})
		return schedulesdb.New(pool), pfx
	})
}

// TestGetReportsChildState pins the child state Get reads next to a job row: a
// missing conversations.child row reads ChildFound false, an open one reads
// ChildClosed false with its daemon id, and a closed one reads ChildClosed
// true. The fire callback decides ownership and liveness from exactly these
// three answers.
func TestGetReportsChildState(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := schedulesdb.New(pool)

	pfx := testPrefix(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM conversations.schedule WHERE child_id LIKE $1", pfx+"%")
		_, _ = pool.Exec(ctx, "DELETE FROM conversations.child WHERE child_id LIKE $1", pfx+"%")
	})
	childWithRow := pfx + "a"
	childNoRow := pfx + "b"
	daemonID := pfx + "daemon"

	if _, err := pool.Exec(ctx,
		`INSERT INTO conversations.child (child_id, kind, status, spawned_at, daemon_id, closed_at)
		 VALUES ($1, 'fundi', 'running', now(), $2, NULL)`, childWithRow, daemonID); err != nil {
		t.Fatalf("insert child row: %v", err)
	}

	c.Require().NoError(createJob(ctx, s, makeJob(pfx+"j1", childWithRow)), "create job for child with a row")
	c.Require().NoError(createJob(ctx, s, makeJob(pfx+"j2", childNoRow)), "create job for child without a row")

	got, err := s.Get(ctx, pfx+"j1")
	c.Require().NoError(err, "get job with a child row")
	c.True(got.ChildFound, "an existing child row must read ChildFound true")
	c.False(got.ChildClosed, "an open child must read ChildClosed false")
	c.Eq(daemonID, got.ChildDaemonID, "ChildDaemonID")

	gotNoRow, err := s.Get(ctx, pfx+"j2")
	c.Require().NoError(err, "get job without a child row")
	c.False(gotNoRow.ChildFound, "a missing child row must read ChildFound false")
	c.False(gotNoRow.ChildClosed, "a missing child must read ChildClosed false")
	c.Eq("", gotNoRow.ChildDaemonID, "a missing child's daemon id must be empty")

	if _, err := pool.Exec(ctx,
		`UPDATE conversations.child SET closed_at = now() WHERE child_id = $1`, childWithRow); err != nil {
		t.Fatalf("close child row: %v", err)
	}
	got, err = s.Get(ctx, pfx+"j1")
	c.Require().NoError(err, "get after closing the child")
	c.True(got.ChildFound, "ChildFound is about the row, not liveness")
	c.True(got.ChildClosed, "a closed child must read ChildClosed true")
}

// TestOwnerTimezone pins OwnerTimezone against the users table: a NULL
// timezone and an unknown or malformed id all read ok=false with no error,
// while a set value reads through.
func TestOwnerTimezone(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := schedulesdb.New(pool)

	pfx := testPrefix(t)
	username := pfx + "owner"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM conversations.users WHERE username = $1", username)
	})

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ($1, $2) RETURNING id::text`, username, pfx+"digest").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	tz, ok, err := s.OwnerTimezone(ctx, userID)
	c.Require().NoError(err, "owner timezone (NULL)")
	c.False(ok, "a NULL timezone must read ok=false")
	c.Eq("", tz, "a NULL timezone must read empty")

	if _, err := pool.Exec(ctx,
		`UPDATE conversations.users SET timezone = 'Europe/Paris' WHERE id = $1::uuid`, userID); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	tz, ok, err = s.OwnerTimezone(ctx, userID)
	c.Require().NoError(err, "owner timezone (set)")
	c.True(ok, "a set timezone must read ok=true")
	c.Eq("Europe/Paris", tz, "timezone")

	// Unknown (well-formed) id and malformed id are both answers, not outages.
	_, ok, err = s.OwnerTimezone(ctx, "11111111-1111-1111-1111-111111111111")
	c.Require().NoError(err, "owner timezone (unknown id)")
	c.False(ok, "an unknown user must read ok=false")

	_, ok, err = s.OwnerTimezone(ctx, "not-a-uuid")
	c.Require().NoError(err, "owner timezone (malformed id)")
	c.False(ok, "a malformed id must read ok=false")
}

// makeJob builds an open cron job for childID with id jobID.
func makeJob(jobID, childID string) schedules.Job {
	return schedules.Job{
		ID:         jobID,
		ChildID:    childID,
		Kind:       schedules.KindCron,
		Cron:       "0 9 * * *",
		TZ:         "UTC",
		Prompt:     "standup",
		Recurring:  true,
		NextFireAt: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC),
	}
}

func createJob(ctx context.Context, s schedules.Store, j schedules.Job) error {
	_, err := s.Create(ctx, j)
	return err
}
