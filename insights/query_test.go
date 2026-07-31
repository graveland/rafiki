// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// selectOnly is a stand-in for the server's cgo pg_query validator: it only has
// to reject non-SELECT text for these tests.
func selectOnly(s string) error {
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "SELECT") {
		return errors.New("read-only SELECT required")
	}
	return nil
}

func TestQuery_RejectsNonSelect(t *testing.T) {
	ctx := context.Background()
	ins := New(newTestPool(t))
	_, _, err := ins.Query(ctx, "DELETE FROM conversations.conversation", 10, selectOnly)
	if err == nil {
		t.Fatal("non-SELECT must be rejected by the validator")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %v, want it to mention rejection", err)
	}
}

func TestQuery_ReturnsRowsAndTruncates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice")
	seedConversation(t, pool, "server", "bob")
	ins := New(pool)

	all, truncated, err := ins.Query(ctx,
		"SELECT id::text, owner FROM conversations.conversation ORDER BY owner", 10, selectOnly)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false (2 rows under a limit of 10)")
	}
	if len(all) != 2 {
		t.Fatalf("rows = %d, want 2", len(all))
	}
	if all[0]["owner"] != "alice" {
		t.Errorf("first owner = %v, want alice", all[0]["owner"])
	}

	capped, truncated, err := ins.Query(ctx,
		"SELECT id::text FROM conversations.conversation", 1, selectOnly)
	if err != nil {
		t.Fatalf("query capped: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true (2 rows under a limit of 1)")
	}
	if len(capped) != 1 {
		t.Errorf("capped rows = %d, want 1", len(capped))
	}
}

func TestQuery_ReadOnlyTxBlocksWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice")
	ins := New(pool)

	// Validator waved through: the read-only transaction must still refuse the
	// write at the database layer (defence in depth).
	_, _, err := ins.Query(ctx,
		"DELETE FROM conversations.conversation", 10, func(string) error { return nil })
	if err == nil {
		t.Fatal("write must fail inside the read-only transaction even without validation")
	}
}

func TestClampQueryLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, defaultQueryLimit},
		{-5, defaultQueryLimit},
		{50, 50},
		{maxQueryLimit, maxQueryLimit},
		{maxQueryLimit + 1, maxQueryLimit}, // clamp DOWN to max, not reset to default
		{999999, maxQueryLimit},
	}
	for _, c := range cases {
		if got := clampQueryLimit(c.in); got != c.want {
			t.Errorf("clampQueryLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestQuery_UUIDColumnIsString(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice")
	ins := New(pool)

	// SELECT id WITHOUT ::text: the raw uuid must still render as a string.
	rows, _, err := ins.Query(ctx, "SELECT id FROM conversations.conversation LIMIT 1", 10, selectOnly)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	id, ok := rows[0]["id"].(string)
	if !ok {
		t.Fatalf("id is %T, want string", rows[0]["id"])
	}
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("id %q is not a canonical uuid string", id)
	}
}

func TestQuery_ByteBudgetTruncates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ins := New(pool)

	// Three ~2MB rows: the first fits, the second would blow the 3MB budget.
	rows, truncated, err := ins.Query(ctx,
		"SELECT repeat('x', 2000000) AS big FROM generate_series(1,3)", 10, selectOnly)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true (byte budget hit)")
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (byte budget stops after the first row)", len(rows))
	}
}

func TestQuery_NilValidatorRefused(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	_, _, err := New(pool).Query(ctx, "SELECT 1", 10, nil)
	if err == nil || !strings.Contains(err.Error(), "requires a validator") {
		t.Fatalf("Query with nil validator err = %v, want fail-closed refusal", err)
	}
}
