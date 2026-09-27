package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/modelquery"

	"github.com/multigres/testkit/assert"
)

// TestEveryToolSortKeyResolves closes the one drift seam between the tool's
// accepted arguments and the daemon that acts on them: agent_models validates
// a sort key against its own list, and the daemon resolves it through
// modelquery. A key accepted there and unknown here would order on nothing,
// with no error anywhere.
func TestEveryToolSortKeyResolves(t *testing.T) {
	c := assert.NewCollecting(t)
	keys := tools.ModelSortKeys()
	c.Require().NotEmpty(keys, "tool advertises no sort keys")
	for _, k := range keys {
		_, ok := modelquery.ParseField(k)
		c.True(ok, "agent_models accepts sort %q but modelquery cannot resolve it", k)
	}
}

// TestNeedsKeepsUnknownCapability is the absence rule on the daemon side. Every
// locally-served model has no catalog entry, so a needs filter that read
// unknown as "no" would hide the whole local fleet.
func TestNeedsKeepsUnknownCapability(t *testing.T) {
	c := assert.NewCollecting(t)
	unknown := &rafikiv1.ModelRow{Id: "ollama/qwen3"} // no supported_parameters
	no := &rafikiv1.ModelRow{Id: "or/plain", SupportedParameters: []string{"temperature"}}
	yes := &rafikiv1.ModelRow{Id: "or/agent", SupportedParameters: []string{"tools"}}

	c.True(admitsNeeds(unknown, []string{"tools"}), "needs=tools dropped a model the catalog cannot answer for")
	c.False(admitsNeeds(no, []string{"tools"}), "needs=tools kept a model that reports no tool support")
	c.True(admitsNeeds(yes, []string{"tools"}), "needs=tools dropped a model that reports tool support")
	c.False(admitsNeeds(yes, []string{"nonsense"}), "an unrecognised capability was ignored rather than refused")
}

// TestModelBoundsAdmitUnpriced pins that a price ceiling keeps rows the
// catalog has no price for.
func TestModelBoundsAdmitUnpriced(t *testing.T) {
	c := assert.NewCollecting(t)
	max := 1.0
	q := tools.ModelQuery{MaxInUSD: &max}
	bounds := modelBounds(q)

	unpriced := &rafikiv1.ModelRow{Id: "ollama/qwen3"}
	c.True(modelquery.AdmitsAll(bounds, unpriced), "max_in_usd dropped an unpriced model")

	cheap := 0.0000004 // $0.40/M
	dear := 0.000015   // $15/M
	c.True(modelquery.AdmitsAll(bounds, &rafikiv1.ModelRow{Id: "or/cheap", PromptUsd: &cheap}), "max_in_usd=1.0 rejected a $0.40/M model")
	c.False(modelquery.AdmitsAll(bounds, &rafikiv1.ModelRow{Id: "or/dear", PromptUsd: &dear}), "max_in_usd=1.0 admitted a $15/M model")
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
	assert.NewCollecting(t).NotNil(got.ContextWindow, "a reported context window of zero became absent")

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
		got := tools.SortDirectionWord(k)
		assert.NewCollecting(t).Eq(want, got, "sort %q: tool says %q first, modelquery orders %q first", k, got, want)
	}
}

func checkBareRow(t *testing.T) {
	t.Helper()
	bare := toolModelInfo(&rafikiv1.ModelRow{Id: "ollama/qwen3"})
	assert.NewCollecting(t).False(bare.PromptUSD != nil || bare.ContextWindow != nil || bare.AgenticIndex != nil, "an absent field became present")
	if bare.Tools != "unknown" || bare.Vision != "unknown" {
		t.Errorf("capability tri-states = (%q, %q), want unknown; "+
			"reading them as \"no\" hides every locally-served model",
			bare.Tools, bare.Vision)
	}
}
