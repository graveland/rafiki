package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// modelSpawner is an AgentSpawner that only answers Models. It records the
// query it was handed so a test can assert the tool parsed arguments rather
// than filtering them itself.
type modelSpawner struct {
	fakeSpawner
	rows []ModelInfo
	got  []ModelQuery
}

func (m *modelSpawner) Models(_ context.Context, q ModelQuery) ([]ModelInfo, error) {
	m.got = append(m.got, q)
	out := make([]ModelInfo, 0, len(m.rows))
	for _, r := range m.rows {
		if q.MaxInUSD != nil {
			v, ok := r.PromptUSDPerMillion()
			if ok && v > *q.MaxInUSD {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

// catalogRows is a small stand-in for the live catalog: two priced models, one
// free, and one locally-served model the catalog knows nothing about.
func catalogRows() []ModelInfo {
	return []ModelInfo{
		{ID: "openrouter/z-ai/glm-4.6", Provider: "openrouter",
			PromptUSD: f64(0.0000004), CompletionUSD: f64(0.00000175),
			ContextWindow: iptr(200_000), AgenticIndex: f64(41.2), Tools: "yes"},
		{ID: "openrouter/anthropic/claude-opus-5", Provider: "openrouter",
			PromptUSD: f64(0.000015), CompletionUSD: f64(0.000075),
			ContextWindow: iptr(200_000), AgenticIndex: f64(62.0), Tools: "yes"},
		{ID: "openrouter/free/thing", Provider: "openrouter",
			PromptUSD: f64(0), CompletionUSD: f64(0),
			ContextWindow: iptr(64_000), Tools: "yes"},
		{ID: "ollama/qwen3", Provider: "ollama", Tools: "unknown"}, // no catalog entry
	}
}

func runModels(t *testing.T, sp *modelSpawner, args string) string {
	t.Helper()
	reg, ctx := newAgentTools(t, sp)
	out, err := reg.Execute(ctx, "agent_models", json.RawMessage(args))
	if err != nil {
		t.Fatalf("agent_models(%s): %v", args, err)
	}
	return out
}

// TestNoArgsReturnsASummaryNotAList is the whole point of the shape: an
// unfiltered call must not dump 300+ ids into the agent's context.
func TestNoArgsReturnsASummaryNotAList(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	out := runModels(t, sp, `{}`)

	if strings.Contains(out, "openrouter/z-ai/glm-4.6") {
		t.Errorf("bare call listed model rows; want a summary only:\n%s", out)
	}
	if !strings.Contains(out, "4 model") {
		t.Errorf("summary does not report the total count:\n%s", out)
	}
	for _, want := range []string{"max_in_usd", "min_context", "sort", "limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary does not name the %q filter:\n%s", want, out)
		}
	}
}

// TestSummaryReportsDistribution is what makes the next call well-aimed. A
// bare count tells the agent to filter but not where to aim.
func TestSummaryReportsDistribution(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	out := runModels(t, sp, `{}`)

	for _, want := range []string{"in$/M", "ctx", "tools"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary omits the %q distribution:\n%s", want, out)
		}
	}
	// The free model and the unpriced local model are different facts and both
	// belong in the summary.
	if !strings.Contains(out, "free") || !strings.Contains(out, "unpriced") {
		t.Errorf("summary conflates free with unpriced:\n%s", out)
	}
}

// TestFilteredCallReturnsRowsWithCost is the gap this work closes: the agent
// could not see price at all.
func TestFilteredCallReturnsRowsWithCost(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	out := runModels(t, sp, `{"max_in_usd":1.0}`)

	if !strings.Contains(out, "openrouter/z-ai/glm-4.6") {
		t.Errorf("cheap model missing from a max_in_usd=1.0 query:\n%s", out)
	}
	if !strings.Contains(out, "0.40") {
		t.Errorf("row does not carry its input price:\n%s", out)
	}
	if !strings.Contains(out, "1.75") {
		t.Errorf("row does not carry its output price:\n%s", out)
	}
}

// TestUnpricedModelSurvivesAPriceBound is the absence rule reaching the agent
// surface: every locally-served model has no price.
func TestUnpricedModelSurvivesAPriceBound(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	out := runModels(t, sp, `{"max_in_usd":1.0}`)

	if !strings.Contains(out, "ollama/qwen3") {
		t.Errorf("a price bound dropped the unpriced local model:\n%s", out)
	}
}

// TestLimitCapsRowsButNotTheCount: the agent must know it is seeing a window.
func TestLimitCapsRowsButNotTheCount(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	out := runModels(t, sp, `{"max_in_usd":100,"limit":2}`)

	if strings.Count(out, "openrouter/")+strings.Count(out, "ollama/") > 2 {
		t.Errorf("limit=2 returned more than two rows:\n%s", out)
	}
	if !strings.Contains(out, "4") {
		t.Errorf("output does not report how many matched before the cap:\n%s", out)
	}
}

// TestZeroMatchesExplainsWhy turns five retries into one.
//
// The row set is deliberately all-priced: a free model and an unpriced local
// model are both ADMITTED by any price bound, so a catalog containing either
// can never answer zero to one. That is the absence rule working, not a gap.
func TestZeroMatchesExplainsWhy(t *testing.T) {
	sp := &modelSpawner{rows: []ModelInfo{
		{ID: "openrouter/a", PromptUSD: f64(0.0000015), ContextWindow: iptr(200_000), Tools: "yes"},
		{ID: "openrouter/b", PromptUSD: f64(0.000015), ContextWindow: iptr(200_000), Tools: "yes"},
	}}
	out := runModels(t, sp, `{"max_in_usd":0.10}`)

	if !strings.Contains(strings.ToLower(out), "0 of") && !strings.Contains(out, "no models") {
		t.Errorf("zero-match output does not say nothing matched:\n%s", out)
	}
	if !strings.Contains(out, "in$/M") {
		t.Errorf("zero-match output does not show the distribution to re-aim with:\n%s", out)
	}
}

// TestUnknownSortIsRefusedByName: a silently-ignored sort key orders on
// something the caller did not ask for.
func TestUnknownSortIsRefusedByName(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "agent_models", json.RawMessage(`{"sort":"cheapness"}`))
	if err == nil {
		t.Fatal("unknown sort key accepted")
	}
	if !strings.Contains(err.Error(), "agentic") {
		t.Errorf("refusal does not name the valid sort keys: %v", err)
	}
}

