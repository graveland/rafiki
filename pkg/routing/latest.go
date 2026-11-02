// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"regexp"
	"sort"
	"strings"
)

// Vendor names a model vendor whose "<tier>-latest" names resolve from the
// OpenRouter catalog. The values equal providers.Kind.Vendor()'s strings.
type Vendor string

const (
	VendorAnthropic Vendor = "anthropic"
	VendorOpenAI    Vendor = "openai"
)

type vendorSpec struct {
	// catalogPrefix is the OpenRouter id prefix shared by the vendor's models
	// and its "~<catalogPrefix><tier>-latest" alias entries.
	catalogPrefix string
	// localPrefix is the vendor's id prefix as a provider-local name carries it.
	localPrefix string
	// prefixOptional lets a local name omit localPrefix ("opus-latest").
	prefixOptional bool
}

var vendorSpecs = map[Vendor]vendorSpec{
	VendorAnthropic: {catalogPrefix: "anthropic/claude-", localPrefix: "claude-", prefixOptional: true},
	VendorOpenAI:    {catalogPrefix: "openai/gpt-", localPrefix: "gpt-"},
}

var tierRe = regexp.MustCompile(`^[a-z]+$`)

// openAIModelRe matches a concrete OpenAI model id inside a tier:
// "openai/gpt-<numeric version>-<tier>". The numeric version directly before
// the tier is what keeps "gpt-audio-mini" and "gpt-5.1-codex-mini" out of the
// "mini" tier, and the end anchor keeps "-pro" and ":batch" variants out of
// every tier.
var openAIModelRe = regexp.MustCompile(`^openai/gpt-[0-9]+(?:\.[0-9]+)*-([a-z]+)$`)

// LatestTier reports whether local, a provider-local model name, is a
// "<tier>-latest" name for vendor v, and returns the tier. OpenAI names need
// the "gpt-" prefix ("gpt-terra-latest"); Anthropic names take "claude-"
// optionally ("opus-latest", "claude-opus-latest"). A vendor with no entry,
// or a name whose middle is not a lowercase word, is not a latest name.
func LatestTier(v Vendor, local string) (string, bool) {
	spec, ok := vendorSpecs[v]
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(local, "-latest")
	if !ok {
		return "", false
	}
	rest, had := strings.CutPrefix(name, spec.localPrefix)
	if !had {
		if !spec.prefixOptional {
			return "", false
		}
		// The prefix may be omitted, but it may not be reduced to its stem: once
		// the stem is removed "claude-latest" has an empty middle, and omitting the
		// prefix is not a licence to re-read a longer id ("claudesonnet-latest") as
		// a tier.
		if name == strings.TrimSuffix(spec.localPrefix, "-") {
			return "", false
		}
		rest = name
	}
	if !tierRe.MatchString(rest) {
		return "", false
	}
	return rest, true
}

// ResolveTier returns the newest catalog model in tier for vendor v as
// (direct, orID): direct is the id the vendor's own API takes, orID the
// OpenRouter id. ok=false when the vendor is unknown or nothing matches.
// Newest is by the catalog's created stamp.
func (c *ModelCatalog) ResolveTier(v Vendor, tier string) (direct, orID string, ok bool) {
	switch v {
	case VendorAnthropic:
		return c.ResolveLatest(tier)
	case VendorOpenAI:
		c.refresh()
		c.mu.Lock()
		defer c.mu.Unlock()
		var best orModel
		for _, m := range c.models {
			sub := openAIModelRe.FindStringSubmatch(m.ID)
			if sub == nil || sub[1] != tier {
				continue
			}
			if best.ID == "" || m.Created > best.Created {
				best = m
			}
		}
		if best.ID == "" {
			return "", "", false
		}
		return strings.TrimPrefix(best.ID, "openai/"), best.ID, true
	}
	return "", "", false
}

// LatestNames lists the "<tier>-latest" names vendor v offers, derived from the
// catalog's own "~<prefix><tier>-latest" alias entries so a new tier appears
// the day OpenRouter lists it. Anthropic names are short ("opus-latest"),
// OpenAI names keep "gpt-" ("gpt-terra-latest"). Sorted; nil on a nil
// receiver, an unknown vendor or a cold catalog.
func (c *ModelCatalog) LatestNames(v Vendor) []string {
	spec, ok := vendorSpecs[v]
	if !ok || c == nil {
		return nil
	}
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	alias := "~" + spec.catalogPrefix
	var out []string
	for _, m := range c.models {
		mid, ok := strings.CutPrefix(m.ID, alias)
		if !ok {
			continue
		}
		tier, ok := strings.CutSuffix(mid, "-latest")
		if !ok || !tierRe.MatchString(tier) {
			continue
		}
		if spec.prefixOptional {
			out = append(out, tier+"-latest")
		} else {
			out = append(out, spec.localPrefix+tier+"-latest")
		}
	}
	sort.Strings(out)
	return out
}
