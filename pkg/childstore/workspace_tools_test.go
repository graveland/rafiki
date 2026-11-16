// SPDX-License-Identifier: Apache-2.0

package childstore

import (
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestWorkspaceToolsSurvivesRoundTrip pins the field's ride through
// SessionFromRecord → Snapshot → RecordFromSnapshot, so a child resolved into
// rafiki mode at spawn keeps the mode across a resume rebuild.
func TestWorkspaceToolsSurvivesRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	for _, mode := range []string{"", protocol.WorkspaceToolsRafiki} {
		rec := ChildRecord{ChildID: "workspace-child", WorkspaceTools: mode}
		snap := SessionFromRecord(rec).Snapshot()
		c.Eq(mode, snap.WorkspaceTools, "Snapshot.WorkspaceTools")
		c.Eq(mode, RecordFromSnapshot(snap).WorkspaceTools, "RecordFromSnapshot.WorkspaceTools")
	}
}
