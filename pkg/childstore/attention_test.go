// SPDX-License-Identifier: Apache-2.0

package childstore

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// attentionFields is the three-field projection compared on every hop, so a
// failure names the fields rather than dumping the whole Session/Snapshot.
type attentionFields struct {
	PendingQuestion string
	LastTurnError   string
}

// TestAttentionFieldsSurviveSessionRecordRoundTrip pins the operator attention
// fields through the two conversions a resumed session traverses: Session ->
// Snapshot -> ChildRecord -> Session. Adding a struct field never breaks a
// literal that omits it, so a copy site forgotten in any of the three mappers
// silently drops the value; this test fails if any one of them is missing.
func TestAttentionFieldsSurviveSessionRecordRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)

	want := attentionFields{
		PendingQuestion: "Which database should I migrate first?",
		LastTurnError:   "upstream 502 from provider",
	}

	orig := &Session{
		ChildID:         "c_attention",
		Kind:            protocol.KindFundi,
		PendingQuestion: want.PendingQuestion,
		LastTurnError:   want.LastTurnError,
	}

	snap := orig.Snapshot()
	c.EqDeep(want, attentionFields{snap.PendingQuestion, snap.LastTurnError}, "Session -> Snapshot")

	rec := RecordFromSnapshot(snap)
	c.EqDeep(want, attentionFields{rec.PendingQuestion, rec.LastTurnError}, "Snapshot -> ChildRecord")

	got := SessionFromRecord(rec)
	c.EqDeep(want, attentionFields{got.PendingQuestion, got.LastTurnError}, "ChildRecord -> Session")

	// And the rebuilt session snapshots back to the same values.
	gotSnap := got.Snapshot()
	c.EqDeep(want, attentionFields{gotSnap.PendingQuestion, gotSnap.LastTurnError}, "Session -> Snapshot (rebuilt)")
}
