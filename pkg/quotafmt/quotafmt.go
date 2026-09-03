// SPDX-License-Identifier: Apache-2.0

// Package quotafmt formats Anthropic subscription rate-limit values for
// display. Split out of pkg/quota (which links pgx via its store) so
// cmd/rafiki — which must link zero postgres packages, enforced by
// TestClientDoesNotLinkPostgres — can share it with pkg/tui and pkg/fundi/tools
// without dragging pgx into the client binary.
package quotafmt

import "fmt"

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
