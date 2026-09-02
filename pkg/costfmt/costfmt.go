// SPDX-License-Identifier: Apache-2.0

// Package costfmt renders a USD spend for display, optionally converting it
// through a client-chosen currency (pkg/clientstate.Currency).
//
// This is the single formatter for every client-side surface that shows a
// cost: cmd/rafiki's `list` table, pkg/tui's rail, and
// pkg/conversationview's stats rendering. Before this package existed each
// of those carried its own near-identical dollars()/fmtCost() function, and
// a currency toggle would have needed three synchronized edits.
package costfmt

import (
	"fmt"
	"math"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

// Format renders a USD amount for display, converting through cur when it
// carries a usable rate.
//
// Zero (or a negative amount, which should not occur but is handled the same
// way) renders as "-": a row of "$0.00" beside every idle agent is noise, and
// the wire's CostUSD is also nil for "not known", which callers collapse to
// 0 before reaching here -- so 0 already means "nothing to show" either way.
// Sub-cent amounts keep 4 decimals of precision to stay visible; everything
// else gets 2.
func Format(usd float64, cur *clientstate.Currency) string {
	amount, suffix := usd, ""
	if cur != nil && cur.Rate > 0 {
		amount = usd * cur.Rate
		if cur.Code != "" {
			suffix = " " + cur.Code
		}
	}

	switch {
	case amount == 0:
		return "-"
	case math.Abs(amount) < 0.01:
		return fmt.Sprintf("$%.4f%s", amount, suffix)
	default:
		return fmt.Sprintf("$%.2f%s", amount, suffix)
	}
}
