// SPDX-License-Identifier: Apache-2.0

package schedules

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// childState is the durable child state the fire callback reads next to a
// job row.
type childState struct {
	closed   bool
	daemonID string
}

// Memory is an in-memory Store: the reference implementation of the Store
// contract and the store unit tests run against. The Postgres store is held
// to the same behaviour by schedulestest.RunConformance.
//
// Semantics Memory fixes where the Store method comments are silent:
//
//   - A child never passed to SetChild reads as ChildFound: true,
//     ChildClosed: false, ChildDaemonID: "". Unit tests that don't care
//     about child state must not have their jobs tombstoned as vanished;
//     only the Postgres store can see a missing conversations.child row, so
//     Memory cannot express ChildFound: false.
//   - Create stores the id as given (callers assign ids with NewID) and
//     inserts the row OPEN: ClosedAt is forced nil and CreatedAt is assigned
//     by the store, whatever the caller passed. An empty ID, an empty
//     ChildID, or a duplicate ID is refused with a plain error.
//   - A refused create leaves the child's open wakeups untouched: the cap
//     counts the open rows a KindWakeup create would leave behind (open
//     wakeups are replaced first in the successful path only), matching the
//     Postgres store's tombstone-then-count-then-insert transaction.
//   - RecordHeld, ClearHeld and Tombstone refuse a missing or closed id with
//     ErrNotFound, like Advance; Tombstone on an already-closed row is the
//     documented idempotent no-op.
//   - SetOwnerTimezone(userID, "") records the NULL/unset case:
//     OwnerTimezone answers ok=false for it, like a missing user row.
//
// Rows are tombstoned, never removed: Tombstone and Delete only stamp
// ClosedAt, so Get keeps answering closed rows for the job's lifetime.
type Memory struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	children map[string]childState
	ownerTZ  map[string]string
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		jobs:     make(map[string]*Job),
		children: make(map[string]childState),
		ownerTZ:  make(map[string]string),
	}
}

// SetChild records the durable child state the fire path reads. A child
// never passed here reads as ChildFound: true, ChildClosed: false,
// ChildDaemonID: "" — see Memory's doc comment.
func (m *Memory) SetChild(childID string, closed bool, daemonID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.children[childID] = childState{closed: closed, daemonID: daemonID}
}

// SetOwnerTimezone records users.timezone for userID. An empty tz is the
// NULL/unset case: OwnerTimezone answers ok=false for it.
func (m *Memory) SetOwnerTimezone(userID, tz string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ownerTZ[userID] = tz
}

// Create inserts j as open, replacing the child's open wakeup when j is a
// wakeup. See Memory's doc comment for the cap and validation semantics.
func (m *Memory) Create(ctx context.Context, j Job) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if j.ID == "" {
		return Job{}, fmt.Errorf("schedules: create: job id must not be empty")
	}
	if j.ChildID == "" {
		return Job{}, fmt.Errorf("schedules: create: child id must not be empty")
	}
	if _, ok := m.jobs[j.ID]; ok {
		return Job{}, fmt.Errorf("schedules: create: job id %q already exists", j.ID)
	}

	// Count the open rows the cap is checked against. A wakeup create is
	// about to tombstone the child's open wakeups, so they do not count;
	// everything else open for the child does. Refusing here — before any
	// tombstone — is the all-or-nothing behaviour of the Postgres store's
	// transaction: a refused create replaces nothing.
	open := 0
	for _, row := range m.jobs {
		if row.ChildID != j.ChildID || row.ClosedAt != nil {
			continue
		}
		if j.Kind == KindWakeup && row.Kind == KindWakeup {
			continue
		}
		open++
	}
	if open >= MaxOpenPerChild {
		return Job{}, ErrCapReached
	}

	now := time.Now().UTC()
	if j.Kind == KindWakeup {
		for _, row := range m.jobs {
			if row.ChildID == j.ChildID && row.Kind == KindWakeup && row.ClosedAt == nil {
				closed := now
				row.ClosedAt = &closed
			}
		}
	}

	stored := j
	stored.CreatedAt = now
	stored.ClosedAt = nil
	m.jobs[stored.ID] = &stored
	return copyJob(&stored), nil
}

// Get returns the row (open or closed) with its child's state; ErrNotFound
// when no row has that id.
func (m *Memory) Get(ctx context.Context, id string) (FireRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.jobs[id]
	if !ok {
		return FireRow{}, ErrNotFound
	}
	fire := FireRow{Job: copyJob(row), ChildFound: true}
	if cs, ok := m.children[row.ChildID]; ok {
		fire.ChildClosed = cs.closed
		fire.ChildDaemonID = cs.daemonID
	}
	return fire, nil
}

// ListOpen returns childID's open jobs ordered by NextFireAt, then ID.
func (m *Memory) ListOpen(ctx context.Context, childID string) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, row := range m.jobs {
		if row.ChildID == childID && row.ClosedAt == nil {
			out = append(out, copyJob(row))
		}
	}
	sortJobs(out)
	return out, nil
}

