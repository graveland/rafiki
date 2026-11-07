// SPDX-License-Identifier: Apache-2.0

package childstore

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestChildstoreRecordRoundTripsNotifyPolicy(t *testing.T) {
	c := assert.NewAborting(t)
	for _, policy := range []string{"", protocol.NotifyResult} {
		rec := ChildRecord{ChildID: "quiet-child", NotifyPolicy: policy}
		got := RecordFromSnapshot(SessionFromRecord(rec).Snapshot())
		c.Eq(got.NotifyPolicy, policy)
	}
}
