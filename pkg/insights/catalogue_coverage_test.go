// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// coverageSeedConversation inserts a conversation with a given
// origin_entrypoint. helpers_test.go's insertConversation hardcodes 'test',
// and the coverage query filters on 'agent', so the fixture needs control
// over exactly that column.
func coverageSeedConversation(t *testing.T, pool *pgxpool.Pool, entrypoint, owner string) string {
	t.Helper()
	var ownerArg any
	if owner != "" {
		ownerArg = ensureUser(t, pool, owner)
	}
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (owner_user_id, persona, model, origin_entrypoint, driven_by)
		 VALUES ($1::uuid, 'team-platform', 'claude-fable-5', $2, 'client') RETURNING id::text`,
		ownerArg, entrypoint).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation")
	return id
}

// coverageSeedChild inserts a conversations.child row beside convID, the way
// a spawned agent child records itself. Only the NOT NULL columns are set --
// kind, status, spawned_at -- any kind/labels answer the coverage join.
func coverageSeedChild(t *testing.T, pool *pgxpool.Pool, convID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.child (child_id, conversation_id, kind, status, spawned_at)
		 VALUES ($1, $2::uuid, 'fundi', 'exited', now())`,
		"test-child-"+convID, convID)
	assert.NewAborting(t).NoError(err, "insert child row")
}

// coverageCellInt reads an IntEntry cell out of a coverage query row.
func coverageCellInt(t *testing.T, e Entry) int64 {
	t.Helper()
	v, ok := e.(IntEntry)
	assert.NewAborting(t).True(ok, "coverage cell is %T, want IntEntry", e)
	return int64(v)
}

// coverageCellFloat reads a FloatEntry cell out of a coverage query row.
func coverageCellFloat(t *testing.T, e Entry) float64 {
	t.Helper()
	v, ok := e.(FloatEntry)
	assert.NewAborting(t).True(ok, "coverage cell is %T, want FloatEntry", e)
	return float64(v)
}

// TestQueryCoverageComputesRatio seeds two agent-entrypoint conversations for
// the same owner in the same week, one with a conversations.child row and one
// without, and expects one weekly row with conversations=2, with_child=1 and
// coverage=0.5.
func TestQueryCoverageComputesRatio(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	withChild := coverageSeedConversation(t, pool, "agent", "coverage-carol")
	coverageSeedChild(t, pool, withChild)
	coverageSeedConversation(t, pool, "agent", "coverage-carol")

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "coverage-carol")), "coverage", StatsFilter{})
	c.Require().NoError(err, "query coverage")
	c.Require().Len(got.Rows, 1, "coverage query returned %d rows, want 1 (both conversations fall in the same week)", len(got.Rows))
	row := got.Rows[0]
	c.Eq(2, coverageCellInt(t, row[1]), "conversations")
	c.Eq(1, coverageCellInt(t, row[2]), "with_child")
	c.Eq(0.5, coverageCellFloat(t, row[3]), "coverage")
}

// TestQueryCoverageIgnoresNonAgentEntrypoints seeds a claude-entrypoint
// conversation alongside an agent-entrypoint one in the same week: only the
// agent one counts, since only agent-kind conversations ever get a child row
// by design.
func TestQueryCoverageIgnoresNonAgentEntrypoints(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	coverageSeedConversation(t, pool, "agent", "coverage-dave")
	coverageSeedConversation(t, pool, "claude", "coverage-dave")

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "coverage-dave")), "coverage", StatsFilter{})
	c.Require().NoError(err, "query coverage")
	c.Require().Len(got.Rows, 1, "coverage query returned %d rows, want 1", len(got.Rows))
	row := got.Rows[0]
	c.Eq(1, coverageCellInt(t, row[1]), "conversations")
	c.Eq(0, coverageCellInt(t, row[2]), "with_child")
	c.Eq(0.0, coverageCellFloat(t, row[3]), "coverage")
}

// TestQueryCoverageScopeOwnerExcludesOtherOwners is the standard scope-owner
// shape: a ScopeOwner(carol) coverage query over agent conversations owned by
// carol and erin counts only carol's.
func TestQueryCoverageScopeOwnerExcludesOtherOwners(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := newTestPool(t)
	carolConv := coverageSeedConversation(t, pool, "agent", "coverage-carol")
	coverageSeedChild(t, pool, carolConv)
	erinConv := coverageSeedConversation(t, pool, "agent", "coverage-erin")
	coverageSeedChild(t, pool, erinConv)

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "coverage-carol")), "coverage", StatsFilter{})
	c.Require().NoError(err, "query coverage scoped to carol")
	c.Require().Len(got.Rows, 1, "scoped coverage query returned %d rows, want 1 (carol's only)", len(got.Rows))
	row := got.Rows[0]
	c.Eq(1, coverageCellInt(t, row[1]), "conversations")
	c.Eq(1, coverageCellInt(t, row[2]), "with_child")
	c.Eq(1.0, coverageCellFloat(t, row[3]), "coverage")
}
