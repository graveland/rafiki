// SPDX-License-Identifier: Apache-2.0

// Package modelquery holds the model-catalog filtering and ordering semantics
// shared by the cockpit's picker and the daemon's agent_models tool.
//
// It exists for one reason: three rules about ABSENT values are easy to get
// backwards, each has already shipped wrong in this repo at least once, and
// two surfaces now need them. The rules are:
//
//   - A bound ADMITS a row the catalog cannot answer for. Every locally-served
//     model (ollama, LM Studio, a custom provider) has no price, no context
//     window and no score, so a bound that rejected unknowns would silently
//     empty the entire local fleet out of every filtered list.
//   - An absent value sorts LAST IN BOTH DIRECTIONS. It is not "the largest";
//     it is no answer. Flipping it for a descending key puts every unscored
//     model at the top of "smartest first".
//   - Unknown tool/vision support is KEPT, never treated as "no". A nil
//     parameter list means no catalog entry, not a missing capability.
//
// Everything here is presentation-free. Column headers, widths, the picker's
// named bound stops and the agent tool's table rendering all live with their
// own surface — only the semantics are shared.
package modelquery

import (
	"sort"
	"strings"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Field names a catalog column that can be ordered or constrained.
//
// One enum for both, because they are the same set: anything worth ordering by
// is worth constraining.
type Field int

const (
	FieldModel Field = iota
	FieldContext
	FieldPromptUSD
	FieldCompletionUSD
	FieldCacheReadUSD
	FieldMaxCompletion
	FieldAge
	FieldIntelligence
	FieldCoding
	FieldAgentic
	FieldCount
)

// String is the short name both surfaces show and the agent tool accepts as a
// sort key.
func (f Field) String() string {
	switch f {
	case FieldModel:
		return "model"
	case FieldContext:
		return "ctx"
	case FieldPromptUSD:
		return "in$"
	case FieldCompletionUSD:
		return "out$"
	case FieldCacheReadUSD:
		return "cache"
	case FieldMaxCompletion:
		return "max out"
	case FieldAge:
		return "age"
	case FieldIntelligence:
		return "intel"
	case FieldCoding:
		return "code"
	case FieldAgentic:
		return "agentic"
	}
	return "?"
}

// Value returns the field's numeric value for one row.
//
// ok=false means the catalog has NO ANSWER -- never zero. Callers must keep
// the two apart: a reported price of 0 (a free model) and an unpriced local
// model are different facts and rank differently.
//
// Prices are returned per MILLION tokens, because that is the unit every
// bound and every rendering is expressed in; the catalog reports per token.
func (f Field) Value(r *rafikiv1.ModelRow) (float64, bool) {
	switch f {
	case FieldContext:
		if r.ContextWindow == nil {
			return 0, false
		}
		return float64(*r.ContextWindow), true
	case FieldPromptUSD:
		if r.PromptUsd == nil {
			return 0, false
		}
		return *r.PromptUsd * 1e6, true
	case FieldCompletionUSD:
		if r.CompletionUsd == nil {
			return 0, false
		}
		return *r.CompletionUsd * 1e6, true
	case FieldCacheReadUSD:
		if r.CacheReadUsd == nil {
			return 0, false
		}
		return *r.CacheReadUsd * 1e6, true
	case FieldMaxCompletion:
		if r.MaxCompletionTokens == nil {
			return 0, false
		}
		return float64(*r.MaxCompletionTokens), true
	case FieldAge:
		if r.Created == nil || *r.Created <= 0 {
			return 0, false
		}
		return float64(*r.Created), true
	case FieldIntelligence:
		if r.IntelligenceIndex == nil {
			return 0, false
		}
		return *r.IntelligenceIndex, true
	case FieldCoding:
		if r.CodingIndex == nil {
			return 0, false
		}
		return *r.CodingIndex, true
	case FieldAgentic:
		if r.AgenticIndex == nil {
			return 0, false
		}
		return *r.AgenticIndex, true
	}
	return 0, false
}

// BiggerIsBetter says which end of a field a reader usually wants first.
// Context, scores and output limits read best descending; prices and names
// ascending. It is what makes a bare sort key ("agentic") mean the useful
// direction without the caller stating one.
func BiggerIsBetter(f Field) bool {
	switch f {
	case FieldContext, FieldMaxCompletion, FieldIntelligence, FieldCoding, FieldAgentic, FieldAge:
		return true
	}
	return false
}

// ParseField resolves a field's short name. ok is false for anything else, so
// a caller can reject an unknown sort key by name rather than silently
// ordering on something the caller did not ask for.
func ParseField(s string) (Field, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for f := Field(0); f < FieldCount; f++ {
		if f.String() == s {
			return f, true
		}
	}
	// Spellings the short names do not cover but callers reach for.
	switch s {
	case "in", "prompt", "input":
		return FieldPromptUSD, true
	case "out", "completion", "output":
		return FieldCompletionUSD, true
	case "context":
		return FieldContext, true
	case "newest", "created":
		return FieldAge, true
	case "intelligence":
		return FieldIntelligence, true
	case "coding":
		return FieldCoding, true
	}
	return 0, false
}

// Bound is a constraint on one field.
//
// Min/Max are absolute values in the field's own unit (prices per million
// tokens, context in tokens), NOT named stops -- the picker's stop tables are
// a keyboard affordance and stay with the picker.
type Bound struct {
	Min *float64
	Max *float64
	// PaidOnly requires a KNOWN value strictly above zero. Free models are a
	// few percent of the catalog and are heavily rate-limited, so excluding
	// them is a real query -- and it cannot be expressed as a minimum,
	// because a handful of free models pass any "<= $2" ceiling.
	PaidOnly bool
	// RequirePresent rejects a row the catalog cannot answer for. It is the
	// ONE deliberate exception to the admit-unknowns rule and must be asked
	// for explicitly; see Admits.
	RequirePresent bool
}

// Set reports whether this bound constrains anything.
func (b Bound) Set() bool {
	return b.Min != nil || b.Max != nil || b.PaidOnly || b.RequirePresent
}

// Admits reports whether a row satisfies this bound.
//
// A row the catalog cannot answer for is ADMITTED unless RequirePresent is
// set. That default is what keeps every locally-served model -- which has no
// price, no context and no score -- from vanishing out of every filtered list.
func (b Bound) Admits(f Field, r *rafikiv1.ModelRow) bool {
	v, ok := f.Value(r)
	if !ok {
		// Unknown: admitted, unless the caller explicitly asked for presence.
		return !b.RequirePresent
	}
	if b.PaidOnly && v <= 0 {
		return false
	}
	if b.Min != nil && v < *b.Min {
		return false
	}
	if b.Max != nil && v > *b.Max {
		return false
	}
	return true
}

// AdmitsAll reports whether a row satisfies every bound in the set.
func AdmitsAll(bounds map[Field]Bound, r *rafikiv1.ModelRow) bool {
	for f, b := range bounds {
		if !b.Admits(f, r) {
			return false
		}
	}
	return true
}

// SortKey is one ordering term. Keys apply in slice order, the first that
// separates two rows deciding.
type SortKey struct {
	Field Field
	Desc  bool
}

// Compare orders two rows on one field.
//
// byPresence=true means the answer came from one side being ABSENT. The caller
// must NOT flip that verdict for a descending key -- an absent value is not
// "the largest", it is no answer, and it sorts last in both directions.
//
// Two absent values TIE (0, false), which is what lets the next key decide.
func Compare(a, b *rafikiv1.ModelRow, f Field) (order int, byPresence bool) {
	if f == FieldModel {
		return strings.Compare(a.GetId(), b.GetId()), false
	}
	av, aok := f.Value(a)
	bv, bok := f.Value(b)
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

// Sort orders rows by every key in turn, the first that separates two rows
// deciding. The id is the final tiebreak so the order is total and a re-render
// never reshuffles equal rows.
func Sort(rows []*rafikiv1.ModelRow, keys []SortKey) {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			c, byPresence := Compare(rows[i], rows[j], k.Field)
			if c == 0 {
				continue
			}
			// Presence is NOT subject to direction. Only a comparison between
			// two KNOWN values flips.
			if byPresence {
				return c < 0
			}
			if k.Desc {
				c = -c
			}
			return c < 0
		}
		return rows[i].GetId() < rows[j].GetId()
	})
}

