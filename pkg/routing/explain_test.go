// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// explainRowMap indexes an ExplainRoutes result by (slug, quantization) so a
// test can assert one endpoint's verdict without depending on row order.
func explainRowMap(rows []ExplainRow) map[string]ExplainRow {
	m := make(map[string]ExplainRow, len(rows))
	for _, r := range rows {
		m[explainKey(r.Endpoint.Slug, r.Endpoint.Quantization)] = r
	}
	return m
}

// explainOrderedKeys returns the (slug, quantization) keys of rows in result
// order, so a test pins the predicted try-order exactly.
func explainOrderedKeys(rows []ExplainRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, explainKey(r.Endpoint.Slug, r.Endpoint.Quantization))
	}
	return out
}

// TestRouteExplainQuantFloorExcludesUnknown pins that a floor like quant=fp8+
// never admits an unlabelled host — it falls out of Quantizations() expanding
// the floor, and "unknown" is on no tier.
func TestRouteExplainQuantFloorExcludesUnknown(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Provider: "Alpha", Slug: "alpha", Quantization: "unknown", PromptPrice: 0.1},
		{Provider: "Beta", Slug: "beta", Quantization: "fp8", PromptPrice: 0.2},
	}
	rows := ExplainRoutes(eps, nil, Spec{Quant: []string{"fp8+"}}, nil)
	byKey := explainRowMap(rows)
	c.Eq(ExcludeQuant, byKey[explainKey("alpha", "unknown")].Reason, "unknown under a floor")
	c.Eq(false, byKey[explainKey("alpha", "unknown")].Eligible, "unknown under a floor is excluded")
	c.Eq(ExcludeNone, byKey[explainKey("beta", "fp8")].Reason, "fp8 under fp8+")
	c.Eq(true, byKey[explainKey("beta", "fp8")].Eligible, "fp8 under fp8+ is eligible")
}

// TestRouteExplainQuantListIncludesUnknown pins that an explicit list naming
// "unknown" is the one spelling that admits an unlabelled host — the floor rule
// must not be special-cased to reject it.
func TestRouteExplainQuantListIncludesUnknown(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Provider: "Alpha", Slug: "alpha", Quantization: "unknown", PromptPrice: 0.1},
		{Provider: "Beta", Slug: "beta", Quantization: "fp8", PromptPrice: 0.2},
		{Provider: "Gamma", Slug: "gamma", Quantization: "bf16", PromptPrice: 0.3},
	}
	rows := ExplainRoutes(eps, nil, Spec{Quant: []string{"unknown", "fp8"}}, nil)
	byKey := explainRowMap(rows)
	c.Eq(ExcludeNone, byKey[explainKey("alpha", "unknown")].Reason, "unknown named explicitly")
	c.Eq(true, byKey[explainKey("alpha", "unknown")].Eligible, "unknown named explicitly is eligible")
	c.Eq(ExcludeQuant, byKey[explainKey("gamma", "bf16")].Reason, "bf16 not in the list")
}

// TestRouteExplainOnlyBypassesIgnore pins that an only is an explicit routing
// decision and the guard's ignore list is not consulted for it.
func TestRouteExplainOnlyBypassesIgnore(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Provider: "Alpha", Slug: "alpha", Quantization: "fp8", PromptPrice: 0.1},
		{Provider: "Beta", Slug: "beta", Quantization: "fp8", PromptPrice: 0.2},
	}
	rows := ExplainRoutes(eps, nil, Spec{Only: []string{"alpha"}}, []string{"alpha"})
	byKey := explainRowMap(rows)
	c.Eq(ExcludeNone, byKey[explainKey("alpha", "fp8")].Reason, "only bypasses a ban on its own slug")
	c.Eq(true, byKey[explainKey("alpha", "fp8")].Eligible, "only provider is eligible despite the ban")
	c.Eq(ExcludeNotInOnly, byKey[explainKey("beta", "fp8")].Reason, "a provider outside only is excluded")
}

// TestRouteExplainIgnoreExcludesWhenNoOnly pins that with no only the guard's
// ignore list (bans and ejections) excludes a provider.
func TestRouteExplainIgnoreExcludesWhenNoOnly(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Provider: "Alpha", Slug: "alpha", Quantization: "fp8", PromptPrice: 0.1},
		{Provider: "Beta", Slug: "beta", Quantization: "fp8", PromptPrice: 0.2},
	}
	rows := ExplainRoutes(eps, nil, Spec{}, []string{"alpha"})
	byKey := explainRowMap(rows)
	c.Eq(ExcludeBanned, byKey[explainKey("alpha", "fp8")].Reason, "ignored slug is banned")
	c.Eq(false, byKey[explainKey("alpha", "fp8")].Eligible, "ignored slug is excluded")
	c.Eq(ExcludeNone, byKey[explainKey("beta", "fp8")].Reason, "unignored slug is eligible")
}

