// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestScopeAllCond pins the all-owners scope: it renders a bare always-true
// condition and must append no parameter.
func TestScopeAllCond(t *testing.T) {
	a := &argList{}
	if got := ScopeAll().cond(a, "c.owner_user_id", "c.id", "c.external_ref"); got != "1=1" {
		t.Fatalf("ScopeAll().cond() = %q, want %q", got, "1=1")
	}
	if len(a.args) != 0 {
		t.Fatalf("ScopeAll().cond() appended %d args, want 0", len(a.args))
	}
}

// TestScopeOwnerCond pins the per-owner scope: the owner id rides a
// positional parameter, cast to uuid, over the given column.
func TestScopeOwnerCond(t *testing.T) {
	a := &argList{}
	got := ScopeOwner("abc").cond(a, "c.owner_user_id", "c.id", "c.external_ref")
	if got != "c.owner_user_id = $1::uuid" {
		t.Fatalf("ScopeOwner().cond() = %q, want %q", got, "c.owner_user_id = $1::uuid")
	}
	if len(a.args) != 1 || a.args[0] != "abc" {
		t.Fatalf("cond() args = %v, want [abc]", a.args)
	}
}

// TestScopeZeroValueCond is the load-bearing test: the zero value DENIES. A
// Scope{} that reached a query builder must render an always-false
// condition, never behave like "no filter".
func TestScopeZeroValueCond(t *testing.T) {
	a := &argList{}
	if got := (Scope{}).cond(a, "c.owner_user_id", "c.id", "c.external_ref"); got != "1=0" {
		t.Fatalf("(Scope{}).cond() = %q, want %q", got, "1=0")
	}
	if len(a.args) != 0 {
		t.Fatalf("Scope{}.cond() appended %d args, want 0", len(a.args))
	}
}

// TestScopeOwnerEmptyIDIsInvalid pins that an empty owner id is not an
// owner: ScopeOwner("") is invalid and fails closed.
func TestScopeOwnerEmptyIDIsInvalid(t *testing.T) {
	s := ScopeOwner("")
	if s.valid() {
		t.Fatal("ScopeOwner(\"\").valid() = true, want false")
	}
	a := &argList{}
	if got := s.cond(a, "c.owner_user_id", "c.id", "c.external_ref"); got != "1=0" {
		t.Fatalf("ScopeOwner(\"\").cond() = %q, want %q", got, "1=0")
	}
	if len(a.args) != 0 {
		t.Fatalf("ScopeOwner(\"\").cond() appended %d args, want 0", len(a.args))
	}
}

// TestScopeAllIsValid pins that the all-owners constructor builds a valid
// scope.
func TestScopeAllIsValid(t *testing.T) {
	if !ScopeAll().valid() {
		t.Fatal("ScopeAll().valid() = false, want true")
	}
}

// TestScopeOwnerIsValid pins that the per-owner constructor with a
// non-empty id builds a valid scope.
func TestScopeOwnerIsValid(t *testing.T) {
	if !ScopeOwner("abc").valid() {
		t.Fatal("ScopeOwner(\"abc\").valid() = false, want true")
	}
}

// TestScopeSubtreeCond pins the subtree scope's three correlation arms in one
// condition — conversation ids, external_refs, external_ref prefixes — the
// same clause SubtreeCost rolls spend up with, over the columns the caller
// names.
func TestScopeSubtreeCond(t *testing.T) {
	a := &argList{}
	sel := SubtreeSelector{
		ConversationIDs:     []string{"uuid-1"},
		ExternalRefs:        []string{"c_01"},
		ExternalRefPrefixes: []string{"c_01:"},
	}
	got := ScopeSubtree(sel).cond(a, "c.owner_user_id", "c.id", "c.external_ref")
	for _, want := range []string{
		"c.id = ANY($1::uuid[])",
		"c.external_ref = ANY($2::text[])",
		"starts_with(c.external_ref, p)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cond() = %q, want it to contain %q", got, want)
		}
	}
	if len(a.args) != 3 {
		t.Fatalf("cond() appended %d args, want 3", len(a.args))
	}
	for i, want := range [][]string{{"uuid-1"}, {"c_01"}, {"c_01:"}} {
		gotArg, ok := a.args[i].([]string)
		if !ok || len(gotArg) != len(want) || gotArg[0] != want[0] {
			t.Fatalf("cond() arg %d = %#v, want %v", i, a.args[i], want)
		}
	}
}

// TestScopeSubtreeEmptyDenies is the fail-closed half: an empty selector
// names zero conversations and must render an always-false condition, never
// "no filter" — the same rule the zero Scope obeys.
func TestScopeSubtreeEmptyDenies(t *testing.T) {
	s := ScopeSubtree(SubtreeSelector{})
	if s.valid() {
		t.Fatal("empty subtree scope .valid() = true, want false")
	}
	a := &argList{}
	if got := s.cond(a, "c.owner_user_id", "c.id", "c.external_ref"); got != "1=0" {
		t.Fatalf("empty subtree cond() = %q, want %q", got, "1=0")
	}
	if len(a.args) != 0 {
		t.Fatalf("empty subtree cond() appended %d args, want 0", len(a.args))
	}
}

