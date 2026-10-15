// SPDX-License-Identifier: Apache-2.0

// This file is the EXTERNAL test package (schedules_test) because it runs
// the schedulestest conformance suite, which imports the package under
// test; the in-package tests (cron_test.go, zone_test.go) pin the
// unexported details.
package schedules_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/schedules"
	"go.graveland.dev/rafiki/pkg/schedules/schedulestest"
)

// TestMemoryConformance runs the full Store contract against Memory. The
// prefix is per-call unique even though a Memory store is fresh per subtest,
// so the same helper shape a database-backed store will need.
func TestMemoryConformance(t *testing.T) {
	schedulestest.RunConformance(t, func(t *testing.T) (schedules.Store, string) {
		t.Helper()
		id, err := schedules.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return schedules.NewMemory(), id
	})
}

// TestMemoryDefaultChildState pins the Memory-only default: a child never
// passed to SetChild reads as found, open, daemonless, so unit tests that
// don't care about child state must not have their jobs tombstoned as
// vanished.
func TestMemoryDefaultChildState(t *testing.T) {
	c := assert.NewAborting(t)
	m := schedules.NewMemory()
	ctx := t.Context()

	_, err := m.Create(ctx, schedules.Job{
		ID:         "sch_j1",
		ChildID:    "child-unknown",
		Kind:       schedules.KindCron,
		Cron:       "0 9 * * *",
		TZ:         "UTC",
		Prompt:     "ping",
		Recurring:  true,
		NextFireAt: time.Now().UTC().Add(time.Hour),
	})
	c.NoError(err, "Create")

	row, err := m.Get(ctx, "sch_j1")
	c.NoError(err, "Get")
	c.True(row.ChildFound, "an unregistered child reads as found")
	c.False(row.ChildClosed, "an unregistered child reads as open")
	c.Eq("", row.ChildDaemonID, "an unregistered child reads as daemonless")

	// SetChild is what the fire-path tests use to model real child state.
	m.SetChild("child-unknown", true, "daemon-7")
	row, err = m.Get(ctx, "sch_j1")
	c.NoError(err, "Get after SetChild")
	c.True(row.ChildFound, "ChildFound stays true")
	c.True(row.ChildClosed, "ChildClosed follows SetChild")
	c.Eq("daemon-7", row.ChildDaemonID, "ChildDaemonID follows SetChild")

	m.SetChild("child-unknown", false, "")
	row, err = m.Get(ctx, "sch_j1")
	c.NoError(err, "Get after SetChild reset")
	c.False(row.ChildClosed, "ChildClosed follows SetChild back to open")
	c.Eq("", row.ChildDaemonID, "ChildDaemonID follows SetChild back to empty")
}

// TestMemoryOwnerTimezone pins the OwnerTimezone contract: a missing user
// row and a NULL/empty timezone both answer ok=false.
func TestMemoryOwnerTimezone(t *testing.T) {
	c := assert.NewAborting(t)
	m := schedules.NewMemory()
	ctx := t.Context()

	_, ok, err := m.OwnerTimezone(ctx, "user-1")
	c.NoError(err, "OwnerTimezone of an unknown user")
	c.False(ok, "an unknown user has no timezone")

	m.SetOwnerTimezone("user-1", "Europe/Paris")
	tz, ok, err := m.OwnerTimezone(ctx, "user-1")
	c.NoError(err, "OwnerTimezone")
	c.True(ok, "a set timezone is found")
	c.Eq("Europe/Paris", tz, "the timezone value")

	m.SetOwnerTimezone("user-2", "") // the NULL/unset case
	_, ok, err = m.OwnerTimezone(ctx, "user-2")
	c.NoError(err, "OwnerTimezone of a NULL timezone")
	c.False(ok, "an empty timezone answers ok=false, like NULL")
}

// TestMemoryCreateEdges pins the Create edges Memory fixes: empty ids and
// duplicate ids are refused, and the row is stored open whatever the caller
// passed in ClosedAt, with CreatedAt assigned by the store.
func TestMemoryCreateEdges(t *testing.T) {
	c := assert.NewAborting(t)
	m := schedules.NewMemory()
	ctx := t.Context()

	_, err := m.Create(ctx, schedules.Job{ChildID: "a", Kind: schedules.KindCron, TZ: "UTC"})
	c.Error(err, "an empty id is refused")

	_, err = m.Create(ctx, schedules.Job{ID: "sch_x", Kind: schedules.KindCron, TZ: "UTC"})
	c.Error(err, "an empty child id is refused")

	closed := time.Now().UTC()
	good := schedules.Job{
		ID:         "sch_x",
		ChildID:    "a",
		Kind:       schedules.KindCron,
		Cron:       "0 9 * * *",
		TZ:         "UTC",
		NextFireAt: closed.Add(time.Hour),
		ClosedAt:   &closed, // must be ignored: Create inserts OPEN
	}
	got, err := m.Create(ctx, good)
	c.NoError(err, "Create")
	c.True(got.ClosedAt == nil, "Create inserts the row open")
	c.False(got.CreatedAt.IsZero(), "Create assigns CreatedAt")
	c.True(got.CreatedAt.Before(closed.Add(time.Hour)), "CreatedAt is the store's, not the caller's")

	_, err = m.Create(ctx, good)
	c.Error(err, "a duplicate id is refused")
}

