// SPDX-License-Identifier: Apache-2.0

package modelquery_test

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelquery"
)

func f64q(v float64) *float64 { return &v }

// --- rule 1: a bound admits a row the catalog cannot answer for ---

// TestBoundAdmitsUnknown is the rule that keeps the local fleet visible. Every
// locally-served model has no price, no context window and no score, so a
// bound that rejected unknowns would empty them out of every filtered list.
func TestBoundAdmitsUnknown(t *testing.T) {
	local := &rafikiv1.ModelRow{Id: "ollama/qwen3"} // no price, no ctx, no score

	cases := []struct {
		name  string
		field modelquery.Field
		bound modelquery.Bound
	}{
		{"max price", modelquery.FieldPromptUSD, modelquery.Bound{Max: f64q(1)}},
		{"min price", modelquery.FieldPromptUSD, modelquery.Bound{Min: f64q(1)}},
		{"min context", modelquery.FieldContext, modelquery.Bound{Min: f64q(200_000)}},
		{"paid only", modelquery.FieldPromptUSD, modelquery.Bound{PaidOnly: true}},
		{"min score", modelquery.FieldAgentic, modelquery.Bound{Min: f64q(40)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.bound.Admits(tc.field, local) {
				t.Fatalf("bound %v rejected a row the catalog cannot answer for; "+
					"that hides every locally-served model", tc.name)
			}
		})
	}
}

// TestRequirePresentIsTheOneException pins that presence CAN be demanded, but
// only by asking for it explicitly.
func TestRequirePresentIsTheOneException(t *testing.T) {
	unscored := &rafikiv1.ModelRow{Id: "ollama/qwen3"}
	scored := &rafikiv1.ModelRow{Id: "or/glm", AgenticIndex: f64q(41.2)}

	b := modelquery.Bound{RequirePresent: true}
	if b.Admits(modelquery.FieldAgentic, unscored) {
		t.Error("RequirePresent admitted an unscored row")
	}
	if !b.Admits(modelquery.FieldAgentic, scored) {
		t.Error("RequirePresent rejected a scored row")
	}
}

// TestPaidOnlyRejectsAKnownZero separates "free" from "unpriced". A reported
// 0 is an answer; a missing price is not.
func TestPaidOnlyRejectsAKnownZero(t *testing.T) {
	free := &rafikiv1.ModelRow{Id: "or/free", PromptUsd: f64q(0)}
	unpriced := &rafikiv1.ModelRow{Id: "ollama/local"}

	b := modelquery.Bound{PaidOnly: true}
	if b.Admits(modelquery.FieldPromptUSD, free) {
		t.Error("PaidOnly admitted a model whose price is a reported zero")
	}
	if !b.Admits(modelquery.FieldPromptUSD, unpriced) {
		t.Error("PaidOnly rejected an unpriced model; unknown is not free")
	}
}

// --- rule 2: an absent value sorts last in BOTH directions ---

// TestAbsentSortsLastInBothDirections is the rule that shipped wrong once with
// a comment claiming it was right. Flipping absence for a descending key puts
// every unscored model at the top of "smartest first".
func TestAbsentSortsLastInBothDirections(t *testing.T) {
	for _, desc := range []bool{false, true} {
		rows := []*rafikiv1.ModelRow{
			{Id: "a/unscored"},
			{Id: "b/scored", AgenticIndex: f64q(41.2)},
			{Id: "c/unscored"},
			{Id: "d/scored", AgenticIndex: f64q(12.0)},
		}
		modelquery.Sort(rows, []modelquery.SortKey{{Field: modelquery.FieldAgentic, Desc: desc}})

		last := []string{rows[2].GetId(), rows[3].GetId()}
		for _, id := range last {
			if id != "a/unscored" && id != "c/unscored" {
				t.Fatalf("desc=%v: tail = %v, want both unscored rows last", desc, last)
			}
		}
	}
}

// TestTwoAbsentValuesTie is what lets the next sort key decide.
func TestTwoAbsentValuesTie(t *testing.T) {
	rows := []*rafikiv1.ModelRow{
		{Id: "b/second", PromptUsd: f64q(0.000002)},
		{Id: "a/first", PromptUsd: f64q(0.000001)},
	}
	modelquery.Sort(rows, []modelquery.SortKey{
		{Field: modelquery.FieldAgentic, Desc: true},
		{Field: modelquery.FieldPromptUSD},
	})
	if rows[0].GetId() != "a/first" {
		t.Errorf("first = %q, want the cheaper of two unscored models", rows[0].GetId())
	}
}

// --- rule 3: unknown capability is kept, never treated as "no" ---

