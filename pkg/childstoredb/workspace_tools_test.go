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

// TestChildStoreWorkspaceToolsColumn pins the column's ride through the
// Postgres store: 'rafiki' written by one Upsert is read back, and a later
// Upsert that clears it writes ” — the NOT NULL default — rather than
// leaving the mode stuck on a child a preset or the operator has moved back to
// native.
func TestChildStoreWorkspaceToolsColumn(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)
	ctx := context.Background()

	id := "c_workspace_" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = s.Delete(ctx, id) })

	rec := childstore.ChildRecord{
		ChildID:   id,
		Kind:      protocol.KindClaude,
		Status:    string(protocol.StatusIdle),
		SpawnedAt: time.Now(),

		WorkspaceTools: protocol.WorkspaceToolsRafiki,
	}
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert rafiki")
	c.Eq(protocol.WorkspaceToolsRafiki, findRecord(t, s, id).WorkspaceTools, "stored WorkspaceTools")

	rec.WorkspaceTools = ""
	c.Require().NoError(s.Upsert(ctx, rec), "Upsert default")
	c.Eq("", findRecord(t, s, id).WorkspaceTools, "WorkspaceTools after reset to native")
}
