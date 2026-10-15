// SPDX-License-Identifier: Apache-2.0

// Package schedulesdb is the Postgres implementation of schedules.Store.
//
// It lives apart from pkg/schedules for the same reason pkg/inboxdb lives
// apart from pkg/inbox: Go imports whole packages, so the pgx-free interface
// package stays importable by cmd/rafiki (a socket client that must never open
// a database), while only the daemon and the direct-DSN CLI tools link pgx.
package schedulesdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/schedules"
)

// Store is a PostgreSQL-backed schedules.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a schedules.Store backed by pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Compile-time proof the store satisfies the contract schedulestest pins.
var _ schedules.Store = (*Store)(nil)

// jobColumns is the projection every Job-returning query shares, minus the
// child state Get adds. cron is COALESCEd to ”: tz is never ”, but cron is
// NULL for a wakeup.
const jobColumns = `id, child_id, kind, COALESCE(cron, ''), tz, prompt, recurring,
	next_fire_at, last_fired_at, held_count, held_first_due, held_last_due,
	created_at, closed_at`

// scanJob reads one jobColumns row.
func scanJob(row pgx.Row) (schedules.Job, error) {
	var j schedules.Job
	var lastFired, heldFirst, heldLast, closedAt *time.Time
	err := row.Scan(&j.ID, &j.ChildID, &j.Kind, &j.Cron, &j.TZ, &j.Prompt,
		&j.Recurring, &j.NextFireAt, &lastFired, &j.HeldCount, &heldFirst,
		&heldLast, &j.CreatedAt, &closedAt)
	j.LastFiredAt = lastFired
	j.HeldFirstDue = heldFirst
	j.HeldLastDue = heldLast
	j.ClosedAt = closedAt
	return j, err
}

