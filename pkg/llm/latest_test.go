// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// latestFixtureSet is the registry the pinning tests resolve names against: one
// provider per vendor-bearing kind, plus a generic OpenAI-compatible one (no
// vendor), an OpenRouter gateway (no vendor), and a provider that DECLARES the
// latest name itself (so its declaration must win over any catalog pin).
func latestFixtureSet() *providers.Set {
	return &providers.Set{
		DefaultProvider: "work",
		Providers: map[string]providers.Provider{
			"gpt":        {Name: "gpt", Kind: providers.KindOpenAIResponses},
			"work":       {Name: "work", Kind: providers.KindAnthropic},
			"local":      {Name: "local", Kind: providers.KindOpenAI},
			"openrouter": {Name: "openrouter", Kind: providers.KindAnthropicOpenRouter},
			"declared": {Name: "declared", Kind: providers.KindOpenAIResponses,
				Models: map[string]providers.ModelDecl{"gpt-terra-latest": {ID: "gpt-pinned"}}},
		},
	}
}

// latestFixtureCatalog seeds the OpenRouter entries the pinning tests resolve
// against, including the "~" alias rows that OFFER the tiers.
func latestFixtureCatalog(t *testing.T) *routing.ModelCatalog {
	t.Helper()
	cat := routing.NewModelCatalog(nil, time.Hour, testLogger(t))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "openai/gpt-6.1-terra", Created: 5},
		{ID: "openai/gpt-6.1-terra-pro", Created: 6},
		{ID: "anthropic/claude-opus-4.8", Created: 3},
		{ID: "~openai/gpt-terra-latest", Created: 7},
		{ID: "~anthropic/claude-opus-latest", Created: 8},
	})
	return cat
}

func TestResolveProviderLatestPinsByProviderKind(t *testing.T) {
	set := latestFixtureSet()
	cat := latestFixtureCatalog(t)

	for _, tc := range []struct {
		name string
		set  *providers.Set
		cat  *routing.ModelCatalog
		in   string
		want string
		errs []string
	}{
		{name: "openai responses latest pins to the newest tier model", set: set, cat: cat,
			in: "gpt/gpt-terra-latest", want: "gpt/gpt-6.1-terra"},
		{name: "a routing spec rides along untouched", set: set, cat: cat,
			in: "gpt/gpt-terra-latest[sort=price]", want: "gpt/gpt-6.1-terra[sort=price]"},
		{name: "anthropic latest pins from the short name", set: set, cat: cat,
			in: "work/opus-latest", want: "work/claude-opus-4-8"},
		{name: "anthropic latest pins from the prefixed name", set: set, cat: cat,
			in: "work/claude-opus-latest", want: "work/claude-opus-4-8"},
		{name: "the anthropic/ marker is routing.ResolveModel's, not ours", set: set, cat: cat,
			in: "anthropic/opus-latest", want: "anthropic/opus-latest"},
		{name: "a kind with no vendor passes through", set: set, cat: cat,
			in: "local/foo-latest", want: "local/foo-latest"},
		{name: "an OpenRouter gateway passes through", set: set, cat: cat,
			in: "openrouter/~deepseek/deepseek-flash-latest", want: "openrouter/~deepseek/deepseek-flash-latest"},
		{name: "a name the provider declares itself is not pinned", set: set, cat: cat,
			in: "declared/gpt-terra-latest", want: "declared/gpt-terra-latest"},
		{name: "a concrete id is not a latest name", set: set, cat: cat,
			in: "gpt/gpt-6.1-terra", want: "gpt/gpt-6.1-terra"},
		{name: "an unknown provider passes through", set: set, cat: cat,
			in: "nosuch/gpt-terra-latest", want: "nosuch/gpt-terra-latest"},
		{name: "a slash-free id is an alias or an error elsewhere, never ours", set: set, cat: cat,
			in: "gpt-terra-latest", want: "gpt-terra-latest"},
		{name: "a tier the catalog does not offer is an error naming both", set: set, cat: cat,
			in: "gpt/gpt-nova-latest", errs: []string{"nova", "gpt-terra-latest"}},
		{name: "no catalog at all is an error", set: set,
			in: "gpt/gpt-terra-latest", errs: []string{"no OpenRouter model catalog"}},
		{name: "no registry passes through", in: "gpt/gpt-terra-latest", want: "gpt/gpt-terra-latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			got, err := ResolveProviderLatest(tc.set, tc.cat, tc.in)
			if len(tc.errs) > 0 {
				ck.Require().Error(err, "ResolveProviderLatest(%q)", tc.in)
				for _, sub := range tc.errs {
					ck.ErrorContains(err, sub, "ResolveProviderLatest(%q) error", tc.in)
				}
				return
			}
			ck.Require().NoError(err, "ResolveProviderLatest(%q)", tc.in)
			ck.Eq(tc.want, got, "ResolveProviderLatest(%q)", tc.in)
		})
	}
}

// TestConversationPinsProviderLatest drives the hook through the real
// Conversation() path: the pinned id is what the conversation keeps, and
// routing.ResolveModel passes the provider-qualified result through intact.
func TestConversationPinsProviderLatest(t *testing.T) {
	ck := assert.NewAborting(t)
	c, err := NewClient(
		WithProviders(latestFixtureSet()),
		WithCatalog(latestFixtureCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err, "NewClient")

	conv, err := c.Conversation(context.Background(), NewConversation("", "test"), Model("gpt/gpt-terra-latest"))
	ck.Require().NoError(err, "Conversation")
	ck.Eq("gpt/gpt-6.1-terra", conv.cfg.model, "pinned model")
	ck.False(strings.Contains(conv.cfg.model, "-latest"), "no -latest survives into the conversation")
}

// TestKindVendorNamesAreRoutingVendors pins the string pairing pkg/providers
// cannot enforce itself: it does not import pkg/routing.
func TestKindVendorNamesAreRoutingVendors(t *testing.T) {
	ck := assert.NewAborting(t)
	for _, tc := range []struct {
		kind   providers.Kind
		vendor routing.Vendor
	}{
		{providers.KindAnthropic, routing.VendorAnthropic},
		{providers.KindOpenAIResponses, routing.VendorOpenAI},
		{providers.KindOpenAIResponsesChatGPT, routing.VendorOpenAI},
	} {
		got := routing.Vendor(tc.kind.Vendor())
		ck.Eq(tc.vendor, got, "%q's vendor", string(tc.kind))
	}
}
