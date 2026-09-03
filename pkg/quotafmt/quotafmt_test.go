// SPDX-License-Identifier: Apache-2.0

package quotafmt

import "testing"

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
			if got := Utilization(c.v); got != c.want {
				t.Errorf("Utilization(%v) = %q, want %q", c.v, got, c.want)
			}
		})
	}
}
