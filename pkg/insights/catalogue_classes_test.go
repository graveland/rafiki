// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedClassesCoordinator seeds a coordinator-shaped conversation for owner (a
// username): the seedConversation base plus one assistant message carrying an
// agent_spawn tool_use, the dispatch signal queryClasses classifies as
// coordinator.
func seedClassesCoordinator(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	convID := seedConversation(t, pool, "client", owner)
	insertMessage(t, pool, convID, 2, "assistant",
		`[{"type":"tool_use","name":"agent_spawn","input":{}}]`)
	return convID
}

// classesCells decodes one classes result row into
// (class, conversations, avg_turns, max_turns), failing the test on a shape
// mismatch rather than panicking.
func classesCells(t *testing.T, row []Entry) (string, int64, float64, int64) {
	t.Helper()
	if len(row) != 4 {
		t.Fatalf("classes row width = %d, want 4 columns", len(row))
	}
	class, ok := row[0].(StringEntry)
	if !ok {
		t.Fatalf("classes row[0] = %T, want StringEntry", row[0])
	}
	convs, ok := row[1].(IntEntry)
	if !ok {
		t.Fatalf("classes row[1] = %T, want IntEntry", row[1])
	}
	avg, ok := row[2].(FloatEntry)
	if !ok {
		t.Fatalf("classes row[2] = %T, want FloatEntry", row[2])
	}
	maxTurns, ok := row[3].(IntEntry)
	if !ok {
		t.Fatalf("classes row[3] = %T, want IntEntry", row[3])
	}
	return string(class), int64(convs), float64(avg), int64(maxTurns)
}

// TestQueryClassesDispatchOutranksBrainstorm seeds one conversation carrying
// BOTH a brainstorming skill call and an agent_spawn dispatch and pins the
// precedence order: it lands in coordinator, not brainstorming.
func TestQueryClassesDispatchOutranksBrainstorm(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversation(t, pool, "client", "alice-classes")
	insertMessage(t, pool, convID, 2, "assistant",
		`[{"type":"tool_use","name":"skill","input":{"skill":"rafiki:brainstorming"}},
		  {"type":"tool_use","name":"agent_spawn","input":{}}]`)

	res, err := New(pool).Query(ctx, ScopeAll(), "classes", StatsFilter{})
	if err != nil {
		t.Fatalf("classes query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("classes rows = %d (%+v), want 1 -- only this conversation has tool_use blocks", len(res.Rows), res.Rows)
	}
	class, convs, avgTurns, maxTurns := classesCells(t, res.Rows[0])
	if class != "coordinator" {
		t.Errorf("class = %q, want coordinator (dispatch outranks brainstorming)", class)
	}
	if convs != 1 {
		t.Errorf("convs = %d, want 1", convs)
	}
	// seedConversation gives the conversation exactly two turns.
	if avgTurns != 2 || maxTurns != 2 {
		t.Errorf("avg/max turns = %v/%d, want 2/2", avgTurns, maxTurns)
	}
}

// TestQueryClassesScopeOwnerExcludesOtherOwners seeds a coordinator-shaped
// conversation for bob and one for carol and pins the scope: querying as bob
// answers one row only, convs=1 -- carol's conversation is invisible.
func TestQueryClassesScopeOwnerExcludesOtherOwners(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedClassesCoordinator(t, pool, "bob")
	seedClassesCoordinator(t, pool, "carol")
	bobID := ensureUser(t, pool, "bob")

	res, err := New(pool).Query(ctx, ScopeOwner(bobID), "classes", StatsFilter{})
	if err != nil {
		t.Fatalf("classes query scoped to bob: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("classes rows = %d (%+v), want 1 -- carol's conversation must be excluded", len(res.Rows), res.Rows)
	}
	class, convs, _, _ := classesCells(t, res.Rows[0])
	if class != "coordinator" {
		t.Errorf("class = %q, want coordinator", class)
	}
	if convs != 1 {
		t.Errorf("convs = %d, want 1 (both conversations leak if this is 2)", convs)
	}
}
