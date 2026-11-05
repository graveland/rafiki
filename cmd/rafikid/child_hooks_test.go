package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

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

// installChildHooks is the ONE place a SpawnSpec's callbacks are installed, and
// every claude LAUNCH path — spawn, resume, respawn, a daraja-backed run — goes
// through it. A claude child must always carry the acceptance-echo callback
// (without it nothing ever publishes a delivery receipt), no other kind may
// (nothing else echoes a message back), and a script child is the only one with
// the raw-output hook.
func TestChildHooksInstallReplayOnlyForClaude(t *testing.T) {
	ck := assert.NewAborting(t)
	c := &Controller{st: childstore.New(), cm: newChildManager()}

	for _, tc := range []struct {
		kind string
	}{
		{protocol.KindClaude},
		{protocol.KindFundi},
		{protocol.KindScript},
		{""},
	} {
		spec := child.SpawnSpec{ChildID: "c_" + tc.kind}
		gen := c.installChildHooks(&spec, tc.kind)

		ck.NotNil(spec.NativeSink, "%q: NativeSink hook is nil", tc.kind)
		ck.NotNil(spec.OnMeta, "%q: OnMeta hook is nil", tc.kind)
		ck.NotNil(spec.OnSubagent, "%q: OnSubagent hook is nil", tc.kind)
		ck.Eq(tc.kind == protocol.KindScript, spec.OnScriptOutput != nil, "%q: OnScriptOutput", tc.kind)
		if tc.kind == protocol.KindClaude {
			ck.NotNil(spec.OnUserReplay,
				"%q: a claude launch with no echo hook never publishes a delivery receipt", tc.kind)
			// The generation is what the exit path retires, so a launch that
			// mints none could never be torn down.
			ck.True(gen != 0, "%q: installChildHooks minted no launch generation", tc.kind)
			continue
		}
		ck.True(spec.OnUserReplay == nil, "%q: OnUserReplay must be nil for a kind that never echoes", tc.kind)
		ck.Eq(uint64(0), gen, "%q: a non-claude launch minted a generation", tc.kind)
	}
}
