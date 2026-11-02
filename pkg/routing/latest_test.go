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
		// Nothing between the prefix and the suffix.
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
	c := newSeededCatalog(
		CatalogEntry{ID: "openai/gpt-6.1-terra", Created: 5},
		CatalogEntry{ID: "openai/gpt-6.1-terra-pro", Created: 6},
		CatalogEntry{ID: "openai/gpt-6.1-terra:batch", Created: 5},
		CatalogEntry{ID: "openai/gpt-5.6-terra", Created: 3},
		CatalogEntry{ID: "openai/gpt-6.1-sol", Created: 4},
		CatalogEntry{ID: "openai/gpt-5.4-mini", Created: 2},
		CatalogEntry{ID: "openai/gpt-audio-mini", Created: 9},
		CatalogEntry{ID: "openai/gpt-5.1-codex-mini", Created: 8},
		CatalogEntry{ID: "openai/gpt-4o", Created: 1},
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

	_, _, ok = c.ResolveTier(VendorOpenAI, "pro")
	ck.False(ok, "pro is a variant suffix, not a tier")
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