// TestRouteExplainReasonPrecedence pins the reason precedence: not-in-only,
// then banned, then quant. When several apply, only the highest is reported.
func TestRouteExplainReasonPrecedence(t *testing.T) {
	c := assert.NewAborting(t)

	// not-in-only beats banned: alpha is both outside only and ignored.
	eps := []Endpoint{
		{Slug: "alpha", Quantization: "unknown", PromptPrice: 0.1},
		{Slug: "beta", Quantization: "unknown", PromptPrice: 0.2},
	}
	rows := ExplainRoutes(eps, nil, Spec{Only: []string{"beta"}, Quant: []string{"fp8+"}}, []string{"alpha", "beta"})
	byKey := explainRowMap(rows)
	c.Eq(ExcludeNotInOnly, byKey[explainKey("alpha", "unknown")].Reason, "not-in-only beats banned")
	// quant applies to beta: it is inside only (so not banned) but fails the floor.
	c.Eq(ExcludeQuant, byKey[explainKey("beta", "unknown")].Reason, "quant applies to an in-only provider")

	// banned beats quant: gamma fails the floor and is ignored, no only set.
	eps2 := []Endpoint{{Slug: "gamma", Quantization: "unknown", PromptPrice: 0.1}}
	rows2 := ExplainRoutes(eps2, nil, Spec{Quant: []string{"fp8+"}}, []string{"gamma"})
	c.Eq(ExcludeBanned, explainRowMap(rows2)[explainKey("gamma", "unknown")].Reason, "banned beats quant")
}

// TestRouteExplainPriceOrderUnknownPriceLast pins the price sort: ascending,
// with an unknown price (-1) sorting last rather than first.
func TestRouteExplainPriceOrderUnknownPriceLast(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Slug: "alpha", Quantization: "fp8", PromptPrice: 0.5},
		{Slug: "beta", Quantization: "fp8", PromptPrice: -1},
		{Slug: "gamma", Quantization: "fp8", PromptPrice: 0.1},
	}
	rows := ExplainRoutes(eps, nil, Spec{}, nil)
	c.EqDeep([]string{
		explainKey("gamma", "fp8"),
		explainKey("alpha", "fp8"),
		explainKey("beta", "fp8"),
	}, explainOrderedKeys(rows), "price ascending, unknown price last")
}

// TestRouteExplainThroughputOrderAbsentLast pins the throughput sort:
// descending p50, with an absent value (nil stats or the -1 sentinel) sorting
// last rather than as the slowest-but-present.
func TestRouteExplainThroughputOrderAbsentLast(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Slug: "alpha", Quantization: "fp8"},
		{Slug: "beta", Quantization: "fp8"},
		{Slug: "gamma", Quantization: "fp8"},
		{Slug: "delta", Quantization: "fp8"}, // no stats row at all
	}
	stats := []EndpointStats{
		{ProviderSlug: "alpha", Quantization: "fp8", P50Throughput: 10},
		{ProviderSlug: "beta", Quantization: "fp8", P50Throughput: -1},
		{ProviderSlug: "gamma", Quantization: "fp8", P50Throughput: 50},
	}
	rows := ExplainRoutes(eps, stats, Spec{Sort: SortThroughput}, nil)
	c.EqDeep([]string{
		explainKey("gamma", "fp8"),
		explainKey("alpha", "fp8"),
		explainKey("beta", "fp8"),
		explainKey("delta", "fp8"),
	}, explainOrderedKeys(rows), "throughput descending, absent last")
}

// TestRouteExplainPreferredFirstThenSort pins the preferred block: preferred
// slugs come first in spec.Prefer order (price ascending within one provider),
// then the rest by the spec's sort.
func TestRouteExplainPreferredFirstThenSort(t *testing.T) {
	c := assert.NewAborting(t)

	eps := []Endpoint{
		{Slug: "alpha", Quantization: "fp8", PromptPrice: 0.9},
		{Slug: "beta", Quantization: "fp8", PromptPrice: 0.1},
		{Slug: "gamma", Quantization: "fp8", PromptPrice: 0.2},
	}
	rows := ExplainRoutes(eps, nil, Spec{Prefer: []string{"gamma", "alpha"}}, nil)
	c.EqDeep([]string{
		explainKey("gamma", "fp8"),
		explainKey("alpha", "fp8"),
		explainKey("beta", "fp8"),
	}, explainOrderedKeys(rows), "prefer order first, then price")

	// Two endpoints of one preferred provider: cheaper first, regardless of the
	// input order.
	eps2 := []Endpoint{
		{Slug: "alpha", Quantization: "fp8", PromptPrice: 0.5},
		{Slug: "alpha", Quantization: "mxfp8", PromptPrice: 0.2},
		{Slug: "beta", Quantization: "fp8", PromptPrice: 0.1},
	}
	rows2 := ExplainRoutes(eps2, nil, Spec{Prefer: []string{"alpha"}}, nil)
	c.EqDeep([]string{
		explainKey("alpha", "mxfp8"),
		explainKey("alpha", "fp8"),
		explainKey("beta", "fp8"),
	}, explainOrderedKeys(rows2), "within a preferred provider, price ascending")
}

