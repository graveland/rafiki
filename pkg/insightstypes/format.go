// SPDX-License-Identifier: Apache-2.0

package insightstypes

import (
	"fmt"
	"strconv"
)

// CompactTokens renders an integer count as a short, human-readable magnitude:
// "16.5K", "144.8M", "2.0G". Counts under 1,000 print exactly; larger counts
// use a base-1000 suffix with one decimal. A magnitude whose one-decimal value
// rounds to 1000 is promoted to the next unit, so 999,900 reads "1.0M" rather
// than "1000.0K".
//
// This is the single source of truth for how every renderer formats token and
// turn counts, so the daemon-side and client-side surfaces stay byte-identical.
// go-humanize has no compact-count helper (its Bytes/IBytes are 1024-based and
// carry a unit suffix), so the compacting lives here.
func CompactTokens(n int64) string {
	if n < 0 {
		return "-" + CompactTokens(-n)
	}
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	const suffixes = "KMGTP"
	var factor int64 = 1000
	unit := 0
	for n >= factor*1000 && unit < len(suffixes)-1 {
		factor *= 1000
		unit++
	}
	v := float64(n) / float64(factor)
	// A value that rounds to 1000.0 belongs to the next magnitude up.
	if v >= 999.5 && unit < len(suffixes)-1 {
		factor *= 1000
		unit++
		v = float64(n) / float64(factor)
	}
	return fmt.Sprintf("%.1f%s", v, string(suffixes[unit]))
}
