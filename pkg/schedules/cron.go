// SPDX-License-Identifier: Apache-2.0

package schedules

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// fiveFieldParser is the standard 5-field cron grammar, with no
// cron.Descriptor: "@hourly"/"@every" are refused and 6-field input is
// refused. It still honours a leading "TZ=" or "CRON_TZ=" prefix — which Parse
// uses itself (see below) and refuses in caller input.
var fiveFieldParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Spec is a parsed 5-field cron expression pinned to one IANA zone. The
// expression and the zone are inseparable: every occurrence of a Spec is a
// wall-clock time in Spec's zone, which is what makes "9am" mean the user's
// 9am across DST shifts.
type Spec struct {
	expr string
	tz   string
	loc  *time.Location
	s    cron.Schedule
}

// Parse parses a standard 5-field expression evaluated in tz.
//
// The expression must not carry its own timezone: a "TZ=" or "CRON_TZ="
// prefix (case-sensitive) is refused, because the job's zone is the Job.TZ
// column, not the expression's. Descriptors (@hourly, @every) and 6-field
// (seconds) expressions are refused by the parser.
//
// Parse then evaluates the grammar with the zone spelled the one way the
// 5-field parser accepts it — as a "CRON_TZ=" prefix on the string it parses
// — so the returned Spec fires wall-clock times in tz.
func Parse(expr, tz string) (*Spec, error) {
	if err := ValidateTimezone(tz); err != nil {
		return nil, err
	}
	if strings.Contains(expr, "TZ=") {
		return nil, fmt.Errorf("cron expression must not carry a timezone prefix; the job's zone is %s", tz)
	}
	sched, err := fiveFieldParser.Parse("CRON_TZ=" + tz + " " + expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", tz, err)
	}
	return &Spec{expr: expr, tz: tz, loc: loc, s: sched}, nil
}

// Next returns the first occurrence strictly after `after`, skipping a
// candidate whose wall clock in the spec's zone (date, hour, minute) equals
// after's — the fall-back rule. Zero Time when there is none.
//
// robfig/cron v3.0.1 walks absolute instants, so a wall clock that does not
// exist (spring-forward's missing 02:30) is skipped naturally, but a wall
// clock that happens TWICE (fall-back's two 01:30s) fires twice: measured in
// America/Toronto, it otherwise fires "30 1 * * *" twice on 2026-11-01
// (01:30 EDT then 01:30 EST). Comparing the candidate's wall clock to
// after's and stepping once more fires the repeated wall clock once.
func (s *Spec) Next(after time.Time) time.Time {
	c := s.s.Next(after)
	if c.IsZero() {
		return c
	}
	if c.In(s.loc).Format("2006-01-02 15:04") == after.In(s.loc).Format("2006-01-02 15:04") {
		c = s.s.Next(c)
	}
	return c
}

// CheckMinGap refuses the expression when any two of the gapSamples
// consecutive occurrences after `from` are closer than gap. Exactly gap
// apart is allowed. An expression that never fires is ErrNeverFires.
func (s *Spec) CheckMinGap(from time.Time, gap time.Duration) error {
	prev := s.Next(from)
	if prev.IsZero() {
		return ErrNeverFires
	}
	for i := 1; i < gapSamples; i++ {
		n := s.Next(prev)
		if n.IsZero() {
			break // fewer samples than gapSamples is fine
		}
		if n.Sub(prev) < gap {
			return fmt.Errorf("cron expression %q fires %s apart (%s then %s); the minimum is %s",
				s.expr, n.Sub(prev), prev.In(s.loc).Format(time.RFC3339), n.In(s.loc).Format(time.RFC3339), gap)
		}
		prev = n
	}
	return nil
}

// CountThrough counts occurrences in (due, now], stopping at limit; last is
// the latest one counted (zero when count is 0).
func (s *Spec) CountThrough(due, now time.Time, limit int) (count int, last time.Time) {
	t := s.Next(due)
	for !t.IsZero() && !t.After(now) && count < limit {
		count++
		last = t
		t = s.Next(t)
	}
	return count, last
}

// ClampWakeup clamps d into [MinGap, WakeupMax].
func ClampWakeup(d time.Duration) time.Duration {
	if d < MinGap {
		return MinGap
	}
	if d > WakeupMax {
		return WakeupMax
	}
	return d
}
