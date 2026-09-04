// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelquery"
)

// modelField names a column that can be sorted or bounded.
//
// It is an alias for modelquery.Field: the ordering and absence semantics are
// shared with the daemon's agent_models tool, and only the presentation --
// which columns are pinned, what their headers say, the named stops below --
// is the picker's own.
type modelField = modelquery.Field

const (
	colModel        = modelquery.FieldModel
	colCtx          = modelquery.FieldContext
	colIn           = modelquery.FieldPromptUSD
	colOut          = modelquery.FieldCompletionUSD
	colCache        = modelquery.FieldCacheReadUSD
	colMaxOut       = modelquery.FieldMaxCompletion
	colAge          = modelquery.FieldAge
	colIntel        = modelquery.FieldIntelligence
	colCode         = modelquery.FieldCoding
	colAgentic      = modelquery.FieldAgentic
	modelFieldCount = modelquery.FieldCount
)

// pinnedField reports whether the field already has a column in the list, so
// the sort does not need to add one. Sorting by something invisible is a list
// that reorders for no visible reason.
//
// A function rather than a method because modelField is an alias for a type
// this package does not own.
func pinnedField(f modelField) bool {
	switch f {
	case colModel, colCtx, colIn, colOut:
		return true
	}
	return false
}

// headerFor returns the column title and width for a field that is not pinned.
func headerFor(f modelField) (string, int) {
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

// query maps this bound's stop indices onto the shared constraint type.
//
// A row the catalog cannot answer for is ADMITTED by a bound, never rejected.
// That is the same rule Tools and Vision follow, and it matters most here:
// every locally-served model has no price, no context and no score, so a bound
// that rejected unknowns would silently empty the local fleet out of every
// filtered list.
//
// The "scored" stop is the ONE exception, and it is the only way to ask for
// "benchmarked models only" -- a numeric minimum cannot express it, because
// "intel >= 55" admits unscored rows by the rule above
// (TestBoundsAdmitModelsTheCatalogCannotAnswerFor pins exactly that). The stop
// is declared special: true precisely because it is a presence predicate
// rather than a threshold. It previously mapped to no constraint at all: the
// old admits() tested "value absent" before it reached the scored branch, so
// the stop filtered nothing while the panel displayed a constraint -- worse
// than having no control, because it misreported.
func (b bound) query(f modelField) modelquery.Bound {
	mins, maxs := minStops(f), maxStops(f)
	var out modelquery.Bound
	if b.minIx > 0 && b.minIx < len(mins) {
		s := mins[b.minIx]
		switch {
		case s.paidOnly:
			out.PaidOnly = true
		case s.special && s.label == "scored":
			out.RequirePresent = true
		default:
			v := s.value
			out.Min = &v
		}
	}
	if b.maxIx > 0 && b.maxIx < len(maxs) {
		v := maxs[b.maxIx].value
		out.Max = &v
	}
	return out
}

// admits reports whether a row satisfies this field's bounds. A row the
// catalog cannot answer for is ADMITTED, never rejected -- see modelquery.
func (b bound) admits(f modelField, r *rafikiv1.ModelRow) bool {
	return b.query(f).Admits(f, r)
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

// sortModels orders rows by every key in turn, the first that separates two
// rows deciding. The id is the final tiebreak so the order is total and a
// re-render never reshuffles equal rows.
// sortModels orders rows by every key in turn. The presence rule -- an absent
// value sorts last in BOTH directions -- lives in modelquery.Sort.
func sortModels(rows []*rafikiv1.ModelRow, keys []sortKey) {
	mq := make([]modelquery.SortKey, 0, len(keys))
	for _, k := range keys {
		mq = append(mq, modelquery.SortKey{Field: k.field, Desc: k.desc})
	}
	modelquery.Sort(rows, mq)
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
		if pinnedField(k.field) || seen[k.field] {
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
// biggerIsBetter says which end of a field you usually want first. Context,
// scores and output limits read best descending; prices and names ascending.
func biggerIsBetter(f modelField) bool {
	return modelquery.BiggerIsBetter(f)
}
