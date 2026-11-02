// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"log/slog"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// newSeededCatalog builds a catalog that never fetches, seeded with entries.
func newSeededCatalog(entries ...CatalogEntry) *ModelCatalog {
	c := NewModelCatalog(nil, time.Minute, slog.New(slog.DiscardHandler))
	c.SeedForTest(entries)
	return c
}

func TestLatestTier(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, tc := range []struct {
		vendor Vendor
		local  string
		tier   string
		ok     bool
	}{
		{VendorAnthropic, "opus-latest", "opus", true},
		{VendorAnthropic, "claude-opus-latest", "opus", true},
		// A concrete versioned id is not a tier name.
		{VendorAnthropic, "claude-3-5-sonnet-latest", "", false},
		{VendorAnthropic, "opus", "", false},
		// Nothing between the name's ends but the vendor's own prefix stem.
		{VendorAnthropic, "claude-latest", "", false},
		{VendorOpenAI, "gpt-terra-latest", "terra", true},
		// OpenAI names require the "gpt-" prefix.
		{VendorOpenAI, "terra-latest", "", false},
		// A concrete model id is not a latest name.
		{VendorOpenAI, "gpt-5.4-mini", "", false},
		{VendorOpenAI, "gpt-latest", "", false},
		{Vendor("none"), "gpt-terra-latest", "", false},
	} {
		tier, ok := LatestTier(tc.vendor, tc.local)
		ck.Eq(tc.ok, ok, "LatestTier(%q, %q) ok", string(tc.vendor), tc.local)
		ck.Eq(tc.tier, tier, "LatestTier(%q, %q) tier", string(tc.vendor), tc.local)
	}
}

func TestResolveTierOpenAI(t *testing.T) {
	ck := assert.NewAborting(t)
	// Within each competing set the OLDER entry is seeded first, so the created
	// stamp (not row order) has to decide the winner: a first-match-wins scan
	// returns gpt-5.6-terra for "terra". The newer variants are seeded because
	// each exclusion rule must have something to exclude.
	c := newSeededCatalog(
		// terra: the older 5.6 wins nothing; 6.1 is newest. "-pro" is NEWER than
		// the winner, so only the end anchor keeps it out.
		CatalogEntry{ID: "openai/gpt-5.6-terra", Created: 3},
		CatalogEntry{ID: "openai/gpt-6.1-terra", Created: 5},
		CatalogEntry{ID: "openai/gpt-6.1-terra:batch", Created: 5},
		CatalogEntry{ID: "openai/gpt-6.1-terra-pro", Created: 6},
		CatalogEntry{ID: "openai/gpt-6.1-sol", Created: 4},
		// mini: the versioned mini is the oldest. gpt-audio-mini is NEWER than it,
		// so only the numeric-version clause keeps it out of the mini tier.
		CatalogEntry{ID: "openai/gpt-5.4-mini", Created: 2},
		CatalogEntry{ID: "openai/gpt-5.1-codex-mini", Created: 8},
		CatalogEntry{ID: "openai/gpt-audio-mini", Created: 9},
		// A real "-pro" model, NEWER than the terra winner: it would be pinned by
		// "pro" if a shape-valid tier word were enough to resolve.
		CatalogEntry{ID: "openai/gpt-5-pro", Created: 7},
		CatalogEntry{ID: "openai/gpt-4o", Created: 1},
		// The alias rows that OFFER the tiers this catalog serves.
		CatalogEntry{ID: "~openai/gpt-terra-latest", Created: 1},
		CatalogEntry{ID: "~openai/gpt-sol-latest", Created: 1},
		CatalogEntry{ID: "~openai/gpt-mini-latest", Created: 1},
	)

	// Newest by created, not by id; the "-pro" variant (created 6) is excluded by
	// the end anchor and the ":batch" twin by the same anchor.
	direct, orID, ok := c.ResolveTier(VendorOpenAI, "terra")
	ck.True(ok, "terra should resolve")
	ck.Eq("gpt-6.1-terra", direct)
	ck.Eq("openai/gpt-6.1-terra", orID)

	direct, orID, ok = c.ResolveTier(VendorOpenAI, "sol")
	ck.True(ok, "sol should resolve")
	ck.Eq("gpt-6.1-sol", direct)
	ck.Eq("openai/gpt-6.1-sol", orID)

	// The numeric version before the tier keeps gpt-audio-mini (created 9) and
	// gpt-5.1-codex-mini out of the mini tier despite their newer stamps.
	direct, orID, ok = c.ResolveTier(VendorOpenAI, "mini")
	ck.True(ok, "mini should resolve")
	ck.Eq("gpt-5.4-mini", direct)
	ck.Eq("openai/gpt-5.4-mini", orID)

	_, _, ok = c.ResolveTier(VendorOpenAI, "astra")
	ck.False(ok, "astra has no catalog model")

	// gpt-5-pro exists and is newer than everything else, but no
	// ~openai/gpt-pro-latest alias offers the tier, so "pro" is not a tier.
	_, _, ok = c.ResolveTier(VendorOpenAI, "pro")
	ck.False(ok, "pro is a variant suffix no alias advertises")
}

