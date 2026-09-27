package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"

	"github.com/multigres/testkit/assert"
)

// Both hooks must be installed for EVERY kind. NativeSink used to be set only
// inside `if runner != nil`, which is fundi-only, so the claude translator was
// unreachable: attaching to a claude child showed an empty pane.
func TestChildHooksAreInstalledForEveryKind(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager()}

	sink, onMeta, onSubagent := c.childHooks("c_test")

	ck.NotNil(sink, "NativeSink hook is nil; the claude translator would never run")
	ck.NotNil(onMeta, "OnMeta hook is nil; a claude session id would never reach the store")
	ck.NotNil(onSubagent, "OnSubagent hook is nil; a native subagent would keep its opaque thread-uuid name")
}

// The meta hook must ignore a metadata frame that carries no session id rather
// than writing an empty string over a good one.
func TestChildHooksMetaIgnoresEmptySessionID(t *testing.T) {
	ck := assert.NewAborting(t)
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_test", SessionID: "sess-good"})
	c := &Controller{st: st, cm: newChildManager()}

	_, onMeta, _ := c.childHooks("c_test")
	onMeta(child.SnifferMetadata{Model: "claude-opus-5"})

	snap, ok := c.st.Get("c_test")
	ck.True(ok, "session missing from store")
	ck.Eq("sess-good", snap.SessionID, "SessionID")
}
