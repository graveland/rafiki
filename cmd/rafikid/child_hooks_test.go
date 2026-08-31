package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
)

// Both hooks must be installed for EVERY kind. NativeSink used to be set only
// inside `if runner != nil`, which is fundi-only, so the claude translator was
// unreachable: attaching to a claude child showed an empty pane.
func TestChildHooksAreInstalledForEveryKind(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager()}

	sink, onMeta := c.childHooks("c_test")

	if sink == nil {
		t.Error("NativeSink hook is nil; the claude translator would never run")
	}
	if onMeta == nil {
		t.Error("OnMeta hook is nil; a claude session id would never reach the store")
	}
}

// The meta hook must ignore a metadata frame that carries no session id rather
// than writing an empty string over a good one.
func TestChildHooksMetaIgnoresEmptySessionID(t *testing.T) {
	st := childstore.New()
	st.Insert(&childstore.Session{ChildID: "c_test", SessionID: "sess-good"})
	c := &Controller{st: st, cm: newChildManager()}

	_, onMeta := c.childHooks("c_test")
	onMeta(child.SnifferMetadata{Model: "claude-opus-5"})

	snap, ok := c.st.Get("c_test")
	if !ok {
		t.Fatal("session missing from store")
	}
	if snap.SessionID != "sess-good" {
		t.Fatalf("SessionID = %q, want %q — an empty sniff must not clobber it", snap.SessionID, "sess-good")
	}
}
