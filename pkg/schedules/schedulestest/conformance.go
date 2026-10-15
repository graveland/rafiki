// SPDX-License-Identifier: Apache-2.0

// Package schedulestest holds the conformance suite every schedules.Store
// implementation must pass. Memory runs it directly (pkg/schedules'
// memory_test.go); the Postgres store runs the same suite against the
// shared test database, which is why every row id and child id a subtest
// creates is derived from the per-subtest prefix newStore hands back.
//
// The suite pins the behaviours named in the Store contract. Edges the
// contract leaves to the implementation (duplicate ids, missing-row
// Tombstone) are NOT asserted here — they are documented on the Store and
// on Memory.
package schedulestest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/schedules"
)

// newStoreFunc returns a Store and the child-id prefix unique to the
// subtest it serves. The prefix must be non-empty and unique across runs
// when the store is backed by a shared database; every subtest builds child
// ids as prefix+"a" / prefix+"b" and job ids from the same prefix, so rows
// tombstoned by an earlier run can never collide.
type newStoreFunc func(t *testing.T) (schedules.Store, string)

// RunConformance runs the store contract against a Store. newStore is
// called once per subtest and must hand back a store (fresh, or a shared
// backend namespaced by the prefix) plus a child-id prefix unique to that
// subtest.
func RunConformance(t *testing.T, newStore newStoreFunc) {
	t.Run("CreateThenListOpen", func(t *testing.T) { testCreateThenListOpen(t, newStore) })
	t.Run("CapReachedAtFifty", func(t *testing.T) { testCapReachedAtFifty(t, newStore) })
	t.Run("WakeupReplacesOpenWakeup", func(t *testing.T) { testWakeupReplacesOpenWakeup(t, newStore) })
	t.Run("DeleteIsScopedToChild", func(t *testing.T) { testDeleteIsScopedToChild(t, newStore) })
	t.Run("AdvanceClearsHeld", func(t *testing.T) { testAdvanceClearsHeld(t, newStore) })
	t.Run("RecordHeldAccumulates", func(t *testing.T) { testRecordHeldAccumulates(t, newStore) })
	t.Run("TombstoneIsIdempotent", func(t *testing.T) { testTombstoneIsIdempotent(t, newStore) })
	t.Run("CloseForChildClosesOnlyThatChild", func(t *testing.T) { testCloseForChildClosesOnlyThatChild(t, newStore) })
	t.Run("ListHeldFiltersByChildAndCount", func(t *testing.T) { testListHeldFiltersByChildAndCount(t, newStore) })
	t.Run("GetReturnsClosedRows", func(t *testing.T) { testGetReturnsClosedRows(t, newStore) })
}

// base is a fixed instant every subtest derives its NextFireAt values from.
// It is far from any DST edge: a store must not care about zones, and the
// fixtures should not flirt with one either.
var base = time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)

// newCronJob is an open recurring job for childID, with job id prefix+suffix.
func newCronJob(prefix, childID, suffix string, next time.Time) schedules.Job {
	return schedules.Job{
		ID:         prefix + suffix,
		ChildID:    childID,
		Kind:       schedules.KindCron,
		Cron:       "0 9 * * *",
		TZ:         "UTC",
		Prompt:     "standup",
		Recurring:  true,
		NextFireAt: next,
	}
}

// newWakeupJob is an open one-shot wakeup for childID, with job id
// prefix+suffix.
func newWakeupJob(prefix, childID, suffix string, next time.Time) schedules.Job {
	return schedules.Job{
		ID:         prefix + suffix,
		ChildID:    childID,
		Kind:       schedules.KindWakeup,
		Cron:       "",
		TZ:         "UTC",
		Prompt:     "check on it",
		Recurring:  false,
		NextFireAt: next,
	}
}

// create stores job and returns what the store handed back.
func create(t *testing.T, s schedules.Store, j schedules.Job) schedules.Job {
	t.Helper()
	got, err := s.Create(context.Background(), j)
	if err != nil {
		t.Fatalf("Create(%s): %v", j.ID, err)
	}
	return got
}

// mustGet fetches a row and fails the test when it is missing.
func mustGet(t *testing.T, s schedules.Store, id string) schedules.FireRow {
	t.Helper()
	row, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return row
}

// eqTime compares instants at Postgres precision: microseconds, in UTC.
func eqTime(c *assert.C, want, got time.Time, msgAndArgs ...any) {
	c.True(want.UTC().Truncate(time.Microsecond).Equal(got.UTC().Truncate(time.Microsecond)),
		append(msgAndArgs, fmt.Sprintf("time %s != %s", got, want))...)
}