// TestMemoryRefusedCreateTouchesNothing pins the all-or-nothing rule: a
// cap-refused create must not change any row. A wakeup create is refused
// only when there is no open wakeup to replace (the cap counts the open
// rows it would leave behind), so the interesting case is the refused cron
// create next to a live open wakeup.
func TestMemoryRefusedCreateTouchesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	m := schedules.NewMemory()
	ctx := t.Context()
	base := time.Now().UTC().Add(time.Hour)

	for i := 0; i < schedules.MaxOpenPerChild; i++ {
		_, err := m.Create(ctx, schedules.Job{
			ID:         "sch_" + fmt.Sprintf("c%02d", i),
			ChildID:    "a",
			Kind:       schedules.KindCron,
			Cron:       "0 9 * * *",
			TZ:         "UTC",
			NextFireAt: base.Add(time.Duration(i) * time.Minute),
		})
		c.NoError(err, "Create cron %d", i)
	}

	// At the cap with no open wakeup: both kinds are refused, nothing moves.
	_, err := m.Create(ctx, schedules.Job{
		ID: "sch_cx", ChildID: "a", Kind: schedules.KindCron,
		Cron: "0 9 * * *", TZ: "UTC", NextFireAt: base,
	})
	c.ErrorIs(err, schedules.ErrCapReached, "cron create at the cap")
	_, err = m.Create(ctx, schedules.Job{
		ID: "sch_w0", ChildID: "a", Kind: schedules.KindWakeup,
		TZ: "UTC", NextFireAt: base,
	})
	c.ErrorIs(err, schedules.ErrCapReached, "wakeup create at the cap with nothing to replace")
	open, err := m.ListOpen(ctx, "a")
	c.NoError(err, "ListOpen after the refusals")
	c.Len(open, schedules.MaxOpenPerChild, "a refused create touches nothing")

	// Make room for a wakeup, then refuse a cron create next to it.
	c.NoError(m.Delete(ctx, "a", "sch_c00"), "Delete one cron row")
	_, err = m.Create(ctx, schedules.Job{
		ID: "sch_w1", ChildID: "a", Kind: schedules.KindWakeup,
		TZ: "UTC", NextFireAt: base,
	})
	c.NoError(err, "Create wakeup below the cap")
	_, err = m.Create(ctx, schedules.Job{
		ID: "sch_cy", ChildID: "a", Kind: schedules.KindCron,
		Cron: "0 9 * * *", TZ: "UTC", NextFireAt: base,
	})
	c.ErrorIs(err, schedules.ErrCapReached, "cron create at the cap again")
	row, err := m.Get(ctx, "sch_w1")
	c.NoError(err, "Get the wakeup after the refused create")
	c.True(row.Job.ClosedAt == nil, "the open wakeup is untouched by the refusal")
}

// TestMemoryReturnsCopies pins that reads hand out copies: mutating a
// returned Job must not reach into the store.
func TestMemoryReturnsCopies(t *testing.T) {
	c := assert.NewAborting(t)
	m := schedules.NewMemory()
	ctx := t.Context()
	base := time.Now().UTC().Add(time.Hour)

	_, err := m.Create(ctx, schedules.Job{
		ID: "sch_j1", ChildID: "a", Kind: schedules.KindCron,
		Cron: "0 9 * * *", TZ: "UTC", NextFireAt: base,
	})
	c.NoError(err, "Create")
	c.NoError(m.RecordHeld(ctx, "sch_j1", base, 2, nil), "RecordHeld")

	got, err := m.Get(ctx, "sch_j1")
	c.NoError(err, "Get")
	got.Job.HeldCount = 99
	*got.Job.HeldFirstDue = time.Now().UTC().Add(-time.Hour)

	again, err := m.Get(ctx, "sch_j1")
	c.NoError(err, "Get again")
	c.Eq(2, again.Job.HeldCount, "the store's HeldCount is untouched")
	c.True(again.Job.HeldFirstDue != nil, "HeldFirstDue still set")
	c.True(again.Job.HeldFirstDue.Equal(base), "HeldFirstDue still the recorded due")
}

// TestMemoryRowsAreNeverDeleted pins the never-delete rule: closed rows
// stay answerable through Get, and Delete/Tombstone only stamp ClosedAt.
func TestMemoryRowsAreNeverDeleted(t *testing.T) {
	c := assert.NewAborting(t)
	m := schedules.NewMemory()
	ctx := t.Context()
	base := time.Now().UTC().Add(time.Hour)

	_, err := m.Create(ctx, schedules.Job{
		ID: "sch_j1", ChildID: "a", Kind: schedules.KindCron,
		Cron: "0 9 * * *", TZ: "UTC", NextFireAt: base,
	})
	c.NoError(err, "Create")
	c.NoError(m.Tombstone(ctx, "sch_j1", nil), "Tombstone")
	row, err := m.Get(ctx, "sch_j1")
	c.NoError(err, "Get after Tombstone")
	c.True(row.Job.ClosedAt != nil, "the tombstoned row is still answerable")

	_, err = m.Create(ctx, schedules.Job{
		ID: "sch_j2", ChildID: "a", Kind: schedules.KindCron,
		Cron: "0 9 * * *", TZ: "UTC", NextFireAt: base,
	})
	c.NoError(err, "Create")
	c.NoError(m.Delete(ctx, "a", "sch_j2"), "Delete")
	row, err = m.Get(ctx, "sch_j2")
	c.NoError(err, "Get after Delete")
	c.True(row.Job.ClosedAt != nil, "a deleted row is tombstoned, not removed")
}
