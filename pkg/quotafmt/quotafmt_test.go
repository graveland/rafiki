// SPDX-License-Identifier: Apache-2.0

package quotafmt

import (
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestUtilization(t *testing.T) {
	frac := 0.42
	large := 3.5

	cases := []struct {
		name string
		v    *float64
		want string
	}{
		{"nil is unknown", nil, "—"},
		{"fraction renders as percentage", &frac, "42%"},
		{"value above 1 renders as-is", &large, "3.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Utilization(c.v)
			assert.NewCollecting(t).Eq(c.want, got, "Utilization(%v) = %q, want", c.v, got)
		})
	}
}

func TestUntil(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return now.Add(d) }

	cases := []struct {
		name  string
		reset time.Time
		want  string
	}{
		{"sub-minute rounds down", at(30 * time.Second), "<1m"},
		{"minutes", at(45 * time.Minute), "45m"},
		{"hours and minutes", at(3*time.Hour + 4*time.Minute), "3h4m"},
		{"whole hours keep the zero minutes", at(2 * time.Hour), "2h0m"},
		{"days and hours", at(50 * time.Hour), "2d2h"},
		{"exactly now is past", now, ""},
		{"past is empty", at(-time.Hour), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Until(c.reset, now)
			assert.NewCollecting(t).Eq(c.want, got, "Until(%v) = %q, want", c.reset, got)
		})
	}
}