func testCreateThenListOpen(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a, b := prefix+"a", prefix+"b"
	ctx := context.Background()

	// Created out of NextFireAt order on purpose; the tie pair t2/t1 is
	// created t2-first so ID order, not insertion order, breaks the tie.
	create(t, s, newCronJob(prefix, a, "j2", base.Add(1*time.Hour)))
	create(t, s, newCronJob(prefix, a, "t2", base))
	create(t, s, newCronJob(prefix, a, "j1", base.Add(2*time.Hour)))
	create(t, s, newCronJob(prefix, a, "t1", base))
	create(t, s, newCronJob(prefix, b, "b1", base))

	open, err := s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a)")
	c.Len(open, 4, "ListOpen(a) row count")
	c.Eq(prefix+"t1", open[0].ID, "ListOpen(a)[0]: tie broken by ID, not insertion")
	c.Eq(prefix+"t2", open[1].ID, "ListOpen(a)[1]")
	c.Eq(prefix+"j2", open[2].ID, "ListOpen(a)[2]: ordered by NextFireAt")
	c.Eq(prefix+"j1", open[3].ID, "ListOpen(a)[3]")

	bOpen, err := s.ListOpen(ctx, b)
	c.NoError(err, "ListOpen(b)")
	c.Len(bOpen, 1, "ListOpen(b) row count")
	c.Eq(prefix+"b1", bOpen[0].ID, "ListOpen(b)[0]")

	first := mustGet(t, s, prefix+"t1").Job
	c.Eq(prefix+"t1", first.ID, "Create echoed ID")
	c.Eq(a, first.ChildID, "Create echoed ChildID")
	c.False(first.CreatedAt.IsZero(), "Create assigned CreatedAt")
	c.True(first.ClosedAt == nil, "Create inserted the row open")
	c.True(first.LastFiredAt == nil, "Create left LastFiredAt unset")
}

func testCapReachedAtFifty(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a := prefix + "a"
	ctx := context.Background()

	// 49 cron jobs + 1 wakeup = 50 open rows: the cap exactly. All 50
	// creates succeed.
	for i := 0; i < 49; i++ {
		create(t, s, newCronJob(prefix, a, fmt.Sprintf("c%02d", i), base.Add(time.Duration(i)*time.Minute)))
	}
	create(t, s, newWakeupJob(prefix, a, "w1", base.Add(time.Hour)))
	open, err := s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a) at the cap")
	c.Len(open, 50, "50 creates must all succeed")

	// The 51st create of either kind is refused while 50 open rows stand
	// and there is no open wakeup to replace.
	_, err = s.Create(ctx, newCronJob(prefix, a, "c51", base.Add(time.Hour)))
	c.ErrorIs(err, schedules.ErrCapReached, "51st cron create")

	// A wakeup replace while at 50 still succeeds: the open wakeup it
	// replaces is tombstoned first and does not count toward the cap.
	create(t, s, newWakeupJob(prefix, a, "w2", base.Add(2*time.Hour)))
	row := mustGet(t, s, prefix+"w1")
	c.True(row.Job.ClosedAt != nil, "the replaced wakeup must be closed")
	open, err = s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a) after the replace")
	c.Len(open, 50, "the replace keeps the child at exactly 50 open rows")
	wakeups := 0
	for _, j := range open {
		if j.Kind == schedules.KindWakeup {
			wakeups++
		}
	}
	c.Eq(1, wakeups, "exactly one open wakeup survives the replace")

	// The cap is filled again: refused.
	_, err = s.Create(ctx, newCronJob(prefix, a, "c52", base.Add(time.Hour)))
	c.ErrorIs(err, schedules.ErrCapReached, "cron create after the refill")

	// With NO open wakeup to replace, a wakeup create is an ordinary
	// create: drop below the cap first, fill it back up with cron rows,
	// then refuse the wakeup.
	replaced, err := s.CancelWakeup(ctx, a)
	c.NoError(err, "CancelWakeup")
	c.True(replaced, "CancelWakeup removes the open wakeup")
	create(t, s, newCronJob(prefix, a, "c53", base.Add(time.Hour)))
	_, err = s.Create(ctx, newWakeupJob(prefix, a, "w3", base.Add(time.Hour)))
	c.ErrorIs(err, schedules.ErrCapReached, "wakeup create at 50 open cron rows")
}

func testWakeupReplacesOpenWakeup(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a := prefix + "a"
	ctx := context.Background()

	create(t, s, newWakeupJob(prefix, a, "w1", base))
	create(t, s, newWakeupJob(prefix, a, "w2", base.Add(time.Minute)))

	open, err := s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a)")
	c.Len(open, 1, "two wakeup creates leave one open wakeup")
	c.Eq(prefix+"w2", open[0].ID, "the surviving wakeup is the new one")
	c.Eq(schedules.KindWakeup, open[0].Kind, "the survivor is a wakeup")

	row := mustGet(t, s, prefix+"w1")
	c.True(row.Job.ClosedAt != nil, "the replaced wakeup is closed per Get")
}

