// SPDX-License-Identifier: Apache-2.0

package control

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/users"
)

// scopeConn is a minimal Connection stub for the scopeForConnection tests;
// identityConn lives in dispatch_test.go, a different package.
type scopeConn struct{ id users.Identity }

func (scopeConn) Deliver(_ []byte)           {}
func (c scopeConn) Identity() users.Identity { return c.id }
func (scopeConn) Restricted() bool           { return false }

// insights.Scope's fields are unexported, but cross-package struct equality is
// still legal — == compares field-by-field regardless of exportedness — and it
// is meaningful here because every operand comes from the package's own
// constructors (ScopeAll/ScopeOwner). Two Scope values compare equal only when
// they were constructed the same way, which is exactly the assertion these
// tests need: the derived scope IS the constructor output named in the test.
//
// These tests pin the framed protocol's scope inference, which the design
// calls "load-bearing and invisible": empty identity → ScopeAll (the
// UDS/local-trust path), admin → ScopeAll, user → ScopeOwner.

func TestScopeForConnectionEmptyIdentityIsScopeAll(t *testing.T) {
	if got := scopeForConnection(nil); got != insights.ScopeAll() {
		t.Errorf("nil connection: got %+v, want ScopeAll", got)
	}
	if got := scopeForConnection(scopeConn{id: users.Identity{}}); got != insights.ScopeAll() {
		t.Errorf("empty identity: got %+v, want ScopeAll", got)
	}
}

func TestScopeForConnectionOwnerIdentityIsScopeOwner(t *testing.T) {
	got := scopeForConnection(scopeConn{id: users.Identity{UserID: "u1"}})
	if got != insights.ScopeOwner("u1") {
		t.Errorf("owner identity: got %+v, want ScopeOwner(u1)", got)
	}
	if got == insights.ScopeAll() {
		t.Error("a user identity's scope must not be ScopeAll")
	}
}

func TestScopeForConnectionAdminIdentityIsScopeAll(t *testing.T) {
	got := scopeForConnection(scopeConn{id: users.Identity{UserID: "u1", IsAdmin: true}})
	if got != insights.ScopeAll() {
		t.Errorf("admin identity: got %+v, want ScopeAll", got)
	}
}
