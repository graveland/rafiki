// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestSetExecutorSetWritesOnlyTheSetOfALiveRow pins the targeted write: the set
// changes and the status a concurrent writer recorded does not.
func TestSetExecutorSetWritesOnlyTheSetOfALiveRow(t *testing.T) {
	c := assert.NewCollecting(t)
	s := New(testPool(t))
	ctx := context.Background()

	id := "c_setwrite_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })
	c.Require().NoError(s.Upsert(ctx, childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi, Status: string(protocol.StatusExited),
		SpawnedAt: time.Now(), ExecutorSet: []string{"A", "B"},
	}), "seed row")

	c.Require().NoError(s.SetExecutorSet(ctx, id, []string{"A"}), "set")
	got := findRecord(t, s, id)
	c.EqDeep([]string{"A"}, got.ExecutorSet, "set written")
	c.Eq(string(protocol.StatusExited), got.Status, "status untouched")

	c.Require().NoError(s.SetExecutorSet(ctx, id, nil), "nil set")
	c.True(findRecord(t, s, id).ExecutorSet == nil, "nil stays a legacy NULL")
}

// TestSetExecutorSetNeverReopensAClosedRow is the late-write regression: an
// asynchronous set update that lands after Close must leave the child closed.
func TestSetExecutorSetNeverReopensAClosedRow(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_setclosed_" + time.Now().Format("150405.000000")
	c.Require().NoError(s.Upsert(ctx, childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi, Status: string(protocol.StatusExited),
		SpawnedAt: time.Now(), ExecutorSet: []string{"A", "B"},
	}), "seed row")
	c.Require().NoError(s.Delete(ctx, id), "close")

	c.Require().NoError(s.SetExecutorSet(ctx, id, []string{"A"}), "late set write")

	var closed bool
	var set []string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT closed_at IS NOT NULL, executor_set FROM conversations.child WHERE child_id = $1`, id).
		Scan(&closed, &set), "read row")
	c.True(closed, "the row is still closed")
	c.EqDeep([]string{"A", "B"}, set, "a closed row's set is not rewritten")
	_, listed := lookup(t, s, id)
	c.True(!listed, "a closed row must not be listed")

	c.NoError(s.SetExecutorSet(ctx, "c_missing_"+id, []string{"A"}), "a missing row is a no-op")
}
