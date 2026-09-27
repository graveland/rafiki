// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestReplaceFindingsPerAnalysisSkipsZeroRowsWithExistingFindings pins Fix 1's
// belt-and-braces guard directly at the unit that owns it: when ranked
// computes zero rows for an analysis that already HAS stored findings,
// replaceFindingsPerAnalysis must skip the store call (recording the skip)
// rather than calling store.ReplaceFindings and truncating the existing
// rows via its DELETE-then-INSERT.
func TestReplaceFindingsPerAnalysisSkipsZeroRowsWithExistingFindings(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	ctx := context.Background()
	convID := seedConversation(t, pool)

	analysisID, _, err := store.UpsertAnalysis(ctx, pool, store.AnalysisRow{
		ConversationID: convID, DetectorVersion: analyze.DetectorVersion, Model: "m", Status: "ok",
		Analysis: []byte(`{"conversation_id":"` + convID + `"}`),
	})
	c.Require().NoError(err)
	c.Require().NoError(store.ReplaceFindings(ctx, pool, analysisID, []store.FindingRow{{
		Axis: "grind", TopicKey: "loop", Title: "retry loop", ExpectedSavingsTokens: 100,
	}}, nil))
	before := snapshotAnalysisFindings(t, pool)
	c.Require().Len(before, 1, "seed stored %d finding rows, want 1", len(before))

	convs := []analyzedConversation{{conversationID: convID, analysisID: analysisID}}
	skipped, err := replaceFindingsPerAnalysis(ctx, pool, convs, nil)
	c.Require().NoError(err, "replaceFindingsPerAnalysis")
	c.Require().Len(skipped, 1, "skipped")
	if skipped[0].conversationID != convID || skipped[0].analysisID != analysisID || skipped[0].existing != 1 {
		t.Errorf("skipped[0] = %+v, want conversationID=%s analysisID=%s existing=1", skipped[0], convID, analysisID)
	}

	after := snapshotAnalysisFindings(t, pool)
	c.Require().Len(after, len(before), "analysis_finding row count changed despite the guard: before=%d after=%d", len(before), len(after))
	for i := range before {
		c.Eq(after[i], before[i], "analysis_finding row %d changed despite the guard: before=%+v after=", i, before[i])
	}
}

// TestReplaceFindingsPerAnalysisAllowsGenuineZeroFindings proves the guard
// doesn't overreach: an analysis that genuinely has zero findings to begin
// with (nothing to protect) must still allow the zero-rows replace through
// with no skip recorded.
func TestReplaceFindingsPerAnalysisAllowsGenuineZeroFindings(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newTestPool(t)
	ctx := context.Background()
	convID := seedConversation(t, pool)

	analysisID, _, err := store.UpsertAnalysis(ctx, pool, store.AnalysisRow{
		ConversationID: convID, DetectorVersion: analyze.DetectorVersion, Model: "m", Status: "ok",
		Analysis: []byte(`{"conversation_id":"` + convID + `"}`),
	})
	c.Require().NoError(err)

	convs := []analyzedConversation{{conversationID: convID, analysisID: analysisID}}
	skipped, err := replaceFindingsPerAnalysis(ctx, pool, convs, nil)
	c.Require().NoError(err, "replaceFindingsPerAnalysis")
	c.Empty(skipped, "skipped")
}