// TestRouteExplainAmbiguousStatsKeyAttachesNothing pins that an ambiguous
// (slug, quantization) key — shared by more than one endpoint, or claimed by
// more than one stats row — attaches no stats to any of them.
func TestRouteExplainAmbiguousStatsKeyAttachesNothing(t *testing.T) {
	c := assert.NewAborting(t)

	// Two endpoints share the key: neither gets the stats.
	eps := []Endpoint{
		{Slug: "alpha", Quantization: "fp8", PromptPrice: 0.1},
		{Slug: "alpha", Quantization: "fp8", PromptPrice: 0.2},
		{Slug: "beta", Quantization: "fp8", PromptPrice: 0.3},
	}
	stats := []EndpointStats{
		{ProviderSlug: "alpha", Quantization: "fp8", P50Throughput: 10},
		{ProviderSlug: "beta", Quantization: "fp8", P50Throughput: 20},
	}
	rows := ExplainRoutes(eps, stats, Spec{}, nil)
	var alphaStats []*EndpointStats
	for _, r := range rows {
		if r.Endpoint.Slug == "alpha" {
			alphaStats = append(alphaStats, r.Stats)
		}
	}
	c.Eq(2, len(alphaStats), "both alpha endpoints present")
	for i, st := range alphaStats {
		c.Eq(true, st == nil, "ambiguous endpoint key attaches no stats (row %d)", i)
	}
	byKey := explainRowMap(rows)
	c.Eq(false, byKey[explainKey("beta", "fp8")].Stats == nil, "unambiguous key still attaches stats")

	// Two stats rows share the key: none attaches.
	eps2 := []Endpoint{{Slug: "gamma", Quantization: "fp8", PromptPrice: 0.1}}
	stats2 := []EndpointStats{
		{ProviderSlug: "gamma", Quantization: "fp8", P50Throughput: 10},
		{ProviderSlug: "gamma", Quantization: "fp8", P50Throughput: 90},
	}
	rows2 := ExplainRoutes(eps2, stats2, Spec{}, nil)
	c.Eq(1, len(rows2), "one gamma endpoint")
	c.Eq(true, rows2[0].Stats == nil, "ambiguous stats key attaches nothing")
}

// TestRouteExplainExcludedRowsFollowEligibleInInputOrder pins the result shape:
// every endpoint is present, eligible rows in predicted order first, then the
// excluded rows in their input order.
func TestRouteExplainExcludedRowsFollowEligibleInInputOrder(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Slug: "a", Quantization: "fp8", PromptPrice: 0.1},
		{Slug: "b", Quantization: "unknown", PromptPrice: 0.1},
		{Slug: "c", Quantization: "unknown", PromptPrice: 0.1},
		{Slug: "d", Quantization: "fp8", PromptPrice: 0.1},
	}
	rows := ExplainRoutes(eps, nil, Spec{Quant: []string{"fp8+"}}, nil)
	c.EqDeep([]string{
		explainKey("a", "fp8"),
		explainKey("d", "fp8"),
		explainKey("b", "unknown"),
		explainKey("c", "unknown"),
	}, explainOrderedKeys(rows), "eligible then excluded, in input order")
	c.Eq(4, len(rows), "every endpoint is kept")
	byKey := explainRowMap(rows)
	c.Eq(ExcludeQuant, byKey[explainKey("b", "unknown")].Reason, "b excluded")
	c.Eq(ExcludeQuant, byKey[explainKey("c", "unknown")].Reason, "c excluded")
}

// TestRouteExplainRanksAreContiguousFromOne pins that eligible rows carry
// 1-based, gap-free ranks in result order and excluded rows rank 0.
func TestRouteExplainRanksAreContiguousFromOne(t *testing.T) {
	c := assert.NewAborting(t)
	eps := []Endpoint{
		{Slug: "a", Quantization: "fp8", PromptPrice: 0.3},
		{Slug: "b", Quantization: "fp8", PromptPrice: 0.1},
		{Slug: "c", Quantization: "unknown", PromptPrice: 0.2},
		{Slug: "d", Quantization: "fp8", PromptPrice: 0.4},
	}
	rows := ExplainRoutes(eps, nil, Spec{Quant: []string{"fp8+"}}, nil)
	var ranks []int
	for _, r := range rows {
		if r.Eligible {
			ranks = append(ranks, r.Rank)
		} else {
			c.Eq(0, r.Rank, "excluded row has no rank")
		}
	}
	c.EqDeep([]int{1, 2, 3}, ranks, "eligible ranks are contiguous from one")
	c.EqDeep([]string{
		explainKey("b", "fp8"),
		explainKey("a", "fp8"),
		explainKey("d", "fp8"),
		explainKey("c", "unknown"),
	}, explainOrderedKeys(rows), "rank order matches price order")
}
