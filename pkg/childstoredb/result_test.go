// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestResultRoundTrip pins the durable half of SetResult (wave 2, item 2.3):
// the result column stores the verbatim JSON, last write wins, and the value
// survives the record->session->snapshot->record round trip a resume takes.
// A write whose record carries no result DOES clear the column — that is the
// same plain-assignment rule every other non-COALESCE column follows, and is
// safe because RecordFromSnapshot always carries the session's current value.
func TestResultRoundTrip(t *testing.T) {
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
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if got := findRecord(t, s, id).Result; got != "" {
		t.Errorf("unset Result = %q, want empty", got)
	}

	rec.Result = `{"answer":42}`
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if got := findRecord(t, s, id).Result; got != `{"answer":42}` {
		t.Errorf("Result = %q, want the stored JSON", got)
	}

	// Last write wins.
	rec.Result = `{"answer":43}`
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	if got := findRecord(t, s, id).Result; got != `{"answer":43}` {
		t.Errorf("Result = %q, want the last write", got)
	}

	// The round trip through Session rebuilds the same value.
	sess := childstore.SessionFromRecord(findRecord(t, s, id))
	if sess.Result != `{"answer":43}` {
		t.Errorf("SessionFromRecord.Result = %q", sess.Result)
	}
	snap := sess.Snapshot()
	if snap.Result != `{"answer":43}` {
		t.Errorf("Snapshot.Result = %q", snap.Result)
	}
	if rec2 := childstore.RecordFromSnapshot(snap); rec2.Result != `{"answer":43}` {
		t.Errorf("RecordFromSnapshot.Result = %q", rec2.Result)
	}
}
