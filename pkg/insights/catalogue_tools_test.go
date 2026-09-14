// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"
)

func TestQueryToolsCountsByNameCasePreserved(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convID, 2, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convID, 3, "assistant", `[{"type":"tool_use","name":"Bash","input":{}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeAll(), "tools", StatsFilter{})
	if err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("tools rows = %d (%v), want 2", len(got.Rows), got.Rows)
	}
	calls := map[string]int64{}
	for _, row := range got.Rows {
		tool, ok := row[0].(StringEntry)
		if !ok {
			t.Fatalf("tool cell = %T, want StringEntry: %v", row[0], row)
		}
		n, ok := row[1].(IntEntry)
		if !ok {
			t.Fatalf("calls cell = %T, want IntEntry: %v", row[1], row)
		}
		convs, ok := row[2].(IntEntry)
		if !ok {
			t.Fatalf("convs cell = %T, want IntEntry: %v", row[2], row)
		}
		if convs != 1 {
			t.Errorf("tool %q convs = %d, want 1", tool, convs)
		}
		calls[string(tool)] = int64(n)
	}
	// Casing is preserved DELIBERATELY: "Bash" (Claude Code, seen through the
	// proxy) and "bash" (fundi) stay separate rows.
	if calls["bash"] != 1 || calls["Bash"] != 1 {
		t.Fatalf("calls by tool = %v, want one bash and one Bash, each 1", calls)
	}
}

func TestQueryToolsScopeOwnerExcludesOtherOwners(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	bobID := ensureUser(t, pool, "bob")
	convBob := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convBob, 2, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	convCarol := seedConversation(t, pool, "client", "carol")
	insertMessage(t, pool, convCarol, 2, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeOwner(bobID), "tools", StatsFilter{})
	if err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("tools rows for bob = %d (%v), want exactly 1", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	tool, ok := row[0].(StringEntry)
	if !ok || string(tool) != "bash" {
		t.Fatalf("tool = %v, want bash", row[0])
	}
	if n, ok := row[1].(IntEntry); !ok || n != 1 {
		t.Fatalf("calls = %v, want 1", row[1])
	}
	if convs, ok := row[2].(IntEntry); !ok || convs != 1 {
		t.Fatalf("convs = %v, want 1 (bob's conversation only)", row[2])
	}
}

func TestQueryToolsIgnoresBareStringMessages(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversation(t, pool, "client", "bob")
	// content is a bare JSON STRING, not an array: the string-or-array guard
	// must not error on it, and a tool call inside a string-shaped message is
	// invisible -- the lossiness the Global Constraints accept.
	insertMessage(t, pool, convID, 2, "assistant", `"a tool_use call lost inside a plain string mentioning bash"`)
	insertMessage(t, pool, convID, 3, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeAll(), "tools", StatsFilter{})
	if err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("tools rows = %d (%v), want exactly 1 (only the array message)", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	if tool, ok := row[0].(StringEntry); !ok || string(tool) != "bash" {
		t.Fatalf("tool = %v, want bash", row[0])
	}
	if n, ok := row[1].(IntEntry); !ok || n != 1 {
		t.Fatalf("calls = %v, want 1", row[1])
	}
}