// TestScopeSubtreeIsValid pins that a non-empty selector builds a valid
// scope.
func TestScopeSubtreeIsValid(t *testing.T) {
	if !ScopeSubtree(SubtreeSelector{ExternalRefs: []string{"c_01"}}).valid() {
		t.Fatal("ScopeSubtree(non-empty).valid() = false, want true")
	}
}

// TestScopeSubtreeQueriesAdmitOnlySelectorNames is the database proof of the
// subtree boundary across the three surfaces the MCP face's conversation
// tools serve: Search, Export and the catalogue Query. A selector naming one
// conversation by uuid, one by external_ref, and one by prefix admits exactly
// those three — an owner-scoped or all-owners caller's other rows never
// appear, and an empty selector denies everything.
func TestScopeSubtreeQueriesAdmitOnlySelectorNames(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	ins := New(pool)

	// seedConversation, not the bare insert: the models catalogue query
	// counts turn-level model rows, so the membership oracle needs turns.
	inSubtree := seedConversation(t, pool, "client", "alice")
	byRef := seedConversation(t, pool, "client", "alice")
	byPrefix := seedConversation(t, pool, "client", "alice")
	outside := seedConversation(t, pool, "client", "alice")
	for _, ref := range []struct{ id, ref string }{
		{byRef, "c_01M2branch"},
		{byPrefix, "c_01M2branch:some-turn-uuid"},
	} {
		if _, err := pool.Exec(ctx,
			`UPDATE conversations.conversation SET external_ref = $1 WHERE id = $2::uuid`,
			ref.ref, ref.id); err != nil {
			t.Fatalf("set external_ref: %v", err)
		}
	}

	sel := SubtreeSelector{
		ConversationIDs:     []string{inSubtree},
		ExternalRefs:        []string{"c_01M2branch"},
		ExternalRefPrefixes: []string{"c_01M2branch:"},
	}
	scope := ScopeSubtree(sel)

	rows, err := ins.Search(ctx, scope, SearchFilter{Limit: 100})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	for _, want := range []string{inSubtree, byRef, byPrefix} {
		if !got[want] {
			t.Errorf("search missing in-subtree conversation %s (got %v)", want, got)
		}
	}
	if got[outside] {
		t.Error("search returned a conversation outside the subtree")
	}

	for _, conv := range []struct {
		name string
		id   string
		want bool
	}{
		{"in-subtree uuid", inSubtree, true},
		{"in-subtree external_ref", byRef, true},
		{"in-subtree prefix", byPrefix, true},
		{"outside", outside, false},
	} {
		_, err := ins.Export(ctx, scope, conv.id)
		found := !errors.Is(err, ErrNotFound)
		if found != conv.want {
			info := "not found"
			if found {
				info = "found"
			}
			t.Errorf("export %s (%s) = %v (%s), want found=%v", conv.name, conv.id, err, info, conv.want)
		}
	}

	// The catalogue honours the same boundary. models groups by served model
	// with a distinct-conversation count, so the seeded rows (all one model,
	// two turns each) give an exact total: 3 in-subtree conversations, never
	// the fourth.
	res, err := ins.Query(ctx, scope, "models", StatsFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	convs := int64(0)
	for _, row := range res.Rows {
		if len(row) != 3 {
			t.Fatalf("models row %v, want [model, conversations, turns]", row)
		}
		if c, ok := row[1].(IntEntry); ok {
			convs += int64(c)
		}
	}
	if convs != 3 {
		t.Errorf("models conversations = %d, want 3 (the subtree, never the outside row)", convs)
	}

	// ConversationStats probes ONE conversation with the alias-less spelling
	// of the same boundary (the bare-table column names), so the subtree arms
	// must hold there too: in-subtree resolves, outside reads not-found.
	for _, conv := range []struct {
		name string
		id   string
		want bool
	}{
		{"in-subtree", inSubtree, true},
		{"outside", outside, false},
	} {
		_, err := ins.ConversationStats(ctx, scope, conv.id)
		found := !errors.Is(err, ErrNotFound)
		if found != conv.want {
			t.Errorf("stats %s = err %v, want found=%v", conv.name, err, conv.want)
		}
	}

	// An empty selector denies: zero rows, never the world.
	rows, err = ins.Search(ctx, ScopeSubtree(SubtreeSelector{}), SearchFilter{Limit: 100})
	if err != nil {
		t.Fatalf("empty-selector search: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty-selector search returned %d rows, want 0", len(rows))
	}
}
