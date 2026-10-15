// SPDX-License-Identifier: Apache-2.0

package schedules

import (
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// toronto returns the America/Toronto location every cron test runs in —
// a zone whose spring-forward and fall-back edges the tests cross on
// purpose.
func toronto(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// at is a wall-clock instant in loc.
func at(loc *time.Location, year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, loc)
}

// TestNextSpringForwardSkippedTimeDoesNotFire pins the spring-forward side
// of the DST contract: a wall clock that does not exist (02:30 on
// 2026-03-08, which Toronto skips from 02:00 EST straight to 03:00 EDT)
// must not fire. The next three fires after 2026-03-07 noon are the 02:30s
// of the following three days.
func TestNextSpringForwardSkippedTimeDoesNotFire(t *testing.T) {
	c := assert.NewAborting(t)
	loc := toronto(t)
	spec, err := Parse("30 2 * * *", "America/Toronto")
	c.NoError(err, `Parse("30 2 * * *")`)

	after := at(loc, 2026, time.March, 7, 12, 0)
	want := []time.Time{
		at(loc, 2026, time.March, 9, 2, 30),
		at(loc, 2026, time.March, 10, 2, 30),
		at(loc, 2026, time.March, 11, 2, 30),
	}
	for i, w := range want {
		got := spec.Next(after)
		c.True(got.Equal(w), "fire %d: Next(%s) = %s, want %s", i+1, after, got, w)
		after = got
	}
}

// TestNextFallBackRepeatedTimeFiresOnce pins the fall-back side of the DST
// contract: a wall clock that happens TWICE (01:30 on 2026-11-01, Toronto's
// 02:00 EDT -> 01:00 EST) must fire ONCE. The second fire is the next day's
// 01:30 EST, not the same day's repeated 01:30 EST.
//
// This test MUST fail if the wall-clock skip in Next is deleted: robfig
// cron v3.0.1 alone returns 2026-11-01 01:30 EST here, a second fire one
// absolute hour after the first.
func TestNextFallBackRepeatedTimeFiresOnce(t *testing.T) {
	c := assert.NewAborting(t)
	loc := toronto(t)
	spec, err := Parse("30 1 * * *", "America/Toronto")
	c.NoError(err, `Parse("30 1 * * *")`)

	after := at(loc, 2026, time.October, 31, 12, 0)

	first := spec.Next(after)
	wantFirst := at(loc, 2026, time.November, 1, 1, 30)
	c.True(first.Equal(wantFirst), "first fire = %s, want %s", first, wantFirst)
	_, firstOff := first.In(loc).Zone()
	c.Eq(-4*60*60, firstOff, "first fire is the EDT 01:30")

	second := spec.Next(first)
	wantSecond := at(loc, 2026, time.November, 2, 1, 30)
	c.True(second.Equal(wantSecond), "second fire = %s, want %s (not the repeated 01:30 EST of 11-01)", second, wantSecond)
	_, secondOff := second.In(loc).Zone()
	c.Eq(-5*60*60, secondOff, "second fire is the EST 01:30")
}

// TestNextNineAmStaysNineAcrossDST pins the wall-clock meaning of a daily
// job: "0 9 * * *" in Toronto is 9am local every day, EDT or EST, across
// the 2026-03-08 spring-forward.
func TestNextNineAmStaysNineAcrossDST(t *testing.T) {
	c := assert.NewAborting(t)
	loc := toronto(t)
	spec, err := Parse("0 9 * * *", "America/Toronto")
	c.NoError(err, `Parse("0 9 * * *")`)

	after := at(loc, 2026, time.March, 6, 9, 0)
	wantDays := []struct {
		day  int
		hour int
	}{
		{7, 9},  // EST
		{8, 9},  // the spring-forward day: 9am EDT
		{9, 9},  // EDT
		{10, 9}, // EDT
	}
	for _, w := range wantDays {
		got := spec.Next(after)
		want := at(loc, 2026, time.March, w.day, w.hour, 0)
		c.True(got.Equal(want), "Next(%s) = %s, want %s", after, got, want)
		c.Eq(w.hour, got.In(loc).Hour(), "the fire stays at 9am wall clock")
		after = got
	}
}

// TestParseRefusesTimezonePrefix pins that an expression may not carry its
// own zone: "TZ=" anywhere in the expression (case-sensitive; covers
// "CRON_TZ=") is refused, because the job's zone is Job.TZ.
func TestParseRefusesTimezonePrefix(t *testing.T) {
	c := assert.NewAborting(t)
	for _, expr := range []string{"CRON_TZ=UTC 0 9 * * *", "TZ=UTC 0 9 * * *"} {
		_, err := Parse(expr, "America/Toronto")
		want := "cron expression must not carry a timezone prefix; the job's zone is America/Toronto"
		c.EqualError(err, want, `Parse(%q)`, expr)
	}
}

// TestParseRefusesDescriptorsAndSixFields pins the 5-field grammar: no
// descriptors (@hourly, @every) and no seconds field.
func TestParseRefusesDescriptorsAndSixFields(t *testing.T) {
	c := assert.NewAborting(t)
	for _, expr := range []string{"@hourly", "@every 5m", "0 0 9 * * *"} {
		_, err := Parse(expr, "America/Toronto")
		c.Error(err, `Parse(%q) must be refused`, expr)
		c.StrContains(err.Error(), `invalid cron expression "`+expr+`"`, "the parse error names the expression")
	}
}

// TestParseRefusesEmptyAndLocalZone pins the zone rules: an empty zone and
// Go's "Local" pseudo-zone are refused before any parsing happens.
func TestParseRefusesEmptyAndLocalZone(t *testing.T) {
	c := assert.NewAborting(t)
	_, err := Parse("0 9 * * *", "")
	c.EqualError(err, "timezone must not be empty", `Parse with an empty zone`)
	_, err = Parse("0 9 * * *", "Local")
	c.EqualError(err, `timezone "Local" is not an IANA name`, `Parse with the Local pseudo-zone`)
}

// TestParseAcceptsStandardExpressions pins that the common shapes parse and
// evaluate in the given zone.
func TestParseAcceptsStandardExpressions(t *testing.T) {
	c := assert.NewAborting(t)
	for _, expr := range []string{
		"*/5 * * * *",
		"0 9 * * *",
		"30 1 * * *",
		"0 9 * * mon-fri",
		"0 9 1,15 * *",
	} {
		spec, err := Parse(expr, "UTC")
		c.NoError(err, `Parse(%q, UTC)`, expr)
		c.NotNil(spec, "Parse(%q) returned a spec", expr)
	}
}

// TestCheckMinGap pins the gap rule over gapSamples consecutive
// occurrences: closer than MinGap is refused, exactly MinGap is allowed,
// and an expression that never fires is ErrNeverFires.
func TestCheckMinGap(t *testing.T) {
	c := assert.NewAborting(t)
	loc := toronto(t)
	from := at(loc, 2026, time.March, 9, 0, 0)

	every5, err := Parse("*/5 * * * *", "America/Toronto")
	c.NoError(err, "Parse every5")
	c.NoError(every5.CheckMinGap(from, MinGap), "*/5 is exactly MinGap apart")

	every4, err := Parse("*/4 * * * *", "America/Toronto")
	c.NoError(err, "Parse every4")
	err = every4.CheckMinGap(from, MinGap)
	c.Error(err, "*/4 is 4m apart: refused")
	c.StrContains(err.Error(), "4m0s", "the refusal names the gap")

	burst, err := Parse("0,1,2 9 * * *", "America/Toronto")
	c.NoError(err, "Parse burst")
	err = burst.CheckMinGap(from, MinGap)
	c.Error(err, "a 1-minute burst: refused")
	c.StrContains(err.Error(), "1m0s", "the refusal names the gap")

	daily, err := Parse("0 9 * * *", "America/Toronto")
	c.NoError(err, "Parse daily")
	c.NoError(daily.CheckMinGap(from, MinGap), "daily is 24h apart")

	never, err := Parse("0 0 30 2 *", "America/Toronto")
	c.NoError(err, "Parse feb 30")
	err = never.CheckMinGap(from, MinGap)
	c.ErrorIs(err, ErrNeverFires, "Feb 30 never fires")
}

// TestCountThrough pins the fire-catch-up count: occurrences in (due, now],
// stopping at limit.
func TestCountThrough(t *testing.T) {
	c := assert.NewAborting(t)
	loc := toronto(t)
	spec, err := Parse("*/5 * * * *", "America/Toronto")
	c.NoError(err, "Parse */5")

	due := at(loc, 2026, time.March, 9, 10, 0)
	now := at(loc, 2026, time.March, 9, 10, 22)

	count, last := spec.CountThrough(due, now, gapSamples)
	c.Eq(4, count, "occurrences in (10:00, 10:22]")
	wantLast := at(loc, 2026, time.March, 9, 10, 20)
	c.True(last.Equal(wantLast), "last = %s, want %s", last, wantLast)

	count, last = spec.CountThrough(due, now, 2)
	c.Eq(2, count, "the limit stops the count")
	wantLast2 := at(loc, 2026, time.March, 9, 10, 10)
	c.True(last.Equal(wantLast2), "last under the limit = %s, want %s", last, wantLast2)

	// Nothing due: count 0 and a zero last.
	count, last = spec.CountThrough(now, due, gapSamples)
	c.Eq(0, count, "an empty window counts 0")
	c.True(last.IsZero(), "an empty window has a zero last")
}

// TestClampWakeup pins the wakeup delay clamp into [MinGap, WakeupMax].
func TestClampWakeup(t *testing.T) {
	c := assert.NewAborting(t)
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, MinGap},
		{-time.Hour, MinGap},
		{299 * time.Second, MinGap},
		{300 * time.Second, MinGap}, // exactly MinGap stays
		{301 * time.Second, 301 * time.Second},
		{time.Hour, time.Hour}, // exactly WakeupMax stays
		{2 * time.Hour, WakeupMax},
	}
	for _, tc := range cases {
		c.Eq(tc.want, ClampWakeup(tc.in), "ClampWakeup(%s)", tc.in)
	}
}

