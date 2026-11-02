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
		// The prefix may be omitted, but the middle may not be exactly the prefix
		// stem: "claude-latest" carries nothing but "claude" between the name's
		// ends, and "claude" is not a tier (it is the vendor's own id prefix).
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
// OpenRouter id. Newest is by the catalog's created stamp. ok=false on a nil
// receiver, a cold catalog, an unknown vendor, a tier that is not tier-shaped,
// or nothing to match.
//
// A tier is OFFERED only when the catalog advertises it with a
// "~<catalogPrefix><tier>-latest" alias entry — the same rows LatestNames
// reads — so a variant suffix that no alias names ("-pro", "-fast", "-chat")
// can never be pinned by a "<tier>-latest" name, and a tier OpenRouter starts
// listing works the day it appears, with no vendor vocabulary in this package.
// The Anthropic arm delegates to ResolveLatest, whose native-id filter is what
// keeps "-fast" variants out; it takes no alias gate because the
// "<family>-latest" names it serves are ResolveModel's long-standing contract.
func (c *ModelCatalog) ResolveTier(v Vendor, tier string) (direct, orID string, ok bool) {
	spec, known := vendorSpecs[v]
	if !known || c == nil || !tierRe.MatchString(tier) {
		return "", "", false
	}
	switch v {
	case VendorAnthropic:
		return c.ResolveLatest(tier)
	case VendorOpenAI:
		c.refresh()
		c.mu.Lock()
		defer c.mu.Unlock()
		// One scan of the locked snapshot answers both questions: whether the
		// catalog offers this tier at all, and which of its models is newest.
		alias := "~" + spec.catalogPrefix + tier + "-latest"
		var offered bool
		var best orModel
		for _, m := range c.models {
			if m.ID == alias {
				offered = true
				continue
			}
			sub := openAIModelRe.FindStringSubmatch(m.ID)
			if sub == nil || sub[1] != tier {
				continue
			}
			if best.ID == "" || m.Created > best.Created {
				best = m
			}
		}
		if !offered || best.ID == "" {
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
