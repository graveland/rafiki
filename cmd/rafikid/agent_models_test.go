package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelquery"
)

// TestEveryToolSortKeyResolves closes the one drift seam between the tool's
// accepted arguments and the daemon that acts on them: agent_models validates
// a sort key against its own list, and the daemon resolves it through
// modelquery. A key accepted there and unknown here would order on nothing,
// with no error anywhere.
func TestEveryToolSortKeyResolves(t *testing.T) {
	keys := tools.ModelSortKeys()
	if len(keys) == 0 {
		t.Fatal("tool advertises no sort keys")
	}
	for _, k := range keys {
		if _, ok := modelquery.ParseField(k); !ok {
			t.Errorf("agent_models accepts sort %q but modelquery cannot resolve it", k)
		}
	}
}

// TestNeedsKeepsUnknownCapability is the absence rule on the daemon side. Every
// locally-served model has no catalog entry, so a needs filter that read
// unknown as "no" would hide the whole local fleet.
func TestNeedsKeepsUnknownCapability(t *testing.T) {
	unknown := &rafikiv1.ModelRow{Id: "ollama/qwen3"} // no supported_parameters
	no := &rafikiv1.ModelRow{Id: "or/plain", SupportedParameters: []string{"temperature"}}
	yes := &rafikiv1.ModelRow{Id: "or/agent", SupportedParameters: []string{"tools"}}

	if !admitsNeeds(unknown, []string{"tools"}) {
		t.Error("needs=tools dropped a model the catalog cannot answer for")
	}
	if admitsNeeds(no, []string{"tools"}) {
		t.Error("needs=tools kept a model that reports no tool support")
	}
	if !admitsNeeds(yes, []string{"tools"}) {
		t.Error("needs=tools dropped a model that reports tool support")
	}
	if admitsNeeds(yes, []string{"nonsense"}) {
		t.Error("an unrecognised capability was ignored rather than refused")
	}
}

// TestModelBoundsAdmitUnpriced pins that a price ceiling keeps rows the
// catalog has no price for.
func TestModelBoundsAdmitUnpriced(t *testing.T) {
	max := 1.0
	q := tools.ModelQuery{MaxInUSD: &max}
	bounds := modelBounds(q)

	unpriced := &rafikiv1.ModelRow{Id: "ollama/qwen3"}
	if !modelquery.AdmitsAll(bounds, unpriced) {
		t.Error("max_in_usd dropped an unpriced model")
	}

	cheap := 0.0000004 // $0.40/M
	dear := 0.000015   // $15/M
	if !modelquery.AdmitsAll(bounds, &rafikiv1.ModelRow{Id: "or/cheap", PromptUsd: &cheap}) {
		t.Error("max_in_usd=1.0 rejected a $0.40/M model")
	}
	if modelquery.AdmitsAll(bounds, &rafikiv1.ModelRow{Id: "or/dear", PromptUsd: &dear}) {
		t.Error("max_in_usd=1.0 admitted a $15/M model")
	}
}

// TestToolModelInfoPreservesAbsence guards the pointer copy. A > 0 guard here
// would turn a reported zero (a free model) into absent, which is the same
// class of bug decorateRows carries a warning about.
func TestToolModelInfoPreservesAbsence(t *testing.T) {
	zero := 0.0
	ctx := int32(0)
	row := &rafikiv1.ModelRow{Id: "or/free", PromptUsd: &zero, ContextWindow: &ctx}

	got := toolModelInfo(row)
	if got.PromptUSD == nil {
		t.Error("a reported price of zero became absent")
	} else if *got.PromptUSD != 0 {
		t.Errorf("price = %v, want 0", *got.PromptUSD)
	}
	if got.ContextWindow == nil {
		t.Error("a reported context window of zero became absent")
	}

	checkBareRow(t)
}

// TestSortDirectionWordMatchesBiggerIsBetter closes the second drift seam
// between the tool and the daemon. agent_models tells the caller which end of
// a sort comes first ("highest first"); modelquery decides which end actually
// does. The tools package deliberately does not import modelquery -- that
// would drag protobuf into every binary linking a tool registry -- so the two
// agree only by this test.
func TestSortDirectionWordMatchesBiggerIsBetter(t *testing.T) {
	for _, k := range tools.ModelSortKeys() {
		f, ok := modelquery.ParseField(k)
		if !ok {
			continue // TestEveryToolSortKeyResolves reports this
		}
		want := "lowest"
		if modelquery.BiggerIsBetter(f) {
			want = "highest"
		}
		if got := tools.SortDirectionWord(k); got != want {
			t.Errorf("sort %q: tool says %q first, modelquery orders %q first", k, got, want)
		}
	}
}

func checkBareRow(t *testing.T) {
	t.Helper()
	bare := toolModelInfo(&rafikiv1.ModelRow{Id: "ollama/qwen3"})
	if bare.PromptUSD != nil || bare.ContextWindow != nil || bare.AgenticIndex != nil {
		t.Error("an absent field became present")
	}
	if bare.Tools != "unknown" || bare.Vision != "unknown" {
		t.Errorf("capability tri-states = (%q, %q), want unknown; "+
			"reading them as \"no\" hides every locally-served model",
			bare.Tools, bare.Vision)
	}
}