// TestNewID pins the id shape: sch_ + 8 lowercase unpadded base32 chars.
func TestNewID(t *testing.T) {
	c := assert.NewAborting(t)
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		id, err := NewID()
		c.NoError(err, "NewID")
		c.Len(id, 4+8, "id length")
		c.Eq("sch_", id[:4], "id prefix")
		for _, r := range id[4:] {
			lower := (r >= 'a' && r <= 'z') || (r >= '2' && r <= '7')
			c.True(lower, "id char %q must be lowercase base32", r)
		}
		c.False(seen[id], "ids must not repeat")
		seen[id] = true
	}
}

// TestValidateTimezone pins the zone validation contract.
func TestValidateTimezone(t *testing.T) {
	c := assert.NewAborting(t)
	c.NoError(ValidateTimezone("UTC"), "UTC")
	c.NoError(ValidateTimezone("America/Toronto"), "America/Toronto")
	c.NoError(ValidateTimezone("Europe/Paris"), "Europe/Paris")

	err := ValidateTimezone("")
	c.EqualError(err, "timezone must not be empty", "empty zone")
	err = ValidateTimezone("Local")
	c.EqualError(err, `timezone "Local" is not an IANA name`, "Local pseudo-zone")
	err = ValidateTimezone("Mars/Phobos")
	c.Error(err, "unknown zone")
	c.StrContains(err.Error(), `unknown timezone "Mars/Phobos"`, "wraps the LoadLocation error")
}
