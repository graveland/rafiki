package main

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/multigres/testkit/assert"
)

// The STARTED column's absolute stamp is hard to read at a glance, so the cell
// carries how long ago it was.
func TestStartedCellSuffixesARelativeAge(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"seconds", 20 * time.Second, "(just now)"},
		{"a minute", time.Minute, "(1m ago)"},
		{"minutes", 5*time.Minute + 40*time.Second, "(5m ago)"},
		{"an hour", time.Hour, "(1h ago)"},
		{"hours", 3*time.Hour + 59*time.Minute, "(3h ago)"},
		{"a day", 24 * time.Hour, "(1d ago)"},
		{"days", 49 * time.Hour, "(2d ago)"},
		// A started_at from a skewed clock is in the future; "-3m ago" would be
		// nonsense, and the timestamp itself is still shown.
		{"future", -3 * time.Minute, "(just now)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			started := now.Add(-tc.ago)
			got := startedCell(timestamppb.New(started), now)
			want := started.Format("2006-01-02 15:04") + " " + tc.want
			assert.NewCollecting(t).Eq(want, got, "cell")
		})
	}
}

func TestStartedCellWithoutATimestampIsADash(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Now()

	ck.Eq("-", startedCell(nil, now), "nil timestamp")
	ck.Eq("-", startedCell(timestamppb.New(time.Time{}), now), "zero timestamp")
}
