// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
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
	if err != nil {
		t.Fatalf("insert child row: %v", err)
	}
}

// coverageCellInt reads an IntEntry cell out of a coverage query row.
func coverageCellInt(t *testing.T, e Entry) int64 {
	t.Helper()
	v, ok := e.(IntEntry)
	if !ok {
		t.Fatalf("coverage cell is %T, want IntEntry", e)
	}
	return int64(v)
}

// coverageCellFloat reads a FloatEntry cell out of a coverage query row.
func coverageCellFloat(t *testing.T, e Entry) float64 {
	t.Helper()
	v, ok := e.(FloatEntry)
	if !ok {
		t.Fatalf("coverage cell is %T, want FloatEntry", e)
	}
	return float64(v)
}

// TestQueryCoverageComputesRatio seeds two agent-entrypoint conversations for
// the same owner in the same week, one with a conversations.child row and one
// without, and expects one weekly row with conversations=2, with_child=1 and
// coverage=0.5.
func TestQueryCoverageComputesRatio(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	withChild := coverageSeedConversation(t, pool, "agent", "coverage-carol")
	coverageSeedChild(t, pool, withChild)
	coverageSeedConversation(t, pool, "agent", "coverage-carol")

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "coverage-carol")), "coverage", StatsFilter{})
	if err != nil {
		t.Fatalf("query coverage: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("coverage query returned %d rows, want 1 (both conversations fall in the same week): %+v",
			len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	if convs := coverageCellInt(t, row[1]); convs != 2 {
		t.Errorf("conversations = %d, want 2", convs)
	}
	if withChild := coverageCellInt(t, row[2]); withChild != 1 {
		t.Errorf("with_child = %d, want 1", withChild)
	}
	if cov := coverageCellFloat(t, row[3]); cov != 0.5 {
		t.Errorf("coverage = %v, want 0.5", cov)
	}
}

// TestQueryCoverageIgnoresNonAgentEntrypoints seeds a claude-entrypoint
// conversation alongside an agent-entrypoint one in the same week: only the
// agent one counts, since only agent-kind conversations ever get a child row
// by design.
func TestQueryCoverageIgnoresNonAgentEntrypoints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	coverageSeedConversation(t, pool, "agent", "coverage-dave")
	coverageSeedConversation(t, pool, "claude", "coverage-dave")

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "coverage-dave")), "coverage", StatsFilter{})
	if err != nil {
		t.Fatalf("query coverage: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("coverage query returned %d rows, want 1: %+v", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	if convs := coverageCellInt(t, row[1]); convs != 1 {
		t.Errorf("conversations = %d, want 1 (only the agent-entrypoint conversation counts)", convs)
	}
	if withChild := coverageCellInt(t, row[2]); withChild != 0 {
		t.Errorf("with_child = %d, want 0 (neither fixture got a child row)", withChild)
	}
	if cov := coverageCellFloat(t, row[3]); cov != 0.0 {
		t.Errorf("coverage = %v, want 0", cov)
	}
}

// TestQueryCoverageScopeOwnerExcludesOtherOwners is the standard scope-owner
// shape: a ScopeOwner(carol) coverage query over agent conversations owned by
// carol and erin counts only carol's.
func TestQueryCoverageScopeOwnerExcludesOtherOwners(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	carolConv := coverageSeedConversation(t, pool, "agent", "coverage-carol")
	coverageSeedChild(t, pool, carolConv)
	erinConv := coverageSeedConversation(t, pool, "agent", "coverage-erin")
	coverageSeedChild(t, pool, erinConv)

	got, err := New(pool).Query(ctx, ScopeOwner(ensureUser(t, pool, "coverage-carol")), "coverage", StatsFilter{})
	if err != nil {
		t.Fatalf("query coverage scoped to carol: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("scoped coverage query returned %d rows, want 1 (carol's only): %+v", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	if convs := coverageCellInt(t, row[1]); convs != 1 {
		t.Errorf("conversations = %d, want 1 (erin's row must be excluded by scope)", convs)
	}
	if withChild := coverageCellInt(t, row[2]); withChild != 1 {
		t.Errorf("with_child = %d, want 1", withChild)
	}
	if cov := coverageCellFloat(t, row[3]); cov != 1.0 {
		t.Errorf("coverage = %v, want 1", cov)
	}
}