// TestQueryReachesTheSpawner: the tool parses arguments; the daemon filters.
func TestQueryReachesTheSpawner(t *testing.T) {
	sp := &modelSpawner{rows: catalogRows()}
	runModels(t, sp, `{"kind":"claude","min_context":128000,"needs":["tools","vision"],"max_out_usd":5}`)

	if len(sp.got) == 0 {
		t.Fatal("spawner never asked")
	}
	q := sp.got[0]
	if q.Kind != "claude" {
		t.Errorf("kind = %q, want claude", q.Kind)
	}
	if q.MinContext == nil || *q.MinContext != 128000 {
		t.Errorf("min_context = %v, want 128000", q.MinContext)
	}
	if q.MaxOutUSD == nil || *q.MaxOutUSD != 5 {
		t.Errorf("max_out_usd = %v, want 5", q.MaxOutUSD)
	}
	if strings.Join(q.Needs, ",") != "tools,vision" {
		t.Errorf("needs = %v, want [tools vision]", q.Needs)
	}
}

// TestDescriptionTellsTheModelToFilter guards the wording the whole design
// rests on. Same reason bash_start's description is pinned.
func TestDescriptionTellsTheModelToFilter(t *testing.T) {
	d := AgentModelsBlueprint{}.Description()
	low := strings.ToLower(d)
	if !strings.Contains(low, "filter") && !strings.Contains(low, "narrow") {
		t.Errorf("description does not tell the model to narrow its query:\n%s", d)
	}
	if !strings.Contains(low, "cost") && !strings.Contains(low, "price") {
		t.Errorf("description does not mention cost, which is why it exists:\n%s", d)
	}
}