func TestUnknownCapabilityIsNotNo(t *testing.T) {
	unknown := &rafikiv1.ModelRow{Id: "ollama/qwen3"} // no catalog entry
	no := &rafikiv1.ModelRow{Id: "or/plain", SupportedParameters: []string{"temperature"}}
	yes := &rafikiv1.ModelRow{Id: "or/agent", SupportedParameters: []string{"tools", "reasoning"}}

	if got := modelquery.Tools(unknown); got != modelquery.SupportUnknown {
		t.Errorf("Tools(no catalog entry) = %v, want unknown", got)
	}
	if got := modelquery.Tools(no); got != modelquery.SupportNo {
		t.Errorf("Tools(params without tools) = %v, want no", got)
	}
	if got := modelquery.Tools(yes); got != modelquery.SupportYes {
		t.Errorf("Tools(params with tools) = %v, want yes", got)
	}
	if got := modelquery.Reasoning(unknown); got != modelquery.SupportUnknown {
		t.Errorf("Reasoning(no catalog entry) = %v, want unknown", got)
	}

	visionUnknown := &rafikiv1.ModelRow{Id: "ollama/qwen3"}
	visionNo := &rafikiv1.ModelRow{Id: "or/text", InputModalities: []string{"text"}}
	visionYes := &rafikiv1.ModelRow{Id: "or/vlm", InputModalities: []string{"text", "image"}}

	if got := modelquery.Vision(visionUnknown); got != modelquery.SupportUnknown {
		t.Errorf("Vision(no modalities) = %v, want unknown", got)
	}
	if got := modelquery.Vision(visionNo); got != modelquery.SupportNo {
		t.Errorf("Vision(text only) = %v, want no", got)
	}
	if got := modelquery.Vision(visionYes); got != modelquery.SupportYes {
		t.Errorf("Vision(text+image) = %v, want yes", got)
	}
}

// --- units and parsing ---

// TestPricesArePerMillion pins the unit conversion every bound and rendering
// depends on. The catalog reports per token.
func TestPricesArePerMillion(t *testing.T) {
	r := &rafikiv1.ModelRow{Id: "or/x", PromptUsd: f64q(0.0000004)} // $0.40/M
	v, ok := modelquery.FieldPromptUSD.Value(r)
	if !ok {
		t.Fatal("price reported absent")
	}
	if v < 0.399 || v > 0.401 {
		t.Errorf("value = %v, want ~0.40 per million", v)
	}
}

// TestValueKeepsAReportedZero guards the difference a > 0 guard would destroy.
func TestValueKeepsAReportedZero(t *testing.T) {
	free := &rafikiv1.ModelRow{Id: "or/free", PromptUsd: f64q(0)}
	if v, ok := modelquery.FieldPromptUSD.Value(free); !ok || v != 0 {
		t.Errorf("Value(reported zero) = (%v, %v), want (0, true)", v, ok)
	}
}

func TestParseField(t *testing.T) {
	cases := map[string]modelquery.Field{
		"in$":     modelquery.FieldPromptUSD,
		"in":      modelquery.FieldPromptUSD,
		"out":     modelquery.FieldCompletionUSD,
		"ctx":     modelquery.FieldContext,
		"context": modelquery.FieldContext,
		"agentic": modelquery.FieldAgentic,
		"newest":  modelquery.FieldAge,
		"intel":   modelquery.FieldIntelligence,
		"code":    modelquery.FieldCoding,
	}
	for in, want := range cases {
		got, ok := modelquery.ParseField(in)
		if !ok || got != want {
			t.Errorf("ParseField(%q) = (%v, %v), want (%v, true)", in, got, ok, want)
		}
	}
	if _, ok := modelquery.ParseField("cheapness"); ok {
		t.Error("ParseField accepted an unknown key; an unknown sort must be refused by name")
	}
}

// TestBiggerIsBetter pins the direction a bare sort key implies.
func TestBiggerIsBetter(t *testing.T) {
	for _, f := range []modelquery.Field{
		modelquery.FieldContext, modelquery.FieldAgentic,
		modelquery.FieldIntelligence, modelquery.FieldCoding,
	} {
		if !modelquery.BiggerIsBetter(f) {
			t.Errorf("BiggerIsBetter(%v) = false, want true", f)
		}
	}
	for _, f := range []modelquery.Field{
		modelquery.FieldPromptUSD, modelquery.FieldCompletionUSD, modelquery.FieldModel,
	} {
		if modelquery.BiggerIsBetter(f) {
			t.Errorf("BiggerIsBetter(%v) = true, want false", f)
		}
	}
}

// TestEveryFieldHasAName fails when a field is added without a short name,
// which would otherwise degrade silently to "?" in both surfaces.
func TestEveryFieldHasAName(t *testing.T) {
	for f := modelquery.Field(0); f < modelquery.FieldCount; f++ {
		if f.String() == "?" {
			t.Errorf("field %d has no short name", int(f))
		}
		if _, ok := modelquery.ParseField(f.String()); !ok {
			t.Errorf("field %q does not round-trip through ParseField", f.String())
		}
	}
}
