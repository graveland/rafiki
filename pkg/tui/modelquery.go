// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"sort"
	"strings"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// modelField names a column that can be sorted or bounded.
//
// One enum for both, because they are the same set: anything worth ordering by
// is worth constraining, and the dialog puts a sort cell and a bound cell
// against the same column name.
type modelField int

const (
	colModel modelField = iota
	colCtx
	colIn
	colOut
	colCache
	colMaxOut
	colAge
	colIntel
	colCode
	colAgentic
	modelFieldCount
)

func (f modelField) String() string {
	switch f {
	case colModel:
		return "model"
	case colCtx:
		return "ctx"
	case colIn:
		return "in$"
	case colOut:
		return "out$"
	case colCache:
		return "cache"
	case colMaxOut:
		return "max out"
	case colAge:
		return "age"
	case colIntel:
		return "intel"
	case colCode:
		return "code"
	case colAgentic:
		return "agentic"
	}
	return "?"
}

// pinned reports whether the field already has a column in the list, so the
// sort does not need to add one. Sorting by something invisible is a list that
// reorders for no visible reason.
func (f modelField) pinned() bool {
	switch f {
	case colModel, colCtx, colIn, colOut:
		return true
	}
	return false
}

func (f modelField) header() (string, int) {
	switch f {
	case colCache:
		return "CACHE", 9
	case colMaxOut:
		return "MAXOUT", 9
	case colAge:
		return "AGE", 7
	case colIntel:
		return "INTEL", 7
	case colCode:
		return "CODE", 7
	case colAgentic:
		return "AGENTIC", 9
	}
	return "", 0
}

// value returns the field's numeric value for one row. ok=false means the
// catalog has no answer -- NOT zero. Every locally-served model answers false
// for nearly all of these.
func (f modelField) value(r *rafikiv1.ModelRow) (float64, bool) {
	switch f {
	case colCtx:
		if r.ContextWindow == nil {
			return 0, false
		}
		return float64(*r.ContextWindow), true
	case colIn:
		if r.PromptUsd == nil {
			return 0, false
		}
		return *r.PromptUsd * 1e6, true // per MILLION, the unit bounds are in
	case colOut:
		if r.CompletionUsd == nil {
			return 0, false
		}
		return *r.CompletionUsd * 1e6, true
	case colCache:
		if r.CacheReadUsd == nil {
			return 0, false
		}
		return *r.CacheReadUsd * 1e6, true
	case colMaxOut:
		if r.MaxCompletionTokens == nil {
			return 0, false
		}
		return float64(*r.MaxCompletionTokens), true
	case colAge:
		if r.Created == nil || *r.Created <= 0 {
			return 0, false
		}
		return float64(*r.Created), true
	case colIntel:
		if r.IntelligenceIndex == nil {
			return 0, false
		}
		return *r.IntelligenceIndex, true
	case colCode:
		if r.CodingIndex == nil {
			return 0, false
		}
		return *r.CodingIndex, true
	case colAgentic:
		if r.AgenticIndex == nil {
			return 0, false
		}
		return *r.AgenticIndex, true
	}
	return 0, false
}

// sortKey is one ordering term. Keys apply in slice order, the first that
// separates two rows deciding.
type sortKey struct {
	field modelField
	desc  bool
}

// bound is a half-open constraint on a numeric field. Each side cycles through
// named stops rather than accepting typed numbers: a text field inside the
// dialog is another focus layer and another blurred-input trap, to express
// "≥173000", which nobody wants.
//
// A stop index of 0 always means UNSET on both sides.
type bound struct {
	minIx int
	maxIx int
}

// boundStop is one named threshold.
type boundStop struct {
	label string
	value float64
	// paidOnly marks the price stop that means "> free" rather than a
	// magnitude. Free is 4% of the catalog and is heavily rate-limited, so
	// excluding it is a real query -- and it cannot be a plain minimum,
	// because 7 free models pass a bare "<= $2".
	paidOnly bool
	// special stops carry their own wording and take NO comparison operator:
	// ">free" and "scored" are predicates, not thresholds, and "in$ ≥>free" is
	// nonsense. The unset "—" is deliberately NOT special -- it keeps its
	// operator so a field's two cells stay distinguishable when both are off.
	special bool
}

// minStops and maxStops are the cycling values per field, derived from the
// live catalog's actual distribution. Index 0 is always "any".
//
// A stop that excludes almost nothing is a stop that wastes a keypress: ctx
// >=32k was dropped because it only removes 17 of 421 models.
func minStops(f modelField) []boundStop {
	switch f {
	case colCtx:
		return []boundStop{{label: "—"}, {label: "128k", value: 128_000},
			{label: "200k", value: 200_000}, {label: "1M", value: 1_000_000}}
	case colIn, colOut, colCache:
		return []boundStop{{label: "—"}, {label: ">free", paidOnly: true, special: true}}
	case colIntel, colCode, colAgentic:
		return []boundStop{{label: "—"}, {label: "scored", value: 0, special: true},
			{label: "40", value: 40}, {label: "55", value: 55}}
	case colMaxOut:
		return []boundStop{{label: "—"}, {label: "16k", value: 16_000},
			{label: "64k", value: 64_000}}
	}
	return []boundStop{{label: "—"}}
}

func maxStops(f modelField) []boundStop {
	switch f {
	case colIn, colOut:
		return []boundStop{{label: "—"}, {label: "$1", value: 1}, {label: "$2", value: 2},
			{label: "$5", value: 5}, {label: "$15", value: 15}}
	case colCache:
		return []boundStop{{label: "—"}, {label: "$1", value: 1}, {label: "$5", value: 5}}
	case colCtx:
		return []boundStop{{label: "—"}, {label: "200k", value: 200_000}}
	}
	return []boundStop{{label: "—"}}
}

