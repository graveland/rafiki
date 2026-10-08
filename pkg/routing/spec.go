// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"fmt"
	"strings"
)

// Sort is a provider-sort preference inside a routing spec. The zero value
// SortInherit means "no opinion: inherit from the next level of the resolution
// chain" — it is never sent on the wire. SortBalanced is a DECISION to send no
// sort (opting out of an inherited sort), so it exists as a parseable value but
// is likewise omitted from ProviderPrefs. Only price/throughput/latency go on
// the wire.
type Sort string

const (
	SortInherit    Sort = ""           // zero value: inherit from the next level
	SortPrice      Sort = "price"      // cheapest eligible provider first
	SortThroughput Sort = "throughput" // fastest generation first
	SortLatency    Sort = "latency"    // fastest time-to-first-token first
	SortBalanced   Sort = "balanced"   // explicitly send no sort
)

// Spec is a parsed routing spec — the bracketed tail of a model string
// ("[sort=price,quant=fp8+]") or the stored content of a routing policy row.
// It carries only what OpenRouter's provider-routing object can express; the
// base model id lives outside it.
type Spec struct {
	Sort   Sort
	Quant  []string // as written: either a floor like "fp8+" (one element) or a list of OpenRouter quantizations
	Prefer []string // provider slugs, tried first; fallbacks stay on
	Only   []string // provider slugs
	NoData bool
	ZDR    bool
}

// sortValues is every sort spelling ParseSpec accepts. SortInherit ("") is
// deliberately absent: `sort=` is a valued key without a value, a parse error —
// inherit is expressed by leaving sort out.
var sortValues = map[Sort]bool{
	SortPrice:      true,
	SortThroughput: true,
	SortLatency:    true,
	SortBalanced:   true,
}

// quantTiers is the quantization ladder in bit-width tiers, lowest first: a
// floor on a name admits its whole tier and every tier above it. "unknown" is
// on no tier — a host OpenRouter hasn't labelled — so no floor admits it and
// "unknown+" is a parse error, while a quant LIST may name "unknown" explicitly.
var quantTiers = [][]string{
	{"int4", "fp4", "mxfp4", "nvfp4"}, // T1
	{"fp6"},                           // T2
	{"int8", "fp8", "mxfp8"},          // T3
	{"fp16", "bf16"},                  // T4
	{"fp32"},                          // T5
}

// quantTierOf maps each tiered quantization name to its tier index; quantNames
// is every name a quant value may carry, tiered names plus "unknown".
var (
	quantTierOf = func() map[string]int {
		m := make(map[string]int)
		for i, tier := range quantTiers {
			for _, name := range tier {
				m[name] = i
			}
		}
		return m
	}()
	quantNames = func() map[string]bool {
		m := make(map[string]bool)
		for _, tier := range quantTiers {
			for _, name := range tier {
				m[name] = true
			}
		}
		m["unknown"] = true
		return m
	}()
)

// ParseModel splits a model string into its base id and routing spec:
// "openrouter/z-ai/glm-5.3-flash[sort=price,nodata]" -> base
// "openrouter/z-ai/glm-5.3-flash", Spec{Sort: price, NoData: true}. A string
// with no brackets is all base with the zero Spec. Malformed — a "[" without a
// final "]", anything after "]", an empty "[]", a second "[", or an empty base —
// is an error: unknown syntax is never silently ignored, because a misread spec
// silently changes where requests are served.
func ParseModel(s string) (base string, spec Spec, err error) {
	open := strings.Index(s, "[")
	if open < 0 {
		if s == "" {
			return "", Spec{}, fmt.Errorf("routing spec: empty model")
		}
		return s, Spec{}, nil
	}
	if base = s[:open]; base == "" {
		return "", Spec{}, fmt.Errorf("routing spec: empty model before \"[\" in %q", s)
	}
	end := strings.LastIndex(s, "]")
	if end < open {
		return "", Spec{}, fmt.Errorf("routing spec: %q: \"[\" without a closing \"]\"", s)
	}
	if end != len(s)-1 {
		return "", Spec{}, fmt.Errorf("routing spec: %q: unexpected text %q after \"]\"", s, s[end+1:])
	}
	inner := s[open+1 : end]
	if strings.ContainsAny(inner, "[]") {
		return "", Spec{}, fmt.Errorf("routing spec: %q: nested bracket in %q", s, inner)
	}
	if inner == "" {
		return "", Spec{}, fmt.Errorf("routing spec: %q: empty brackets", s)
	}
	spec, err = ParseSpec(inner)
	if err != nil {
		return "", Spec{}, err
	}
	return base, spec, nil
}

