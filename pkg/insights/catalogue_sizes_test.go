// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedSizesWorker seeds a worker/other-shaped conversation for owner (a
// username) carrying EXACTLY n turns. The conversation rows start empty
// (insertConversation, not seedConversation, whose own two turns would corrupt
// a stated count), the n turn rows mirror stats_test.go's seedTurns insert
// shape -- insertTurn, its exact INSERT statement, looped with the turn COUNT
// as the stated quantity rather than the token values -- and one plain bash
// tool_use message is added so the conversation reaches the cls CTE at all: a
// conversation with no tool_use blocks is invisible to both catalogue queries.
// The bash call dispatches, brainstorms and plans nothing, so the conversation
// classifies as worker/other.
func seedSizesWorker(t *testing.T, pool *pgxpool.Pool, owner string, turns int) string {
	t.Helper()
	convID := insertConversation(t, pool, "client", owner)
	for i := 0; i < turns; i++ {
		insertTurn(t, pool, convID, seedTurn{
			ordinal: i, model: "claude-fable-5", source: "claude", upstream: "anthropic",
			inTok: 100, outTok: 50, cacheRead: 0, latencyMS: 1000,
		})
	}
	insertMessage(t, pool, convID, 0, "assistant",
		`[{"type":"tool_use","name":"bash","input":{"command":"true"}}]`)
	return convID
}

// sizesCells decodes one sizes result row into (class, size_bucket,
// conversations), failing the test on a shape mismatch rather than panicking.
func sizesCells(t *testing.T, row []Entry) (string, string, int64) {
	t.Helper()
	if len(row) != 3 {
		t.Fatalf("sizes row width = %d, want 3 columns", len(row))
	}
	class, ok := row[0].(StringEntry)
	if !ok {
		t.Fatalf("sizes row[0] = %T, want StringEntry", row[0])
	}
	bucket, ok := row[1].(StringEntry)
	if !ok {
		t.Fatalf("sizes row[1] = %T, want StringEntry", row[1])
	}
	n, ok := row[2].(IntEntry)
	if !ok {
		t.Fatalf("sizes row[2] = %T, want IntEntry", row[2])
	}
	return string(class), string(bucket), int64(n)
}

// TestQuerySizesBucketsByTurnCount seeds three worker/other conversations of
// 10, 60 and 300 turns and pins the histogram: one row per conversation,
// bucketed <25, 25-100 and 250-500 respectively. Rows come back class
// alphabetical and, within a class, in sizeBucketOrder -- the exact sequence
// asserted here.
func TestQuerySizesBucketsByTurnCount(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedSizesWorker(t, pool, "hank-sizes", 10)
	seedSizesWorker(t, pool, "iris-sizes", 60)
	seedSizesWorker(t, pool, "june-sizes", 300)

	res, err := New(pool).Query(ctx, ScopeAll(), "sizes", StatsFilter{})
	if err != nil {
		t.Fatalf("sizes query: %v", err)
	}
	want := []struct {
		class, bucket string
		n             int64
	}{
		{"worker/other", "<25", 1},
		{"worker/other", "25-100", 1},
		{"worker/other", "250-500", 1},
	}
	if len(res.Rows) != len(want) {
		t.Fatalf("sizes rows = %d (%+v), want %d -- one bucket row per conversation", len(res.Rows), res.Rows, len(want))
	}
	for i, w := range want {
		class, bucket, n := sizesCells(t, res.Rows[i])
		if class != w.class || bucket != w.bucket || n != w.n {
			t.Errorf("sizes row %d = (%q, %q, %d), want (%q, %q, %d)", i, class, bucket, n, w.class, w.bucket, w.n)
		}
	}
}

// TestQuerySizesScopeOwnerExcludesOtherOwners is the same shape as the classes
// scope test: one sized worker conversation each for bob and carol, queried as
// bob, answering one row only -- carol's is invisible.
func TestQuerySizesScopeOwnerExcludesOtherOwners(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedSizesWorker(t, pool, "bob", 30)
	seedSizesWorker(t, pool, "carol", 30)
	bobID := ensureUser(t, pool, "bob")

	res, err := New(pool).Query(ctx, ScopeOwner(bobID), "sizes", StatsFilter{})
	if err != nil {
		t.Fatalf("sizes query scoped to bob: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("sizes rows = %d (%+v), want 1 -- carol's conversation must be excluded", len(res.Rows), res.Rows)
	}
	class, bucket, n := sizesCells(t, res.Rows[0])
	if class != "worker/other" {
		t.Errorf("class = %q, want worker/other", class)
	}
	if bucket != "25-100" {
		t.Errorf("bucket = %q, want 25-100 (30 turns)", bucket)
	}
	if n != 1 {
		t.Errorf("conversations = %d, want 1 (both conversations leak if this is 2)", n)
	}
}