// admits reports whether a row satisfies this field's bounds.
//
// A row the catalog cannot answer for is ADMITTED by a bound, never rejected.
// That is the same rule toolsKind and visionKind follow, and it matters most
// here: every locally-served model has no price, no context and no score, so a
// bound that rejected unknowns would silently empty the local fleet out of
// every filtered list.
func (b bound) admits(f modelField, r *rafikiv1.ModelRow) bool {
	mins, maxs := minStops(f), maxStops(f)
	v, ok := f.value(r)

	if b.minIx > 0 && b.minIx < len(mins) {
		s := mins[b.minIx]
		switch {
		case !ok:
			// unknown: admitted
		case s.paidOnly:
			if v <= 0 {
				return false
			}
		case s.special && s.label == "scored":
			// presence alone; ok is already true here
		case v < s.value:
			return false
		}
	}
	if b.maxIx > 0 && b.maxIx < len(maxs) {
		s := maxs[b.maxIx]
		if ok && v > s.value {
			return false
		}
	}
	return true
}

func (b bound) set() bool { return b.minIx > 0 || b.maxIx > 0 }

// label renders the bound as the dialog shows it.
func (b bound) label(f modelField) string {
	mins, maxs := minStops(f), maxStops(f)
	var parts []string
	if b.minIx > 0 && b.minIx < len(mins) {
		parts = append(parts, stopText(mins[b.minIx], "≥"))
	}
	if b.maxIx > 0 && b.maxIx < len(maxs) {
		parts = append(parts, stopText(maxs[b.maxIx], "≤"))
	}
	return strings.Join(parts, " ")
}

// stopText renders one stop with its comparison operator. A special stop is a
// predicate and carries its own wording, so it takes no operator.
func stopText(s boundStop, op string) string {
	if s.special {
		return s.label
	}
	return op + s.label
}

// compareField orders two rows on one field.
//
// It returns byPresence=true when the answer came from one side being ABSENT.
// The caller must NOT flip that verdict for a descending key -- an absent value
// is not "the largest", it is no answer, and it sorts last in both directions.
// Flipping it puts every unscored model at the top of "smartest descending",
// and 47 of the 104 models passing a typical query have no score at all.
//
// Two absent values TIE (0, false), which is what lets the next key decide.
func compareField(a, b *rafikiv1.ModelRow, f modelField) (order int, byPresence bool) {
	if f == colModel {
		return strings.Compare(a.GetId(), b.GetId()), false
	}
	av, aok := f.value(a)
	bv, bok := f.value(b)
	switch {
	case !aok && !bok:
		return 0, false
	case !aok:
		return +1, true
	case !bok:
		return -1, true
	case av < bv:
		return -1, false
	case av > bv:
		return +1, false
	}
	return 0, false
}

// sortModels orders rows by every key in turn, the first that separates two
// rows deciding. The id is the final tiebreak so the order is total and a
// re-render never reshuffles equal rows.
func sortModels(rows []*rafikiv1.ModelRow, keys []sortKey) {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			c, byPresence := compareField(rows[i], rows[j], k.field)
			if c == 0 {
				continue
			}
			// Presence is NOT subject to direction. Only a comparison between
			// two KNOWN values flips.
			if byPresence {
				return c < 0
			}
			if k.desc {
				c = -c
			}
			return c < 0
		}
		return rows[i].GetId() < rows[j].GetId()
	})
}

// summarizeKeys names the active ordering for a hint line.
func summarizeKeys(keys []sortKey) string {
	if len(keys) == 0 {
		return "unsorted"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		arrow := "↑"
		if k.desc {
			arrow = "↓"
		}
		parts = append(parts, k.field.String()+arrow)
	}
	return strings.Join(parts, " ")
}

// extraColumns are the sorted fields that have no pinned column, capped so a
// four-key sort cannot squeeze the model id off the row.
func extraColumns(keys []sortKey) []modelField {
	var out []modelField
	seen := map[modelField]bool{}
	for _, k := range keys {
		if k.field.pinned() || seen[k.field] {
			continue
		}
		seen[k.field] = true
		out = append(out, k.field)
		if len(out) == 2 {
			break
		}
	}
	return out
}

// cellFor renders one extra column's value.
func cellFor(r *rafikiv1.ModelRow, f modelField, now time.Time) string {
	switch f {
	case colCache:
		return priceCell(r.CacheReadUsd)
	case colMaxOut:
		return tokCell(r.MaxCompletionTokens)
	case colAge:
		return ageCell(r, now)
	case colIntel:
		return scoreCell(r.IntelligenceIndex)
	case colCode:
		return scoreCell(r.CodingIndex)
	case colAgentic:
		return scoreCell(r.AgenticIndex)
	}
	return ""
}

func boundsSummary(bounds map[modelField]bound) string {
	var parts []string
	for f := modelField(0); f < modelFieldCount; f++ {
		if b, ok := bounds[f]; ok && b.set() {
			parts = append(parts, f.String()+" "+b.label(f))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "  ")
}

// admitsBounds reports whether a row satisfies every constraint.
func admitsBounds(bounds map[modelField]bound, r *rafikiv1.ModelRow) bool {
	for f, b := range bounds {
		if !b.admits(f, r) {
			return false
		}
	}
	return true
}

// biggerIsBetter says which end of a field you usually want first. Context,
// scores and output limits read best descending; prices and names ascending.
func biggerIsBetter(f modelField) bool {
	switch f {
	case colCtx, colMaxOut, colIntel, colCode, colAgentic, colAge:
		return true
	}
	return false
}