// Create inserts j as open. The count-then-insert pair is atomic per child
// under an advisory transaction lock; a wakeup replaces the child's open
// wakeup first, so the replaced row does not count toward the cap. A refused
// create (cap) rolls back before any tombstone, so nothing is replaced.
func (s *Store) Create(ctx context.Context, j schedules.Job) (schedules.Job, error) {
	if j.ID == "" {
		return schedules.Job{}, errors.New("schedules: create: job id must not be empty")
	}
	if j.ChildID == "" {
		return schedules.Job{}, errors.New("schedules: create: child id must not be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return schedules.Job{}, fmt.Errorf("create schedule: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit has run

	// Serialize the whole create per child: without this lock two concurrent
	// creates can both read count < cap and both insert past it.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('schedule:' || $1))`, j.ChildID); err != nil {
		return schedules.Job{}, fmt.Errorf("create schedule: lock: %w", err)
	}

	// A wakeup create replaces the child's open wakeup. The replaced row is
	// closed here, BEFORE the count, so it does not count toward the cap.
	if j.Kind == schedules.KindWakeup {
		if _, err := tx.Exec(ctx,
			`UPDATE conversations.schedule SET closed_at = now()
			  WHERE child_id = $1 AND kind = 'wakeup' AND closed_at IS NULL`, j.ChildID); err != nil {
			return schedules.Job{}, fmt.Errorf("create schedule: replace wakeup: %w", err)
		}
	}

	var open int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM conversations.schedule
		  WHERE child_id = $1 AND closed_at IS NULL`, j.ChildID).Scan(&open); err != nil {
		return schedules.Job{}, fmt.Errorf("create schedule: count open: %w", err)
	}
	if open >= schedules.MaxOpenPerChild {
		return schedules.Job{}, schedules.ErrCapReached
	}

	var createdAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO conversations.schedule
		   (id, child_id, kind, cron, tz, prompt, recurring, next_fire_at,
		    last_fired_at, held_count, held_first_due, held_last_due)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11, $12)
		 RETURNING created_at`,
		j.ID, j.ChildID, j.Kind, j.Cron, j.TZ, j.Prompt, j.Recurring, j.NextFireAt,
		j.LastFiredAt, j.HeldCount, j.HeldFirstDue, j.HeldLastDue).Scan(&createdAt)
	if err != nil {
		return schedules.Job{}, fmt.Errorf("create schedule: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return schedules.Job{}, fmt.Errorf("create schedule: commit: %w", err)
	}

	// Create inserts the row OPEN: CreatedAt is the store's, ClosedAt is
	// forced nil, everything else is echoed as given (matching Memory).
	stored := j
	stored.CreatedAt = createdAt
	stored.ClosedAt = nil
	return stored, nil
}

// Get returns the row (open or closed) with its child's state; ErrNotFound
// when no row has that id.
func (s *Store) Get(ctx context.Context, id string) (schedules.FireRow, error) {
	var fr schedules.FireRow
	var lastFired, heldFirst, heldLast, closedAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.child_id, s.kind, COALESCE(s.cron, ''), s.tz, s.prompt,
		        s.recurring, s.next_fire_at, s.last_fired_at, s.held_count,
		        s.held_first_due, s.held_last_due, s.created_at, s.closed_at,
		        c.child_id IS NOT NULL, c.closed_at IS NOT NULL,
		        COALESCE(c.daemon_id, '')
		   FROM conversations.schedule s
		   LEFT JOIN conversations.child c ON c.child_id = s.child_id
		  WHERE s.id = $1`, id).Scan(
		&fr.Job.ID, &fr.Job.ChildID, &fr.Job.Kind, &fr.Job.Cron, &fr.Job.TZ,
		&fr.Job.Prompt, &fr.Job.Recurring, &fr.Job.NextFireAt, &lastFired,
		&fr.Job.HeldCount, &heldFirst, &heldLast, &fr.Job.CreatedAt, &closedAt,
		&fr.ChildFound, &fr.ChildClosed, &fr.ChildDaemonID)
	if errors.Is(err, pgx.ErrNoRows) {
		return schedules.FireRow{}, schedules.ErrNotFound
	}
	if err != nil {
		return schedules.FireRow{}, fmt.Errorf("get schedule %s: %w", id, err)
	}
	fr.Job.LastFiredAt = lastFired
	fr.Job.HeldFirstDue = heldFirst
	fr.Job.HeldLastDue = heldLast
	fr.Job.ClosedAt = closedAt
	return fr, nil
}

// ListOpen returns childID's open jobs ordered by NextFireAt, then ID.
func (s *Store) ListOpen(ctx context.Context, childID string) ([]schedules.Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+jobColumns+`
		   FROM conversations.schedule
		  WHERE child_id = $1 AND closed_at IS NULL
		  ORDER BY next_fire_at, id`, childID)
	if err != nil {
		return nil, fmt.Errorf("list open schedules for %s: %w", childID, err)
	}
	defer rows.Close()
	return collectJobs(rows)
}

// ListHeld returns the open jobs of any of childIDs with HeldCount > 0,
// ordered like ListOpen.
func (s *Store) ListHeld(ctx context.Context, childIDs []string) ([]schedules.Job, error) {
	if len(childIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+jobColumns+`
		   FROM conversations.schedule
		  WHERE child_id = ANY($1) AND closed_at IS NULL AND held_count > 0
		  ORDER BY next_fire_at, id`, childIDs)
	if err != nil {
		return nil, fmt.Errorf("list held schedules: %w", err)
	}
	defer rows.Close()
	return collectJobs(rows)
}

// collectJobs drains a jobColumns result set.
func collectJobs(rows pgx.Rows) ([]schedules.Job, error) {
	var out []schedules.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Delete tombstones job id IF it is an open job of childID; otherwise
// ErrNotFound (another child's id answers exactly like a missing one).
func (s *Store) Delete(ctx context.Context, childID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule SET closed_at = now()
		  WHERE id = $2 AND child_id = $1 AND closed_at IS NULL`, childID, id)
	if err != nil {
		return fmt.Errorf("delete schedule %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return schedules.ErrNotFound
	}
	return nil
}

// CancelWakeup tombstones childID's open wakeup; false when none.
func (s *Store) CancelWakeup(ctx context.Context, childID string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule SET closed_at = now()
		  WHERE child_id = $1 AND kind = 'wakeup' AND closed_at IS NULL`, childID)
	if err != nil {
		return false, fmt.Errorf("cancel wakeup for %s: %w", childID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Advance sets NextFireAt = next, sets LastFiredAt = *firedAt when firedAt is
// non-nil (left unchanged when nil), and clears the held state. ErrNotFound
// when id is not an open job.
func (s *Store) Advance(ctx context.Context, id string, next time.Time, firedAt *time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule
		    SET next_fire_at = $2,
		        last_fired_at = COALESCE($3, last_fired_at),
		        held_count = 0, held_first_due = NULL, held_last_due = NULL
		  WHERE id = $1 AND closed_at IS NULL`, id, next, firedAt)
	if err != nil {
		return fmt.Errorf("advance schedule %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return schedules.ErrNotFound
	}
	return nil
}

// RecordHeld adds n to HeldCount, sets HeldFirstDue to due if it was nil, sets
// HeldLastDue = due, and sets NextFireAt = *next when next != nil. ErrNotFound
// when id is not an open job. n must be >= 1: a non-positive n is refused with
// a plain error up front rather than tripping the held-count CHECK as a raw
// SQLSTATE.
func (s *Store) RecordHeld(ctx context.Context, id string, due time.Time, n int, next *time.Time) error {
	if n <= 0 {
		return errors.New("schedules: held count must be positive")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule
		    SET held_count = held_count + $3,
		        held_first_due = COALESCE(held_first_due, $2),
		        held_last_due = $2,
		        next_fire_at = COALESCE($4, next_fire_at)
		  WHERE id = $1 AND closed_at IS NULL`, id, due, n, next)
	if err != nil {
		return fmt.Errorf("record held for schedule %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return schedules.ErrNotFound
	}
	return nil
}

// ClearHeld zeroes HeldCount and nils both held instants. ErrNotFound when id
// is not an open job.
func (s *Store) ClearHeld(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule
		    SET held_count = 0, held_first_due = NULL, held_last_due = NULL
		  WHERE id = $1 AND closed_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("clear held for schedule %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return schedules.ErrNotFound
	}
	return nil
}

// Tombstone stamps ClosedAt = now (and LastFiredAt = *firedAt when non-nil).
// Idempotent: an already-closed row is left as is (an already-set ClosedAt and
// LastFiredAt are not moved), no error. A missing id is ErrNotFound — the
// WHERE matches the row whether open or closed, so zero rows means gone.
func (s *Store) Tombstone(ctx context.Context, id string, firedAt *time.Time) error {
	// Both SET expressions read the OLD row: Postgres evaluates an UPDATE's
	// SET list against the pre-update values. closed_at is stamped only when
	// it was NULL, and last_fired_at only moves on that same open->closed
	// transition, so a second Tombstone changes nothing.
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule
		    SET closed_at = COALESCE(closed_at, now()),
		        last_fired_at = CASE WHEN closed_at IS NULL
		                             THEN COALESCE($2, last_fired_at)
		                             ELSE last_fired_at END
		  WHERE id = $1`, id, firedAt)
	if err != nil {
		return fmt.Errorf("tombstone schedule %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return schedules.ErrNotFound
	}
	return nil
}

// CloseForChild tombstones every open job of childID. A child with nothing
// open is a no-op, not an error.
func (s *Store) CloseForChild(ctx context.Context, childID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.schedule SET closed_at = now()
		  WHERE child_id = $1 AND closed_at IS NULL`, childID); err != nil {
		return fmt.Errorf("close schedules for %s: %w", childID, err)
	}
	return nil
}

// OwnerTimezone returns users.timezone for userID; ok=false when the user row
// is missing or its timezone is NULL. An unparseable id names nobody: a
// false answer, never a SQLSTATE 22P02 outage.
func (s *Store) OwnerTimezone(ctx context.Context, userID string) (tz string, ok bool, err error) {
	if _, err := uuid.Parse(userID); err != nil {
		return "", false, nil
	}
	var zone *string
	err = s.pool.QueryRow(ctx,
		`SELECT timezone FROM conversations.users WHERE id = $1::uuid`, userID).Scan(&zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("owner timezone for %s: %w", userID, err)
	}
	if zone == nil {
		return "", false, nil
	}
	return *zone, true, nil
}