// TestResolveTierOffersOnlyAdvertisedTiers pins the gate itself: the tier
// vocabulary comes from the catalog's "~" alias rows, so the same model is
// unresolvable as "pro" without the alias and resolvable with it. If this were a
// hardcoded ban on "-pro" the advertised case could not pass.
func TestResolveTierOffersOnlyAdvertisedTiers(t *testing.T) {
	ck := assert.NewAborting(t)

	unadvertised := newSeededCatalog(
		CatalogEntry{ID: "openai/gpt-5-pro", Created: 7},
	)
	_, _, ok := unadvertised.ResolveTier(VendorOpenAI, "pro")
	ck.False(ok, "an unadvertised tier must not resolve even when a model matches it")

	advertised := newSeededCatalog(
		CatalogEntry{ID: "openai/gpt-5-pro", Created: 7},
		CatalogEntry{ID: "~openai/gpt-pro-latest", Created: 1},
	)
	direct, orID, ok := advertised.ResolveTier(VendorOpenAI, "pro")
	ck.True(ok, "a tier the catalog advertises does resolve")
	ck.Eq("gpt-5-pro", direct)
	ck.Eq("openai/gpt-5-pro", orID)
}

func TestResolveTierAnthropicDelegates(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSeededCatalog(
		CatalogEntry{ID: "anthropic/claude-opus-4.8", Created: 2},
		CatalogEntry{ID: "anthropic/claude-opus-4.8-fast", Created: 3},
	)

	// Delegates to ResolveLatest, so the newer "-fast" variant stays excluded.
	direct, orID, ok := c.ResolveTier(VendorAnthropic, "opus")
	ck.True(ok, "opus should resolve")
	ck.Eq("claude-opus-4-8", direct)
	ck.Eq("anthropic/claude-opus-4.8", orID)
}

// TestResolveTierGuards covers the three refusals a lenient implementation
// reaches by accident: a shapeless tier (including the empty one, which the
// Anthropic arm's "anthropic/claude-" prefix would otherwise match against every
// Claude model), an unknown vendor, and a nil receiver.
func TestResolveTierGuards(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSeededCatalog(
		CatalogEntry{ID: "anthropic/claude-opus-4.8", Created: 2},
		CatalogEntry{ID: "openai/gpt-6.1-terra", Created: 5},
		CatalogEntry{ID: "~openai/gpt-terra-latest", Created: 1},
	)

	for _, tier := range []string{"", "Pro", "ter ra", "ter-ra", "terra9", "térra"} {
		_, _, ok := c.ResolveTier(VendorAnthropic, tier)
		ck.False(ok, "anthropic tier %q is not tier-shaped", tier)
		_, _, ok = c.ResolveTier(VendorOpenAI, tier)
		ck.False(ok, "openai tier %q is not tier-shaped", tier)
	}

	_, _, ok := c.ResolveTier(Vendor("none"), "terra")
	ck.False(ok, "unknown vendor resolves nothing")

	var nilCatalog *ModelCatalog
	_, _, ok = nilCatalog.ResolveTier(VendorOpenAI, "terra")
	ck.False(ok, "nil receiver resolves nothing (openai arm)")
	_, _, ok = nilCatalog.ResolveTier(VendorAnthropic, "opus")
	ck.False(ok, "nil receiver resolves nothing (anthropic arm)")
}

// TestResolveTierColdCatalog pins the documented failure mode: a catalog that
// has fetched nothing advertises no tiers and resolves none, rather than
// panicking inside the Anthropic arm or matching an empty snapshot.
func TestResolveTierColdCatalog(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSeededCatalog() // zero models, stamped fresh: the cold-cache shape

	_, _, ok := c.ResolveTier(VendorOpenAI, "terra")
	ck.False(ok, "cold catalog resolves no openai tier")
	_, _, ok = c.ResolveTier(VendorAnthropic, "opus")
	ck.False(ok, "cold catalog resolves no anthropic tier")
	ck.Empty(c.LatestNames(VendorOpenAI), "cold catalog advertises nothing")
}

func TestLatestNames(t *testing.T) {
	ck := assert.NewAborting(t)
	c := newSeededCatalog(
		CatalogEntry{ID: "~openai/gpt-terra-latest"},
		CatalogEntry{ID: "~openai/gpt-mini-latest"},
		CatalogEntry{ID: "~openai/gpt-5-latest"}, // not a tier name
		CatalogEntry{ID: "~anthropic/claude-opus-latest"},
		CatalogEntry{ID: "~anthropic/claude-haiku-latest"},
	)

	ck.EqDeep([]string{"gpt-mini-latest", "gpt-terra-latest"}, c.LatestNames(VendorOpenAI))
	ck.EqDeep([]string{"haiku-latest", "opus-latest"}, c.LatestNames(VendorAnthropic))

	_, ok := vendorSpecs[Vendor("none")]
	ck.False(ok, "no spec for an unknown vendor")
	ck.Empty(c.LatestNames(Vendor("none")), "unknown vendor lists nothing")

	var nilCatalog *ModelCatalog
	ck.Empty(nilCatalog.LatestNames(VendorOpenAI), "nil receiver lists nothing")
}
