// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/store"
)

// TestReplaceFindingsPerAnalysisSkipsZeroRowsWithExistingFindings pins Fix 1's
// belt-and-braces guard directly at the unit that owns it: when ranked
// computes zero rows for an analysis that already HAS stored findings,
// replaceFindingsPerAnalysis must skip the store call (recording the skip)
// rather than calling store.ReplaceFindings and truncating the existing
// rows via its DELETE-then-INSERT.
func TestReplaceFindingsPerAnalysisSkipsZeroRowsWithExistingFindings(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convID := seedConversation(t, pool)

	analysisID, _, err := store.UpsertAnalysis(ctx, pool, store.AnalysisRow{
		ConversationID: convID, DetectorVersion: analyze.DetectorVersion, Model: "m", Status: "ok",
		Analysis: []byte(`{"conversation_id":"` + convID + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceFindings(ctx, pool, analysisID, []store.FindingRow{{
		Axis: "grind", TopicKey: "loop", Title: "retry loop", ExpectedSavingsTokens: 100,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	before := snapshotAnalysisFindings(t, pool)
	if len(before) != 1 {
		t.Fatalf("seed stored %d finding rows, want 1", len(before))
	}

	convs := []analyzedConversation{{conversationID: convID, analysisID: analysisID}}
	skipped, err := replaceFindingsPerAnalysis(ctx, pool, convs, nil)
	if err != nil {
		t.Fatalf("replaceFindingsPerAnalysis: %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly 1 entry", skipped)
	}
	if skipped[0].conversationID != convID || skipped[0].analysisID != analysisID || skipped[0].existing != 1 {
		t.Errorf("skipped[0] = %+v, want conversationID=%s analysisID=%s existing=1", skipped[0], convID, analysisID)
	}

	after := snapshotAnalysisFindings(t, pool)
	if len(after) != len(before) {
		t.Fatalf("analysis_finding row count changed despite the guard: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("analysis_finding row %d changed despite the guard: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

// TestReplaceFindingsPerAnalysisAllowsGenuineZeroFindings proves the guard
// doesn't overreach: an analysis that genuinely has zero findings to begin
// with (nothing to protect) must still allow the zero-rows replace through
// with no skip recorded.
func TestReplaceFindingsPerAnalysisAllowsGenuineZeroFindings(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	convID := seedConversation(t, pool)

	analysisID, _, err := store.UpsertAnalysis(ctx, pool, store.AnalysisRow{
		ConversationID: convID, DetectorVersion: analyze.DetectorVersion, Model: "m", Status: "ok",
		Analysis: []byte(`{"conversation_id":"` + convID + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	convs := []analyzedConversation{{conversationID: convID, analysisID: analysisID}}
	skipped, err := replaceFindingsPerAnalysis(ctx, pool, convs, nil)
	if err != nil {
		t.Fatalf("replaceFindingsPerAnalysis: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %+v, want none — this analysis genuinely had zero findings to begin with", skipped)
	}
}
