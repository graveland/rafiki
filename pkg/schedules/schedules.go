// SPDX-License-Identifier: Apache-2.0

// Package schedules is the domain for agent-facing schedules: recurring cron
// jobs and one-shot wakeups a child (or its owner) asks the daemon to fire on
// its behalf.
//
// It is a pgx-free package, like pkg/inbox: it defines the Store interface a
// Postgres-backed implementation satisfies, not the implementation. The
// schedule algebra lives here too — cron parsing under a fixed DST contract
// (cron.go), the daemon's zone resolution (zone.go) — so every caller of a
// schedule, daemon or test, agrees on when a job fires.
//
// Rows are tombstoned, never deleted: a closed job keeps its row with
// ClosedAt set, which is what lets a fire callback distinguish "gone" from
// "finished" and lets lineage reads see past closure.
package schedules

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	KindCron   = "cron"
	KindWakeup = "wakeup"

	// MinGap is the closest two fires of one recurring job may be, and the
	// floor ScheduleWakeup's delay clamps to.
	MinGap = 5 * time.Minute
	// WakeupMax is ScheduleWakeup's ceiling.
	WakeupMax = time.Hour
	// MaxOpenPerChild bounds open jobs (both kinds) per child.
	MaxOpenPerChild = 50
	// gapSamples is how many consecutive occurrences CheckMinGap inspects.
	gapSamples = 32
)

var (
	ErrNotFound   = errors.New("schedules: no such job")
	ErrCapReached = errors.New("schedules: per-child job cap reached")
	ErrNeverFires = errors.New("schedules: cron expression never fires")
	ErrNoTimezone = errors.New("schedules: no timezone could be resolved")
)

// Job is one schedule row. A row is open until ClosedAt is stamped; closed
// rows are kept forever.
type Job struct {
	ID           string
	ChildID      string
	Kind         string // KindCron or KindWakeup
	Cron         string // "" for KindWakeup
	TZ           string // IANA name; never ""
	Prompt       string
	Recurring    bool
	NextFireAt   time.Time
	LastFiredAt  *time.Time
	HeldCount    int
	HeldFirstDue *time.Time
	HeldLastDue  *time.Time
	CreatedAt    time.Time
	ClosedAt     *time.Time
}

// FireRow is a job as the fire callback sees it: the row plus its child's
// durable state, read together so ownership and closure are checked at FIRE
// time.
type FireRow struct {
	Job           Job
	ChildFound    bool   // false: no conversations.child row for Job.ChildID
	ChildClosed   bool   // child.closed_at IS NOT NULL
	ChildDaemonID string // child.daemon_id; "" when NULL
}

// Store is the durable contract behind schedules. Implementations live in
// storage packages (pkg/schedulesdb, and pkg/schedules.Memory for tests);
// both are held to the same edge semantics by
// schedulestest.RunConformance.
//
// Edge semantics this contract fixes beyond the method comments below:
//
//   - Create stores j.ID as given; the CALLER assigns ids with NewID, the
//     store never invents one. An empty ID or empty ChildID is refused, and
//     a duplicate ID is an error.
//   - Create is all-or-nothing: a refused create (cap) leaves the child's
//     open wakeups untouched — the replaced wakeup stays open.
//   - Advance, RecordHeld, ClearHeld and Tombstone refuse a MISSING id with
//     ErrNotFound, and all but Tombstone refuse a CLOSED id the same way
//     ("id is not an open job", like Advance). Tombstone on an already
//     closed row is the documented idempotent no-op.
//   - ListHeld orders like ListOpen: NextFireAt, then ID.
type Store interface {
	// Create inserts j as open. Count-then-insert is atomic per child (the
	// Postgres store holds pg_advisory_xact_lock(hashtext('schedule:'||child_id))).
	// For KindWakeup, any open wakeup of j.ChildID is tombstoned in the same
	// transaction FIRST and does not count toward the cap. Refuses with
	// ErrCapReached when MaxOpenPerChild open rows already exist. CreatedAt is
	// assigned by the store.
	Create(ctx context.Context, j Job) (Job, error)
	// Get returns the row (open or closed) with its child's state; ErrNotFound
	// when no row has that id.
	Get(ctx context.Context, id string) (FireRow, error)
	// ListOpen returns childID's open jobs ordered by NextFireAt, then ID.
	ListOpen(ctx context.Context, childID string) ([]Job, error)
	// ListHeld returns the open jobs of any of childIDs with HeldCount > 0.
	ListHeld(ctx context.Context, childIDs []string) ([]Job, error)
	// Delete tombstones job id IF it is an open job of childID; otherwise
	// ErrNotFound (another child's id answers exactly like a missing one).
	Delete(ctx context.Context, childID, id string) error
	// CancelWakeup tombstones childID's open wakeup; false when none.
	CancelWakeup(ctx context.Context, childID string) (bool, error)
	// Advance sets NextFireAt = next, sets LastFiredAt = *firedAt when firedAt
	// is non-nil (left unchanged when nil), and clears HeldCount/HeldFirstDue/
	// HeldLastDue. ErrNotFound when id is not an open job.
	Advance(ctx context.Context, id string, next time.Time, firedAt *time.Time) error
	// RecordHeld adds n to HeldCount, sets HeldFirstDue to due if it was nil,
	// sets HeldLastDue = due, and sets NextFireAt = *next when next != nil.
	// n must be >= 1 (a fire is one occurrence at minimum); a non-positive n is
	// refused with an error and leaves the row unchanged.
	RecordHeld(ctx context.Context, id string, due time.Time, n int, next *time.Time) error
	// ClearHeld zeroes HeldCount and nils both held instants.
	ClearHeld(ctx context.Context, id string) error
	// Tombstone stamps ClosedAt = now (and LastFiredAt = *firedAt when
	// non-nil). Idempotent: an already-closed row is left as is, no error.
	Tombstone(ctx context.Context, id string, firedAt *time.Time) error
	// CloseForChild tombstones every open job of childID.
	CloseForChild(ctx context.Context, childID string) error
	// OwnerTimezone returns users.timezone for userID; ok=false when the user
	// row is missing or its timezone is NULL.
	OwnerTimezone(ctx context.Context, userID string) (tz string, ok bool, err error)
}

// NewID returns a fresh job id: "sch_" + 8 chars of lowercase unpadded
// base32 over 5 bytes from crypto/rand.
func NewID() (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("schedules: reading id entropy: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "sch_" + strings.ToLower(enc.EncodeToString(b[:])), nil
}

// ValidateTimezone refuses an empty zone, Go's "Local" pseudo-zone, and any
// name time.LoadLocation does not know. IANA names are the only thing that
// may reach a Job.TZ — "Local" and "" both mean "depends on the machine",
// which is exactly what a schedule must not.
func ValidateTimezone(tz string) error {
	if tz == "" {
		return errors.New("timezone must not be empty")
	}
	if tz == "Local" {
		return fmt.Errorf("timezone %q is not an IANA name", tz)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("unknown timezone %q: %w", tz, err)
	}
	return nil
}
