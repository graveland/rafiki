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

func TestChildstoreDBNotifyPolicyRoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_notify_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := childstore.ChildRecord{
		ChildID: id, Kind: protocol.KindFundi,
		Status:       string(protocol.StatusIdle),
		SpawnedAt:    time.Now(),
		NotifyPolicy: protocol.NotifyResult,
	}
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert result")
	c.Eq(protocol.NotifyResult, findRecord(t, s, id).NotifyPolicy, "stored NotifyPolicy")

	rec.NotifyPolicy = ""
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert default")
	c.Eq("", findRecord(t, s, id).NotifyPolicy, "NotifyPolicy after reset to default")
}
