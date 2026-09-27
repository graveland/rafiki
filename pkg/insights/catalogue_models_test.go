// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// modelsCellString reads a StringEntry cell out of a models query row.
func modelsCellString(t *testing.T, e Entry) string {
	t.Helper()
	v, ok := e.(StringEntry)
	assert.NewAborting(t).True(ok, "models cell is %T, want StringEntry", e)
	return string(v)
}

// modelsCellInt reads an IntEntry cell out of a models query row.
func modelsCellInt(t *testing.T, e Entry) int64 {
	t.Helper()
	v, ok := e.(IntEntry)
	assert.NewAborting(t).True(ok, "models cell is %T, want IntEntry", e)
	return int64(v)
}

// TestQueryModelsGroupsByModelAcrossTurns seeds one conversation with three
// turns on "claude-sonnet-5" and one turn on "z-ai/glm-5.3-flash" and expects
// the distribution grouped by model: two rows, turns summed per model. The
// rows are matched by model rather than position: both rows carry
// conversations=1, so ORDER BY conversations DESC leaves their relative order
// to Postgres.
func TestQueryModelsGroupsByModelAcrossTurns(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "client", "models-alice")
	insertTurn(t, pool, convID, seedTurn{ordinal: 0, model: "claude-sonnet-5"})
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "claude-sonnet-5"})
	insertTurn(t, pool, convID, seedTurn{ordinal: 2, model: "claude-sonnet-5"})
	insertTurn(t, pool, convID, seedTurn{ordinal: 3, model: "z-ai/glm-5.3-flash"})

	got, err := New(pool).Query(ctx, ScopeAll(), "models", StatsFilter{})
	c.Require().NoError(err, "query models")
	c.Require().Len(got.Rows, 2, "models query returned %d rows, want 2", len(got.Rows))
	byModel := map[string][2]int64{} // model -> (conversations, turns)
	for _, row := range got.Rows {
		byModel[modelsCellString(t, row[0])] = [2]int64{modelsCellInt(t, row[1]), modelsCellInt(t, row[2])}
	}
	c.Eq([2]int64{1, 3}, byModel["claude-sonnet-5"], "claude-sonnet-5")
	c.Eq([2]int64{1, 1}, byModel["z-ai/glm-5.3-flash"], "z-ai/glm-5.3-flash")
}

// TestQueryModelsScopeOwnerExcludesOtherOwners is the standard scope-owner
// shape: a ScopeOwner(bob) query over conversations owned by bob and carol
// yields exactly bob's rows -- scope ANDs with the query, it does not widen it.
func TestQueryModelsScopeOwnerExcludesOtherOwners(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	bobConv := insertConversation(t, pool, "client", "models-bob")
	insertTurn(t, pool, bobConv, seedTurn{ordinal: 0, model: "scope-model-bob"})
	carolConv := insertConversation(t, pool, "client", "models-carol")
	insertTurn(t, pool, carolConv, seedTurn{ordinal: 0, model: "scope-model-carol"})

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "models-bob")), "models", StatsFilter{})
	c.Require().NoError(err, "query models scoped to bob")
	c.Require().Len(got.Rows, 1, "scoped models query returned %d rows, want 1 (bob's only)", len(got.Rows))
	c.Eq("scope-model-bob", modelsCellString(t, got.Rows[0][0]), "model")
}