// Support is a tri-state claim about a capability. Unknown is a real answer
// and is never the same as No.
type Support int

const (
	SupportUnknown Support = iota // no catalog entry at all
	SupportNo
	SupportYes
)

// String renders the tri-state for humans and models alike.
func (s Support) String() string {
	switch s {
	case SupportYes:
		return "yes"
	case SupportNo:
		return "no"
	}
	return "unknown"
}

// Vision reads the modality claim, and UNKNOWN is a real answer.
//
// Empty modalities means the daemon has no catalog entry for this id -- which
// is every locally-served model -- and is NOT the same as "no vision".
func Vision(r *rafikiv1.ModelRow) Support {
	mods := r.GetInputModalities()
	if len(mods) == 0 {
		return SupportUnknown
	}
	for _, m := range mods {
		if m == "image" {
			return SupportYes
		}
	}
	return SupportNo
}

// Tools reads the tool-calling claim, and UNKNOWN is a real answer.
//
// A nil parameter list means no catalog entry -- three openrouter/* router
// meta-models, and every locally-served model -- and is NOT the same as
// "cannot tool-call".
func Tools(r *rafikiv1.ModelRow) Support {
	return paramSupport(r, "tools")
}

// Reasoning reads the reasoning-parameter claim, with the same unknown rule.
func Reasoning(r *rafikiv1.ModelRow) Support {
	return paramSupport(r, "reasoning")
}

func paramSupport(r *rafikiv1.ModelRow, want string) Support {
	params := r.GetSupportedParameters()
	if len(params) == 0 {
		return SupportUnknown
	}
	if HasParam(r, want) {
		return SupportYes
	}
	return SupportNo
}

// HasParam reports whether the catalog lists a supported parameter by name.
func HasParam(r *rafikiv1.ModelRow, want string) bool {
	for _, p := range r.GetSupportedParameters() {
		if p == want {
			return true
		}
	}
	return false
}
