// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"git.graveland.dev/brent/rafiki/analyze"
	"git.graveland.dev/brent/rafiki/insights"
	"git.graveland.dev/brent/rafiki/store"
)

// fakeBackend is a Backend stub for Compare tests: Analyze looks up the
// requested profile's DetectorModel in byModel/failFor and emits the
// corresponding canned Summary or error, without any DB or LLM.
type fakeBackend struct {
	byModel map[string]*Summary
	failFor map[string]bool
}

func (*fakeBackend) Stats(context.Context, insights.StatsFilter) (*insights.Stats, error) {
	return nil, nil
}

func (*fakeBackend) ConversationStats(context.Context, string) (*insights.Stats, error) {
	return nil, nil
}

func (*fakeBackend) Search(context.Context, insights.SearchFilter) ([]insights.ConversationSummary, error) {
	return nil, nil
}

func (*fakeBackend) Export(context.Context, string) (*insights.Transcript, error) {
	return nil, nil
}

func (f *fakeBackend) Analyze(_ context.Context, req AnalyzeRequest) (<-chan AnalyzeEvent, error) {
	model := req.Profile.DetectorModel
	ch := make(chan AnalyzeEvent, 4)
	go func() {
		defer close(ch)
		if f.failFor[model] {
			ch <- AnalyzeEvent{Kind: EventError, Err: fmt.Errorf("model %s failed", model)}
			return
		}
		ch <- AnalyzeEvent{Kind: EventSummary, Summary: f.byModel[model]}
	}()
	return ch, nil
}

func (*fakeBackend) Findings(context.Context, store.FindingFilter) ([]store.FindingRow, error) {
	return nil, nil
}

func (*fakeBackend) SetFindingStatus(context.Context, string, string) (store.FindingRow, error) {
	return store.FindingRow{}, nil
}

func TestCompareRunsEachModelAndIsolatesFailures(t *testing.T) {
	fake := &fakeBackend{byModel: map[string]*Summary{
		"claude-haiku-4-5": {Analyzed: 1, Ranked: []RankedFindingWithDraft{{RankedFinding: analyze.RankedFinding{Finding: analyze.Finding{Axis: "grind", Title: "a"}}}}},
	}, failFor: map[string]bool{"broken/model": true}}
	runs, err := Compare(context.Background(), fake, AnalyzeRequest{Profile: &analyze.Profile{}}, []string{"claude-haiku-4-5", "broken/model"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("want a run per model, got %d", len(runs))
	}
	if runs[0].Err != nil || runs[0].Summary.Analyzed != 1 {
		t.Errorf("first run should succeed: %+v", runs[0])
	}
	if runs[1].Err == nil {
		t.Error("failing model must record its error, not abort the sweep")
	}
}

func TestRenderCompareTable(t *testing.T) {
	var b bytes.Buffer
	err := RenderCompare(&b, []CompareRun{
		{Model: "claude-haiku-4-5", Summary: &Summary{Analyzed: 1, Ranked: []RankedFindingWithDraft{{RankedFinding: analyze.RankedFinding{Finding: analyze.Finding{Axis: "grind", Title: "a"}}}}, Totals: Totals{OutputTokens: 500}}},
		{Model: "broken/model", Err: errors.New("upstream refused")},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"claude-haiku-4-5", "broken/model", "ERROR"} {
		if !strings.Contains(out, want) {
			t.Errorf("compare table missing %q:\n%s", want, out)
		}
	}
}

// TestCompareErrorsWhenEveryModelFails covers Fix 6's headline gap: before
// this, a --compare sweep where EVERY model failed still returned a nil
// error, indistinguishable at the call site from a sweep that genuinely
// succeeded. A partial failure (at least one model ok) must still report a
// nil error — each run's own Err/failed() already carries that signal.
func TestCompareErrorsWhenEveryModelFails(t *testing.T) {
	allFail := &fakeBackend{failFor: map[string]bool{"a": true, "b": true}}
	runs, err := Compare(context.Background(), allFail, AnalyzeRequest{Profile: &analyze.Profile{}}, []string{"a", "b"}, t.TempDir())
	if err == nil {
		t.Fatal("want an error when every model in the sweep failed")
	}
	if len(runs) != 2 {
		t.Fatalf("want a run per model even on total failure, got %d", len(runs))
	}

	partial := &fakeBackend{
		byModel: map[string]*Summary{"ok-model": {Analyzed: 1}},
		failFor: map[string]bool{"broken-model": true},
	}
	runs, err = Compare(context.Background(), partial, AnalyzeRequest{Profile: &analyze.Profile{}}, []string{"ok-model", "broken-model"}, t.TempDir())
	if err != nil {
		t.Fatalf("a partial failure (one model ok) must not error Compare itself: %v", err)
	}
	if len(runs) != 2 || runs[1].Err == nil {
		t.Fatalf("the failing model's own run must still record its error: %+v", runs)
	}
}

// TestRenderCompareDistinguishesZeroAnalyzedFailureFromOK covers Fix 6's
// render gap: a run with no Summary (or a Summary with zero Analyzed) that
// nonetheless recorded per-conversation failures rendered as "ok" before
// this fix — identical to a run that simply had zero eligible candidates.
func TestRenderCompareDistinguishesZeroAnalyzedFailureFromOK(t *testing.T) {
	var b bytes.Buffer
	err := RenderCompare(&b, []CompareRun{
		{Model: "quietly-failed", Summary: nil, Analyzed: 0, Failed: 3},
		{Model: "genuinely-empty", Summary: &Summary{Analyzed: 0, Skipped: 5}, Analyzed: 0, Failed: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "FAILED") {
		t.Errorf("a zero-analyzed run with recorded failures must render FAILED, got:\n%s", out)
	}
	if !strings.Contains(out, "genuinely-empty") {
		t.Errorf("missing genuinely-empty row:\n%s", out)
	}
}

func TestModelSlug(t *testing.T) {
	if got := modelSlug("~moonshotai/kimi-latest"); strings.ContainsAny(got, "/~") {
		t.Fatalf("slug %q must be path-safe", got)
	}
}
