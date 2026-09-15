// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"
)

func TestQueryToolsMergesCasingUnderTheDominantSpelling(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convID, 2, "assistant", `[{"type":"tool_use","name":"Bash","input":{}}]`)
	insertMessage(t, pool, convID, 3, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convID, 4, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeAll(), "tools", StatsFilter{})
	if err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("tools rows = %d (%v), want 1 (casing merged)", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	// Casing is merged: "Bash" (Claude Code, seen through the proxy) and
	// "bash" (fundi) are one row, displayed under the spelling that carried
	// more calls -- here bash, 2 calls against Bash's 1.
	if tool, ok := row[0].(StringEntry); !ok || string(tool) != "bash" {
		t.Fatalf("tool = %v, want bash (dominant spelling)", row[0])
	}
	if n, ok := row[1].(IntEntry); !ok || n != 3 {
		t.Fatalf("calls = %v, want 3 (merged across spellings)", row[1])
	}
	if convs, ok := row[2].(IntEntry); !ok || convs != 1 {
		t.Fatalf("convs = %v, want 1", row[2])
	}
}

// Postgres mode() is deterministic on a tie: the sort-first spelling wins
// ('B' sorts before 'b' in byte order). The test pins that determinism so a
// silent switch to something arbitrary would show up as a flake.
func TestQueryToolsCasingTieIsDeterministic(t *testing.T) {
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
	if len(got.Rows) != 1 {
		t.Fatalf("tools rows = %d (%v), want 1", len(got.Rows), got.Rows)
	}
	if tool, ok := got.Rows[0][0].(StringEntry); !ok || string(tool) != "Bash" {
		t.Fatalf("tool = %v, want Bash (sort-first spelling wins the tie)", got.Rows[0][0])
	}
}

// Rows sort by CALLS descending, never by conversations: grep spans two
// conversations (one call each) while bash fires three calls in one, and
// bash must come first.
func TestQueryToolsSortsByCallsNotConversations(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convA := seedConversation(t, pool, "client", "bob")
	convB := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convA, 2, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convA, 3, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convA, 4, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convB, 2, "assistant", `[{"type":"tool_use","name":"grep","input":{}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeAll(), "tools", StatsFilter{})
	if err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("tools rows = %d (%v), want 2", len(got.Rows), got.Rows)
	}
	first, ok := got.Rows[0][0].(StringEntry)
	if !ok || string(first) != "bash" {
		t.Fatalf("first row = %v, want bash (3 calls beats grep's 2 conversations)", got.Rows[0])
	}
	if n, ok := got.Rows[0][1].(IntEntry); !ok || n != 3 {
		t.Fatalf("bash calls = %v, want 3", got.Rows[0][1])
	}
}

// The conversations column counts DISTINCT conversations over the whole
// merged group. Summing per-spelling counts would answer 6 here (a
// conversation that used both spellings counts once per spelling); the
// merged answer is 3.
func TestQueryToolsMergesConversationCounts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convA := seedConversation(t, pool, "client", "bob")
	convB := seedConversation(t, pool, "client", "bob")
	convC := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convA, 2, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convB, 2, "assistant", `[{"type":"tool_use","name":"Bash","input":{}}]`)
	insertMessage(t, pool, convC, 2, "assistant", `[{"type":"tool_use","name":"bash","input":{}}]`)
	insertMessage(t, pool, convC, 3, "assistant", `[{"type":"tool_use","name":"Bash","input":{}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeAll(), "tools", StatsFilter{})
	if err != nil {
		t.Fatalf("query tools: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("tools rows = %d (%v), want 1", len(got.Rows), got.Rows)
	}
	if n, ok := got.Rows[0][1].(IntEntry); !ok || n != 4 {
		t.Fatalf("calls = %v, want 4", got.Rows[0][1])
	}
	if convs, ok := got.Rows[0][2].(IntEntry); !ok || convs != 3 {
		t.Fatalf("convs = %v, want 3 (distinct conversations, never a per-spelling sum)", got.Rows[0][2])
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