func testDeleteIsScopedToChild(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a, b := prefix+"a", prefix+"b"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "j1", base))

	// b deleting a's id answers exactly like a missing one, and leaves the
	// row alone.
	c.ErrorIs(s.Delete(ctx, b, prefix+"j1"), schedules.ErrNotFound, "another child's delete")
	open, err := s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a) after b's delete")
	c.Len(open, 1, "a's job survives b's delete")

	c.ErrorIs(s.Delete(ctx, a, prefix+"nope"), schedules.ErrNotFound, "delete of a missing id")
	c.NoError(s.Delete(ctx, a, prefix+"j1"), "the owner's delete")

	open, err = s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a) after the delete")
	c.Empty(open, "a's job is no longer open")
	row := mustGet(t, s, prefix+"j1")
	c.True(row.Job.ClosedAt != nil, "Delete tombstones; the row stays answerable")
}

func testAdvanceClearsHeld(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a := prefix + "a"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "j1", base))
	id := prefix + "j1"

	due := base.Add(10 * time.Minute)
	c.NoError(s.RecordHeld(ctx, id, due, 2, nil), "RecordHeld sets up held state")
	c.ErrorIs(s.Advance(ctx, prefix+"nope", base, nil), schedules.ErrNotFound, "Advance of a missing id")

	next := base.Add(time.Hour)
	fired := base.Add(30 * time.Minute)
	c.NoError(s.Advance(ctx, id, next, &fired), "Advance")

	row := mustGet(t, s, id)
	eqTime(c, next, row.Job.NextFireAt, "Advance set NextFireAt")
	c.True(row.Job.LastFiredAt != nil, "Advance set LastFiredAt")
	eqTime(c, fired, *row.Job.LastFiredAt, "Advance set LastFiredAt = firedAt")
	c.Eq(0, row.Job.HeldCount, "Advance cleared HeldCount")
	c.True(row.Job.HeldFirstDue == nil, "Advance cleared HeldFirstDue")
	c.True(row.Job.HeldLastDue == nil, "Advance cleared HeldLastDue")

	// nil firedAt leaves LastFiredAt alone.
	next2 := base.Add(2 * time.Hour)
	c.NoError(s.Advance(ctx, id, next2, nil), "Advance with nil firedAt")
	row = mustGet(t, s, id)
	eqTime(c, next2, row.Job.NextFireAt, "Advance set NextFireAt (nil firedAt)")
	c.True(row.Job.LastFiredAt != nil, "nil firedAt left LastFiredAt set")
	eqTime(c, fired, *row.Job.LastFiredAt, "nil firedAt left LastFiredAt unchanged")

	// A closed row is not an open job.
	c.NoError(s.Tombstone(ctx, id, nil), "Tombstone")
	c.ErrorIs(s.Advance(ctx, id, next2, nil), schedules.ErrNotFound, "Advance of a closed id")
}

func testRecordHeldAccumulates(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a := prefix + "a"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "j1", base))
	id := prefix + "j1"

	due1 := base.Add(10 * time.Minute)
	c.NoError(s.RecordHeld(ctx, id, due1, 1, nil), "RecordHeld n=1")
	due2 := base.Add(20 * time.Minute)
	c.NoError(s.RecordHeld(ctx, id, due2, 3, nil), "RecordHeld n=3")

	row := mustGet(t, s, id)
	c.Eq(4, row.Job.HeldCount, "HeldCount accumulates 1+3")
	c.True(row.Job.HeldFirstDue != nil, "HeldFirstDue set")
	eqTime(c, due1, *row.Job.HeldFirstDue, "HeldFirstDue is the FIRST due")
	c.True(row.Job.HeldLastDue != nil, "HeldLastDue set")
	eqTime(c, due2, *row.Job.HeldLastDue, "HeldLastDue is the SECOND due")
	eqTime(c, base, row.Job.NextFireAt, "nil next left NextFireAt unchanged")

	// A non-nil next moves NextFireAt too.
	next := base.Add(time.Hour)
	c.NoError(s.RecordHeld(ctx, id, due2, 1, &next), "RecordHeld with next")
	row = mustGet(t, s, id)
	eqTime(c, next, row.Job.NextFireAt, "RecordHeld set NextFireAt = next")
}

