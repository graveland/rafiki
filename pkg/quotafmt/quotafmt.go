// SPDX-License-Identifier: Apache-2.0

// Package quotafmt formats Anthropic subscription rate-limit values for
// display. Split out of pkg/quota (which links pgx via its store) so
// cmd/rafiki — which must link zero postgres packages, enforced by
// TestClientDoesNotLinkPostgres — can share it with pkg/tui and pkg/fundi/tools
// without dragging pgx into the client binary.
package quotafmt

import (
	"fmt"
	"time"
)

// Utilization formats a rate-limit utilization value for display. A value at
// or below 1 is treated as a fraction and rendered as a percentage; anything
// larger is shown as-is. This convention is a guess — the exact wire format
// Anthropic uses for anthropic-ratelimit-unified-*-utilization is not pinned
// down against a live response — kept in ONE place so correcting the guess
// later means editing one function, not hunting down every renderer that
// copied it. nil (the header was absent or unparseable) renders as "—".
func Utilization(v *float64) string {
	if v == nil {
		return "—"
	}
	if *v <= 1.0 {
		return fmt.Sprintf("%.0f%%", *v*100)
	}
	return fmt.Sprintf("%.4g", *v)
}

// Until formats how long remains before a window resets, as the compact
// duration a status line has room for: "3h4m", "2d6h", "45m", "<1m".
//
// now is a parameter rather than time.Now() so the caller's clock — and in a
// renderer, the frame being drawn — decides freshness. A reset already in the
// past returns "", because it is not a countdown to display: the snapshot it
// came from predates the rollover, and printing "resets in 0m" would read as
// "any second now" rather than "this number is stale". Callers nil-check the
// pointer themselves — an absent header must stay distinguishable from an
// epoch-valued one, the same rule every other optional rate-limit field
// follows.
func Until(reset, now time.Time) string {
	d := reset.Sub(now)
	if d <= 0 {
		return ""
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
