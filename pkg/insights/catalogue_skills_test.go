// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"testing"
)

func TestQuerySkillsNormalizesNamespacePrefix(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convID, 2, "assistant", `[{"type":"tool_use","name":"skill","input":{"skill":"rafiki:brainstorming"}}]`)
	insertMessage(t, pool, convID, 3, "assistant", `[{"type":"tool_use","name":"skill","input":{"skill":"superpowers:brainstorming"}}]`)
	insertMessage(t, pool, convID, 4, "assistant", `[{"type":"tool_use","name":"skill","input":{"skill":"brainstorming"}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeAll(), "skills", StatsFilter{})
	if err != nil {
		t.Fatalf("query skills: %v", err)
	}
	// All three spellings collapse into one row: the namespace prefix
	// (everything up to and including the last ':') is stripped before
	// grouping.
	if len(got.Rows) != 1 {
		t.Fatalf("skills rows = %d (%v), want exactly 1", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	skill, ok := row[0].(StringEntry)
	if !ok || string(skill) != "brainstorming" {
		t.Fatalf("skill = %v, want brainstorming", row[0])
	}
	if n, ok := row[1].(IntEntry); !ok || n != 3 {
		t.Fatalf("invocations = %v, want 3", row[1])
	}
	if convs, ok := row[2].(IntEntry); !ok || convs != 1 {
		t.Fatalf("convs = %v, want 1", row[2])
	}
}

func TestQuerySkillsScopeOwnerExcludesOtherOwners(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	bobID := ensureUser(t, pool, "bob")
	convBob := seedConversation(t, pool, "client", "bob")
	insertMessage(t, pool, convBob, 2, "assistant", `[{"type":"tool_use","name":"skill","input":{"skill":"rafiki:brainstorming"}}]`)
	convCarol := seedConversation(t, pool, "client", "carol")
	insertMessage(t, pool, convCarol, 2, "assistant", `[{"type":"tool_use","name":"skill","input":{"skill":"rafiki:brainstorming"}}]`)
	ins := New(pool)

	got, err := ins.Query(ctx, ScopeOwner(bobID), "skills", StatsFilter{})
	if err != nil {
		t.Fatalf("query skills: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("skills rows for bob = %d (%v), want exactly 1", len(got.Rows), got.Rows)
	}
	row := got.Rows[0]
	skill, ok := row[0].(StringEntry)
	if !ok || string(skill) != "brainstorming" {
		t.Fatalf("skill = %v, want brainstorming", row[0])
	}
	if n, ok := row[1].(IntEntry); !ok || n != 1 {
		t.Fatalf("invocations = %v, want 1", row[1])
	}
	if convs, ok := row[2].(IntEntry); !ok || convs != 1 {
		t.Fatalf("convs = %v, want 1 (bob's conversation only)", row[2])
	}
}