// ParseSpec parses the inside of the brackets — the same string a routing
// policy row stores ("sort=price,quant=fp8+"). Items are comma-separated; the
// valued keys are sort, quant, prefer and only (list values joined with "|"),
// the flags nodata and zdr are bare. Any malformed or unknown item is an error
// naming it: never ignored. The empty string is the zero Spec — a policy row
// stores ” to mean "no spec".
//
// quant=, prefer= and only= with an EMPTY value are valid: they parse to a
// non-nil empty slice, the "explicitly cleared" signal that Merge keeps when a
// more general level set the key — a nil slice ("unset") inherits, a non-nil
// empty one ("cleared") does not. sort= with an empty value remains an error:
// sort inherit is expressed by omitting it.
func ParseSpec(s string) (Spec, error) {
	if s == "" {
		return Spec{}, nil
	}
	var spec Spec
	seen := make(map[string]bool)
	for _, item := range strings.Split(s, ",") {
		if item == "" {
			return Spec{}, fmt.Errorf("routing spec %q: empty item", s)
		}
		key, val, hasVal := strings.Cut(item, "=")
		switch key {
		case "sort":
			if !hasVal || val == "" {
				return Spec{}, fmt.Errorf("routing spec %q: item %q: sort needs a value", s, item)
			}
		case "quant", "prefer", "only":
			if !hasVal {
				return Spec{}, fmt.Errorf("routing spec %q: item %q: %q needs a value", s, item, key)
			}
		case "nodata", "zdr":
			if hasVal {
				return Spec{}, fmt.Errorf("routing spec %q: item %q: %q takes no value", s, item, key)
			}
		default:
			return Spec{}, fmt.Errorf("routing spec %q: item %q: unknown key %q", s, item, key)
		}
		if seen[key] {
			return Spec{}, fmt.Errorf("routing spec %q: item %q: %q repeated", s, item, key)
		}
		seen[key] = true
		switch key {
		case "sort":
			sort := Sort(val)
			if !sortValues[sort] {
				return Spec{}, fmt.Errorf("routing spec %q: item %q: unknown sort %q (want price|throughput|latency|balanced)", s, item, val)
			}
			spec.Sort = sort
		case "quant":
			quant, err := parseQuantList(s, item, val)
			if err != nil {
				return Spec{}, err
			}
			spec.Quant = quant
		case "prefer":
			if val == "" {
				spec.Prefer = []string{}
			} else {
				slugs := strings.Split(val, "|")
				for _, slug := range slugs {
					if slug == "" {
						return Spec{}, fmt.Errorf("routing spec %q: item %q: empty provider slug", s, item)
					}
				}
				spec.Prefer = slugs
			}
		case "only":
			if val == "" {
				spec.Only = []string{}
			} else {
				slugs := strings.Split(val, "|")
				for _, slug := range slugs {
					if slug == "" {
						return Spec{}, fmt.Errorf("routing spec %q: item %q: empty provider slug", s, item)
					}
				}
				spec.Only = slugs
			}
		case "nodata":
			spec.NoData = true
		case "zdr":
			spec.ZDR = true
		}
	}
	return spec, nil
}

// parseQuantList parses a quant value: either a single floor ("fp8+", one
// element stored as written) or a "|"-joined list of OpenRouter quantization
// names (which may include "unknown"). Every name is validated here — an
// unknown quantization is a parse error, not a silent pass-through.
func parseQuantList(spec, item, val string) ([]string, error) {
	if val == "" {
		return []string{}, nil
	}
	els := strings.Split(val, "|")
	for _, el := range els {
		if el == "" {
			return nil, fmt.Errorf("routing spec %q: item %q: empty quantization", spec, item)
		}
	}
	if len(els) > 1 {
		for _, el := range els {
			if strings.HasSuffix(el, "+") {
				return nil, fmt.Errorf("routing spec %q: item %q: quantization floor %q cannot be combined in a list", spec, item, el)
			}
		}
	}
	for _, el := range els {
		if name, floor := strings.CutSuffix(el, "+"); floor {
			if name == "unknown" {
				return nil, fmt.Errorf("routing spec %q: item %q: quantization floor %q: \"unknown\" is on no tier", spec, item, el)
			}
			if _, tiered := quantTierOf[name]; !tiered {
				return nil, fmt.Errorf("routing spec %q: item %q: unknown quantization %q", spec, item, el)
			}
		} else if !quantNames[name] {
			return nil, fmt.Errorf("routing spec %q: item %q: unknown quantization %q", spec, item, el)
		}
	}
	return els, nil
}

