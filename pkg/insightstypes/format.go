// SPDX-License-Identifier: Apache-2.0

package insightstypes

import (
	"fmt"
	"strconv"
)

// CompactTokens renders an integer count as a short, human-readable magnitude:
// "16.5K", "144.8M", "2.0B". Counts under 1,000 print exactly; larger counts
// use a base-1000 suffix with one decimal. A magnitude whose one-decimal value
// rounds to 1000 is promoted to the next unit, so 999,900 reads "1.0M" rather
// than "1000.0K".
//
// The suffixes are K/M/B/T (thousand/million/billion/trillion) — how people
// actually say a count out loud — not the SI/byte-prefix K/M/G/T
// (kilo/mega/giga/tera). These are token and turn counts, not bytes, and
// "3.2G tokens" reads as a unit mismatch where "3.2B tokens" doesn't.
//
// This is the single source of truth for how every renderer formats token and
// turn counts, so the daemon-side and client-side surfaces stay byte-identical.
//
// go-humanize's SI/ComputeSI do compact large numbers, but not into this
// shape: they use lowercase SI prefixes with the same K/M/G/T mismatch noted
// above, format with full significant-digit precision rather than a fixed
// one decimal (so table columns wouldn't line up), and put a space before the
// unit. Getting from "16.528 k" to "16.5K" means overriding nearly everything
// SI does, at which point a direct implementation is simpler than a wrapper.
func CompactTokens(n int64) string {
	if n < 0 {
		return "-" + CompactTokens(-n)
	}
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	const suffixes = "KMBT"
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
