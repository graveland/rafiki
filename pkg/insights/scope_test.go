// SPDX-License-Identifier: Apache-2.0

package insights

import "testing"

// TestScopeAllCond pins the all-owners scope: it renders a bare always-true
// condition and must append no parameter.
func TestScopeAllCond(t *testing.T) {
	a := &argList{}
	if got := ScopeAll().cond(a, "c.owner_user_id"); got != "1=1" {
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
	got := ScopeOwner("abc").cond(a, "c.owner_user_id")
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
	if got := (Scope{}).cond(a, "c.owner_user_id"); got != "1=0" {
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
	if got := s.cond(a, "c.owner_user_id"); got != "1=0" {
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
