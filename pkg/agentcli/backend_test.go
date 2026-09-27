// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestAnalyzeEventUnion documents the union contract every Backend must
// honor: Kind selects exactly one payload.
func TestAnalyzeEventUnion(t *testing.T) {
	c := assert.NewAborting(t)
	ev := AnalyzeEvent{Kind: EventAnalysis, Analysis: &analyze.Analysis{ConversationID: "c1"}}
	c.False(ev.Kind != EventAnalysis || ev.Analysis == nil || ev.Progress != nil || ev.Summary != nil, "analysis event should carry only Analysis: %+v", ev)
	p := AnalyzeEvent{Kind: EventProgress, Progress: &Progress{ConversationID: "c1", State: StateFailed, Detail: "boom"}}
	c.False(p.Progress.State != StateFailed || p.Progress.Detail != "boom", "progress event lost fields: %+v", p.Progress)
	errEv := AnalyzeEvent{Kind: EventError, Err: errors.New("analysis failed")}
	c.False(errEv.Kind != EventError || errEv.Err == nil || errEv.Analysis != nil || errEv.Progress != nil || errEv.Summary != nil, "error event should carry only Err with all payloads nil: %+v", errEv)
}

// TestBackendInterfaceIsImplementable pins the method set: a compile-time
// assertion that a struct with these methods satisfies Backend.
func TestBackendInterfaceIsImplementable(t *testing.T) {
	var _ Backend = (*nopBackend)(nil)
}

// nopBackend is a stub implementing Backend with zero returns.
type nopBackend struct{}

func (*nopBackend) Stats(context.Context, insights.Scope, insights.StatsFilter) (*insights.Stats, error) {
	return nil, nil
}

func (*nopBackend) ConversationStats(context.Context, insights.Scope, string) (*insights.Stats, error) {
	return nil, nil
}

func (*nopBackend) Search(context.Context, insights.Scope, insights.SearchFilter) ([]insights.ConversationSummary, error) {
	return nil, nil
}

func (*nopBackend) Export(context.Context, insights.Scope, string) (*insights.Transcript, error) {
	return nil, nil
}

func (*nopBackend) Query(context.Context, insights.Scope, string, insights.StatsFilter) (insights.QueryResult, error) {
	return insights.QueryResult{}, nil
}

func (*nopBackend) Analyze(context.Context, AnalyzeRequest) (<-chan AnalyzeEvent, error) {
	return nil, nil
}

func (*nopBackend) Findings(context.Context, store.FindingFilter) ([]store.FindingRow, error) {
	return nil, nil
}

func (*nopBackend) SetFindingStatus(context.Context, string, string) (store.FindingRow, error) {
	return store.FindingRow{}, nil
}

// TestWireJSONTagsMatchServerShape locks down the snake_case JSON key
// spelling on Summary/Totals/Progress/RankedFindingWithDraft (and, in the
// sibling store package, FindingRow) against the server's own wire shape
// (client/pkg/server/agent_analyze.go's summaryPayload/summaryTotals/
// progressPayload/rankedFindingWithDraft). Before these tags existed, every
// one of these types marshaled with its bare Go field names (PascalCase),
// silently breaking any consumer expecting the server's snake_case contract.
func TestWireJSONTagsMatchServerShape(t *testing.T) {
	c := assert.NewAborting(t)
	sum := Summary{
		Ranked: []RankedFindingWithDraft{
			{
				RankedFinding: analyze.RankedFinding{Finding: analyze.Finding{Axis: "grind", Title: "t"}},
				Draft:         &analyze.SkillEdit{FindingTitle: "t"},
			},
		},
		Analyzed:   1,
		Skipped:    2,
		Failed:     3,
		Remaining:  4,
		Population: 5,
		Totals:     Totals{InputTokens: 10, OutputTokens: 20, CostUSD: 1.5},
	}

	raw, err := json.Marshal(sum)
	c.NoError(err)
	var m map[string]any
	c.NoError(json.Unmarshal(raw, &m))
	for _, key := range []string{"ranked", "analyzed", "skipped", "failed", "remaining", "population", "totals"} {
		if _, ok := m[key]; !ok {
			t.Errorf("Summary JSON missing snake_case key %q: %s", key, raw)
		}
	}

	totals, ok := m["totals"].(map[string]any)
	c.True(ok, "Summary.totals did not marshal as an object: %s", raw)
	for _, key := range []string{"input_tokens", "output_tokens", "cost_usd"} {
		if _, ok := totals[key]; !ok {
			t.Errorf("Totals JSON missing snake_case key %q: %s", key, raw)
		}
	}

	ranked, ok := m["ranked"].([]any)
	c.False(!ok || len(ranked) != 1, "Summary.ranked did not marshal as a one-element array: %s", raw)
	rf, ok := ranked[0].(map[string]any)
	c.True(ok, "ranked[0] did not marshal as an object: %s", raw)
	if _, ok := rf["draft"]; !ok {
		t.Errorf("RankedFindingWithDraft JSON missing snake_case key %q: %s", "draft", raw)
	}

	prog := Progress{ConversationID: "c1", State: StateDone, InputTokens: 1, OutputTokens: 2, CostUSD: 0.5}
	praw, err := json.Marshal(prog)
	c.NoError(err)
	var pm map[string]any
	c.NoError(json.Unmarshal(praw, &pm))
	for _, key := range []string{"conversation_id", "state", "input_tokens", "output_tokens", "cost_usd"} {
		if _, ok := pm[key]; !ok {
			t.Errorf("Progress JSON missing snake_case key %q: %s", key, praw)
		}
	}

	fr := store.FindingRow{ID: "f1", AnalysisID: "a1", Axis: "grind", TopicKey: "tk", Title: "t", ExpectedSavingsTokens: 7, Status: "open"}
	fraw, err := json.Marshal(fr)
	c.NoError(err)
	var fm map[string]any
	c.NoError(json.Unmarshal(fraw, &fm))
	for _, key := range []string{"id", "analysis_id", "axis", "topic_key", "title", "expected_savings_tokens", "status"} {
		if _, ok := fm[key]; !ok {
			t.Errorf("store.FindingRow JSON missing snake_case key %q: %s", key, fraw)
		}
	}
}
