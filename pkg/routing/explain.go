// SPDX-License-Identifier: Apache-2.0

package routing

import "slices"

// ExcludeReason says why a row in an ExplainRoutes result is not eligible.
type ExcludeReason string

const (
	ExcludeNone      ExcludeReason = ""
	ExcludeQuant     ExcludeReason = "quant"       // quantization not in the spec's set
	ExcludeNotInOnly ExcludeReason = "not-in-only" // spec.Only is set and excludes this provider
	ExcludeBanned    ExcludeReason = "banned"      // slug is in the guard's ignore list (bans/ejections)
)

// ExplainRow is one endpoint's fate under a routing spec: whether it is
// eligible, why not when it isn't, the stats joined to it, and its predicted
// try-order position.
type ExplainRow struct {
	Endpoint  Endpoint
	Stats     *EndpointStats // nil when no stats matched
	Eligible  bool
	Reason    ExcludeReason
	Rank      int  // 1-based position among eligible rows; 0 when excluded
	Preferred bool // slug is in spec.Prefer
}

// ExplainRoutes applies spec to eps. ignore is the guard's merged ban/ejection
// slugs (IgnoredFor). stats may be nil. The result keeps every endpoint:
// eligible rows first in predicted try-order, then excluded rows in input
// order.
//
// data-policy (nodata/zdr) filters are applied by OpenRouter and are not
// reflected here.
//
// The ordering is a prediction of what OpenRouter will try: OpenRouter's live
// health-based fallback is not reproducible here.
func ExplainRoutes(eps []Endpoint, stats []EndpointStats, spec Spec, ignore []string) []ExplainRow {
	quants := spec.Quantizations()
	qset := make(map[string]bool, len(quants))
	for _, q := range quants {
		qset[q] = true
	}
	preferIdx := make(map[string]int, len(spec.Prefer))
	for i, slug := range spec.Prefer {
		if _, ok := preferIdx[slug]; !ok {
			preferIdx[slug] = i
		}
	}
	only := make(map[string]bool, len(spec.Only))
	for _, slug := range spec.Only {
		only[slug] = true
	}
	ign := make(map[string]bool, len(ignore))
	for _, slug := range ignore {
		ign[slug] = true
	}

	// A (slug, quantization) key that names more than one endpoint, or that more
	// than one stats row claims, is ambiguous: attaching a speed to either would
	// mis-attribute it, so no stats attach to any endpoint with that key.
	epsCount := make(map[string]int, len(eps))
	for _, ep := range eps {
		epsCount[explainKey(ep.Slug, ep.Quantization)]++
	}
	statsIdx := make(map[string][]int, len(stats))
	for i, st := range stats {
		k := explainKey(st.ProviderSlug, st.Quantization)
		statsIdx[k] = append(statsIdx[k], i)
	}

	var eligible, excluded []ExplainRow
	for _, ep := range eps {
		row := ExplainRow{Endpoint: ep}
		if _, ok := preferIdx[ep.Slug]; ok {
			row.Preferred = true
		}
		k := explainKey(ep.Slug, ep.Quantization)
		if epsCount[k] == 1 && len(statsIdx[k]) == 1 {
			row.Stats = &stats[statsIdx[k][0]]
		}
		switch {
		case len(spec.Only) > 0 && !only[ep.Slug]:
			row.Reason = ExcludeNotInOnly
		case len(spec.Only) == 0 && ign[ep.Slug]:
			row.Reason = ExcludeBanned
		case len(quants) > 0 && !qset[ep.Quantization]:
			row.Reason = ExcludeQuant
		}
		row.Eligible = row.Reason == ExcludeNone
		if row.Eligible {
			eligible = append(eligible, row)
		} else {
			excluded = append(excluded, row)
		}
	}

	mode := explainSortMode(spec.Sort)
	slices.SortStableFunc(eligible, func(a, b ExplainRow) int {
		return explainCompare(a, b, preferIdx, mode)
	})
	for i := range eligible {
		eligible[i].Rank = i + 1
	}
	return append(eligible, excluded...)
}

// explainKey is the join key between an Endpoint and an EndpointStats row:
// provider slug plus quantization. NUL cannot occur in either half.
func explainKey(slug, quant string) string { return slug + "\x00" + quant }

// explainSortMode reduces a spec Sort to the ordering it predicts. SortInherit
// (the zero value), SortBalanced and SortPrice all order by price: inherit and
// balanced mean "no opinion from this level", and price is the stable default.
func explainSortMode(s Sort) Sort {
	switch s {
	case SortThroughput:
		return SortThroughput
	case SortLatency:
		return SortLatency
	default:
		return SortPrice
	}
}

// explainCompare orders two eligible rows: preferred rows first (by their
// position in spec.Prefer, then by price ascending within one provider), then
// the rest by mode. Returning 0 keeps input order under SortStableFunc.
func explainCompare(a, b ExplainRow, preferIdx map[string]int, mode Sort) int {
	ai, aPref := preferIdx[a.Endpoint.Slug]
	bi, bPref := preferIdx[b.Endpoint.Slug]
	switch {
	case aPref && !bPref:
		return -1
	case !aPref && bPref:
		return 1
	case aPref && bPref:
		if ai != bi {
			return explainCmpInt(ai, bi)
		}
		return explainPriceAsc(a.Endpoint, b.Endpoint)
	}
	switch mode {
	case SortThroughput:
		return explainThroughputDesc(a.Stats, b.Stats)
	case SortLatency:
		return explainLatencyAsc(a.Stats, b.Stats)
	default:
		return explainPriceAsc(a.Endpoint, b.Endpoint)
	}
}

// explainPriceAsc orders by prompt price ascending, an unknown price (-1) last.
func explainPriceAsc(a, b Endpoint) int {
	ap, bp := a.PromptPrice, b.PromptPrice
	aAbs, bAbs := ap < 0, bp < 0
	switch {
	case aAbs && bAbs:
		return 0
	case aAbs:
		return 1
	case bAbs:
		return -1
	case ap < bp:
		return -1
	case ap > bp:
		return 1
	default:
		return 0
	}
}

// explainThroughputDesc orders by p50 throughput descending, an absent value
// (nil stats, or the -1 sentinel) last.
func explainThroughputDesc(a, b *EndpointStats) int {
	av, aAbs := explainThroughput(a)
	bv, bAbs := explainThroughput(b)
	switch {
	case aAbs && bAbs:
		return 0
	case aAbs:
		return 1
	case bAbs:
		return -1
	case av > bv:
		return -1
	case av < bv:
		return 1
	default:
		return 0
	}
}

// explainLatencyAsc orders by p50 latency ascending, an absent value last.
func explainLatencyAsc(a, b *EndpointStats) int {
	av, aAbs := explainLatency(a)
	bv, bAbs := explainLatency(b)
	switch {
	case aAbs && bAbs:
		return 0
	case aAbs:
		return 1
	case bAbs:
		return -1
	case av < bv:
		return -1
	case av > bv:
		return 1
	default:
		return 0
	}
}

func explainThroughput(st *EndpointStats) (float64, bool) {
	if st == nil || st.P50Throughput < 0 {
		return 0, true
	}
	return st.P50Throughput, false
}

func explainLatency(st *EndpointStats) (float64, bool) {
	if st == nil || st.P50LatencyMs < 0 {
		return 0, true
	}
	return st.P50LatencyMs, false
}

func explainCmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