// ListHeld returns the open jobs of any of childIDs with HeldCount > 0,
// ordered like ListOpen.
func (m *Memory) ListHeld(ctx context.Context, childIDs []string) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := make(map[string]bool, len(childIDs))
	for _, id := range childIDs {
		want[id] = true
	}
	var out []Job
	for _, row := range m.jobs {
		if want[row.ChildID] && row.ClosedAt == nil && row.HeldCount > 0 {
			out = append(out, copyJob(row))
		}
	}
	sortJobs(out)
	return out, nil
}

// Delete tombstones job id IF it is an open job of childID; otherwise
// ErrNotFound (another child's id answers exactly like a missing one).
func (m *Memory) Delete(ctx context.Context, childID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.jobs[id]
	if !ok || row.ChildID != childID || row.ClosedAt != nil {
		return ErrNotFound
	}
	now := time.Now().UTC()
	row.ClosedAt = &now
	return nil
}

// CancelWakeup tombstones childID's open wakeup; false when none.
func (m *Memory) CancelWakeup(ctx context.Context, childID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	now := time.Now().UTC()
	for _, row := range m.jobs {
		if row.ChildID == childID && row.Kind == KindWakeup && row.ClosedAt == nil {
			closed := now
			row.ClosedAt = &closed
			found = true
		}
	}
	return found, nil
}

// Advance sets NextFireAt = next, sets LastFiredAt = *firedAt when firedAt
// is non-nil (left unchanged when nil), and clears HeldCount/HeldFirstDue/
// HeldLastDue. ErrNotFound when id is not an open job.
func (m *Memory) Advance(ctx context.Context, id string, next time.Time, firedAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.open(id)
	if !ok {
		return ErrNotFound
	}
	row.NextFireAt = next
	if firedAt != nil {
		t := *firedAt
		row.LastFiredAt = &t
	}
	row.HeldCount = 0
	row.HeldFirstDue = nil
	row.HeldLastDue = nil
	return nil
}

// RecordHeld adds n to HeldCount, sets HeldFirstDue to due if it was nil,
// sets HeldLastDue = due, and sets NextFireAt = *next when next != nil.
// ErrNotFound when id is not an open job, like Advance.
func (m *Memory) RecordHeld(ctx context.Context, id string, due time.Time, n int, next *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.open(id)
	if !ok {
		return ErrNotFound
	}
	row.HeldCount += n
	if row.HeldFirstDue == nil {
		t := due
		row.HeldFirstDue = &t
	}
	t := due
	row.HeldLastDue = &t
	if next != nil {
		nt := *next
		row.NextFireAt = nt
	}
	return nil
}

// ClearHeld zeroes HeldCount and nils both held instants. ErrNotFound when
// id is not an open job, like Advance.
func (m *Memory) ClearHeld(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.open(id)
	if !ok {
		return ErrNotFound
	}
	row.HeldCount = 0
	row.HeldFirstDue = nil
	row.HeldLastDue = nil
	return nil
}

// Tombstone stamps ClosedAt = now (and LastFiredAt = *firedAt when non-nil).
// Idempotent: an already-closed row is left as is, no error. A missing id is
// ErrNotFound.
func (m *Memory) Tombstone(ctx context.Context, id string, firedAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if row.ClosedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	row.ClosedAt = &now
	if firedAt != nil {
		t := *firedAt
		row.LastFiredAt = &t
	}
	return nil
}

// CloseForChild tombstones every open job of childID.
func (m *Memory) CloseForChild(ctx context.Context, childID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, row := range m.jobs {
		if row.ChildID == childID && row.ClosedAt == nil {
			closed := now
			row.ClosedAt = &closed
		}
	}
	return nil
}

// OwnerTimezone returns users.timezone for userID; ok=false when the user
// row is missing or its timezone is NULL.
func (m *Memory) OwnerTimezone(ctx context.Context, userID string) (tz string, ok bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tz, ok = m.ownerTZ[userID]
	if !ok || tz == "" {
		return "", false, nil
	}
	return tz, true, nil
}

// open returns the open row with that id under m.mu.
func (m *Memory) open(id string) (*Job, bool) {
	row, ok := m.jobs[id]
	if !ok || row.ClosedAt != nil {
		return nil, false
	}
	return row, true
}

// copyJob returns a Job whose pointer fields are no longer aliased to the
// stored row: callers must not be able to reach into the store through a
// returned *time.Time.
func copyJob(j *Job) Job {
	c := *j
	if j.LastFiredAt != nil {
		t := *j.LastFiredAt
		c.LastFiredAt = &t
	}
	if j.HeldFirstDue != nil {
		t := *j.HeldFirstDue
		c.HeldFirstDue = &t
	}
	if j.HeldLastDue != nil {
		t := *j.HeldLastDue
		c.HeldLastDue = &t
	}
	if j.ClosedAt != nil {
		t := *j.ClosedAt
		c.ClosedAt = &t
	}
	return c
}

// sortJobs orders jobs by NextFireAt, then ID — ListOpen's contract order.
func sortJobs(jobs []Job) {
	slices.SortFunc(jobs, func(a, b Job) int {
		if c := a.NextFireAt.Compare(b.NextFireAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}
