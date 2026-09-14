// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/users"
)

// The two conversationReader constructors are the binding story for the
// conversation_search/export tools: the scope they bake in is what keeps a
// caller inside its own corpus. Scope is unexported-fielded (a struct literal
// cannot construct a valid one outside pkg/insights), so equality against the
// constructors is the observable -- zero value denies, ScopeAll admits all.
func TestNewControllerConversationReaderScopesAnEmptyOwnerToDeny(t *testing.T) {
	if r := newControllerConversationReader(nil, ""); r.scope != (insights.Scope{}) {
		t.Errorf("anonymous spawn scope = %+v, want the zero-value deny-all Scope", r.scope)
	}
	if r := newControllerConversationReader(nil, "u-owner"); r.scope != insights.ScopeOwner("u-owner") {
		t.Errorf("owned spawn scope = %+v, want ScopeOwner", r.scope)
	}
}

func TestNewMCPConversationReaderScopePerIdentity(t *testing.T) {
	if r := newMCPConversationReader(nil, users.Identity{IsAdmin: true}); r.scope != insights.ScopeAll() {
		t.Errorf("admin caller scope = %+v, want ScopeAll", r.scope)
	}
	if r := newMCPConversationReader(nil, users.Identity{UserID: "u-alice"}); r.scope != insights.ScopeOwner("u-alice") {
		t.Errorf("user caller scope = %+v, want ScopeOwner", r.scope)
	}
	if r := newMCPConversationReader(nil, users.Identity{}); r.scope != (insights.Scope{}) {
		t.Errorf("anonymous caller scope = %+v, want the zero-value deny-all Scope", r.scope)
	}
}