// copyList copies a string slice, preserving nil-vs-empty: a nil slice stays
// nil ("unset"), an empty non-nil one stays non-nil ("set to nothing") — the
// distinction Merge's "set = non-nil" rule and the wire's omitempty both read.
func copyList(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// Merge combines two specs from the resolution chain, the receiver being the
// more specific level (spawn over preset over policy row). Per key, the
// receiver's value wins when set — Sort when non-empty, Quant, Prefer and Only
// when non-nil — else the lower level's fills in. NoData and ZDR are monotone
// instead: set at ANY level they hold (confidentiality, not preference), so
// they OR.
func (s Spec) Merge(lower Spec) Spec {
	merged := Spec{NoData: s.NoData, ZDR: s.ZDR}
	if s.Sort != "" {
		merged.Sort = s.Sort
	} else {
		merged.Sort = lower.Sort
	}
	if s.Quant != nil {
		merged.Quant = copyList(s.Quant)
	} else if lower.Quant != nil {
		merged.Quant = copyList(lower.Quant)
	}
	if s.Prefer != nil {
		merged.Prefer = copyList(s.Prefer)
	} else if lower.Prefer != nil {
		merged.Prefer = copyList(lower.Prefer)
	}
	if s.Only != nil {
		merged.Only = copyList(s.Only)
	} else if lower.Only != nil {
		merged.Only = copyList(lower.Only)
	}
	merged.NoData = merged.NoData || lower.NoData
	merged.ZDR = merged.ZDR || lower.ZDR
	return merged
}

// String renders the canonical form — keys in fixed order (sort, quant, prefer,
// only, nodata, zdr), list values joined with "|", a floor kept as written
// ("fp8+"), unset keys omitted. sort=balanced IS emitted: it is a decision,
// unlike the zero Sort. ParseSpec(String()) returns an equal Spec, so a stored
// policy row round-trips.
func (s Spec) String() string {
	var parts []string
	if s.Sort != "" {
		parts = append(parts, "sort="+string(s.Sort))
	}
	if s.Quant != nil {
		parts = append(parts, "quant="+strings.Join(s.Quant, "|"))
	}
	if s.Prefer != nil {
		parts = append(parts, "prefer="+strings.Join(s.Prefer, "|"))
	}
	if s.Only != nil {
		parts = append(parts, "only="+strings.Join(s.Only, "|"))
	}
	if s.NoData {
		parts = append(parts, "nodata")
	}
	if s.ZDR {
		parts = append(parts, "zdr")
	}
	return strings.Join(parts, ",")
}

// IsZero reports whether the spec carries nothing at all.
func (s Spec) IsZero() bool {
	return s.Sort == "" && s.Quant == nil && s.Prefer == nil && s.Only == nil && !s.NoData && !s.ZDR
}

// Quantizations expands Quant for the wire: a floor ("fp8+") becomes every
// quantization in its tier and above, in tier order; a list is returned as
// written (it may name "unknown"); nil when Quant is unset. A floor never
// expands to "unknown" — floors exist to avoid problems and hold benchmarks
// fixed, and an unlabelled host breaks both.
func (s Spec) Quantizations() []string {
	if len(s.Quant) == 0 {
		return nil
	}
	if name, floor := strings.CutSuffix(s.Quant[0], "+"); floor && len(s.Quant) == 1 {
		tier, tiered := quantTierOf[name]
		if !tiered {
			return copyList(s.Quant) // unexpandable floor: send as written, fail loudly upstream
		}
		var out []string
		for _, t := range quantTiers[tier:] {
			out = append(out, t...)
		}
		return out
	}
	return copyList(s.Quant)
}

// Prefs builds the OpenRouter provider-routing object for a request whose
// resolved spec is s. pinOnly is the provider pin below every spec level (a
// providers.toml alias's only, or the static providerPins); ignore is the
// guard's merged bans and ejections. The bool reports whether anything needs
// sending at all — false means the caller sends no provider object.
//
// Rules:
//   - Only: the spec's own only when set, else the pin. A spec only is an
//     explicit routing decision and BYPASSES bans and ejections (decision 5 of
//     the design), so ignore is dropped in that case.
//   - Sort: sent for price/throughput/latency; omitted for SortInherit and for
//     SortBalanced (balanced = deliberately send no sort).
//   - Prefer: the spec's preferred slugs ride as Order, ALWAYS, independent of
//     Only — Order is a try-first list and OpenRouter still falls back to the
//     rest, so it neither bypasses nor drops bans and ejections.
//   - DataCollection "deny" and ZDR ride along regardless of Only — data policy
//     is the one boundary an only never bypasses; OpenRouter applies them
//     jointly.
func (s Spec) Prefs(pinOnly, ignore []string) (ProviderPrefs, bool) {
	var prefs ProviderPrefs
	if s.Only != nil {
		prefs.Only = copyList(s.Only)
	} else {
		prefs.Only = copyList(pinOnly)
		prefs.Ignore = copyList(ignore)
	}
	prefs.Order = copyList(s.Prefer)
	if s.Sort != SortInherit && s.Sort != SortBalanced {
		prefs.Sort = string(s.Sort)
	}
	prefs.Quantizations = s.Quantizations()
	if s.NoData {
		prefs.DataCollection = "deny"
	}
	prefs.ZDR = s.ZDR
	sent := len(prefs.Only) > 0 || len(prefs.Ignore) > 0 || prefs.Sort != "" ||
		len(prefs.Order) > 0 ||
		len(prefs.Quantizations) > 0 || prefs.DataCollection != "" || prefs.ZDR
	return prefs, sent
}
