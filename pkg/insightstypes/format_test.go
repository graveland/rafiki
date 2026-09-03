// SPDX-License-Identifier: Apache-2.0

package insightstypes

import "testing"

func TestCompactTokens(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{17, "17"},
		{999, "999"},
		{1000, "1.0K"},
		{16528, "16.5K"},
		{144751240, "144.8M"},
		{5892236, "5.9M"},
		{2044479938, "2.0B"},
		{999900, "1.0M"}, // rounded magnitude promotion
		{1000000000, "1.0B"},
		{1234567890123, "1.2T"},
		{-1500, "-1.5K"},
	} {
		if got := CompactTokens(tc.in); got != tc.want {
			t.Errorf("CompactTokens(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
