// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestResultRoundTrip pins the durable half of SetResult (wave 2, item 2.3):
// the result column stores the verbatim JSON, last write wins, and the value
// survives the record->session->snapshot->record round trip a resume takes.
// A write whose record carries no result DOES clear the column — that is the
// same plain-assignment rule every other non-COALESCE column follows, and is
// safe because RecordFromSnapshot always carries the session's current value.
func TestResultRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_result_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status:    string(protocol.StatusIdle),
		SpawnedAt: time.Now(),
	}
	c.Require().NoError(s.Upsert(ctx, rec), "first Upsert")
	c.Eq("", findRecord(t, s, id).Result, "unset Result")

	rec.Result = `{"answer":42}`
	c.Require().NoError(s.Upsert(ctx, rec), "second Upsert")
	c.Eq(`{"answer":42}`, findRecord(t, s, id).Result, "Result")

	// Last write wins.
	rec.Result = `{"answer":43}`
	c.Require().NoError(s.Upsert(ctx, rec), "third Upsert")
	c.Eq(`{"answer":43}`, findRecord(t, s, id).Result, "Result")

	// The round trip through Session rebuilds the same value.
	sess := childstore.SessionFromRecord(findRecord(t, s, id))
	c.Eq(`{"answer":43}`, sess.Result, "SessionFromRecord.Result =")
	snap := sess.Snapshot()
	c.Eq(`{"answer":43}`, snap.Result, "Snapshot.Result =")
	if rec2 := childstore.RecordFromSnapshot(snap); rec2.Result != `{"answer":43}` {
		t.Errorf("RecordFromSnapshot.Result = %q", rec2.Result)
	}
}