func testTombstoneIsIdempotent(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a := prefix + "a"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "j1", base))
	id := prefix + "j1"

	fired := base.Add(time.Minute)
	c.NoError(s.Tombstone(ctx, id, &fired), "first Tombstone")
	first := mustGet(t, s, id)
	c.True(first.Job.ClosedAt != nil, "Tombstone stamped ClosedAt")
	c.True(first.Job.LastFiredAt != nil, "Tombstone set LastFiredAt")
	eqTime(c, fired, *first.Job.LastFiredAt, "Tombstone set LastFiredAt = firedAt")
	closedAt := *first.Job.ClosedAt

	// A second tombstone — with a DIFFERENT firedAt — leaves the row as is.
	fired2 := base.Add(2 * time.Minute)
	c.NoError(s.Tombstone(ctx, id, &fired2), "second Tombstone")
	row := mustGet(t, s, id)
	c.True(row.Job.ClosedAt != nil, "row still closed")
	eqTime(c, closedAt, *row.Job.ClosedAt, "ClosedAt unchanged by the second Tombstone")
	eqTime(c, fired, *row.Job.LastFiredAt, "LastFiredAt unchanged by the second Tombstone")
}

func testCloseForChildClosesOnlyThatChild(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a, b := prefix+"a", prefix+"b"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "a1", base))
	create(t, s, newWakeupJob(prefix, a, "a2", base.Add(time.Minute)))
	create(t, s, newCronJob(prefix, b, "b1", base))

	c.NoError(s.CloseForChild(ctx, a), "CloseForChild(a)")
	c.NoError(s.CloseForChild(ctx, a), "CloseForChild(a) is idempotent")

	openA, err := s.ListOpen(ctx, a)
	c.NoError(err, "ListOpen(a)")
	c.Empty(openA, "a's jobs are all closed")
	for _, id := range []string{prefix + "a1", prefix + "a2"} {
		row := mustGet(t, s, id)
		c.True(row.Job.ClosedAt != nil, "closed row %s stays answerable", id)
	}

	openB, err := s.ListOpen(ctx, b)
	c.NoError(err, "ListOpen(b)")
	c.Len(openB, 1, "b's job is untouched")
	c.Eq(prefix+"b1", openB[0].ID, "b's surviving job")
}

func testListHeldFiltersByChildAndCount(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a, b := prefix+"a", prefix+"b"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "h1", base))
	c.NoError(s.RecordHeld(ctx, prefix+"h1", base.Add(time.Minute), 1, nil), "RecordHeld(a/h1)")
	create(t, s, newCronJob(prefix, a, "o1", base.Add(time.Minute))) // open, HeldCount 0
	create(t, s, newCronJob(prefix, b, "h2", base))                  // open, not yet held

	// Only a's job is held so far.
	held, err := s.ListHeld(ctx, []string{a, b})
	c.NoError(err, "ListHeld([a,b])")
	c.Len(held, 1, "only the held rows come back")
	c.Eq(prefix+"h1", held[0].ID, "the held row")

	c.NoError(s.RecordHeld(ctx, prefix+"h2", base.Add(2*time.Minute), 2, nil), "RecordHeld(b/h2)")
	held, err = s.ListHeld(ctx, []string{a, b})
	c.NoError(err, "ListHeld([a,b]) both held")
	c.Len(held, 2, "both children's held rows")
	var ids []string
	for _, j := range held {
		ids = append(ids, j.ID)
	}
	c.ElementsMatch([]string{prefix + "h1", prefix + "h2"}, ids, "held rows of both children")

	// An empty child list answers empty, no error.
	held, err = s.ListHeld(ctx, nil)
	c.NoError(err, "ListHeld(nil)")
	c.Empty(held, "ListHeld(nil)")

	// A closed row is not held.
	c.NoError(s.Tombstone(ctx, prefix+"h1", nil), "Tombstone(h1)")
	held, err = s.ListHeld(ctx, []string{a, b})
	c.NoError(err, "ListHeld after tombstone")
	c.Len(held, 1, "closed rows drop out of ListHeld")
	c.Eq(prefix+"h2", held[0].ID, "the surviving held row")
}

func testGetReturnsClosedRows(t *testing.T, newStore newStoreFunc) {
	c := assert.NewAborting(t)
	s, prefix := newStore(t)
	a := prefix + "a"
	ctx := context.Background()

	create(t, s, newCronJob(prefix, a, "j1", base))
	c.NoError(s.Tombstone(ctx, prefix+"j1", nil), "Tombstone")

	row, err := s.Get(ctx, prefix+"j1")
	c.NoError(err, "Get of a closed row")
	c.True(row.Job.ClosedAt != nil, "closed row keeps its ClosedAt")
	c.Eq(a, row.Job.ChildID, "closed row keeps its ChildID")
	c.Eq(prefix+"j1", row.Job.ID, "closed row keeps its ID")

	_, err = s.Get(ctx, prefix+"nope")
	c.ErrorIs(err, schedules.ErrNotFound, "Get of a missing id")
	c.True(errors.Is(err, schedules.ErrNotFound), "errors.Is(ErrNotFound)")
}
