// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// A spawn whose INITIAL child row cannot be persisted must be REFUSED and fully
// unwound: the lineage walk cannot cross a missing row, so a child that ran
// without one would hide its whole sub-subtree from its ancestor's spend. The
// error is returned, the child is in neither the store nor the process map, and
// no process is left launched.
//
// Fails against the pre-change spawn, which logged the failed insert and
// carried on.
func TestSpawnRefusedWhenInitialRowCannotBePersisted(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	fs := &failingChildStore{}
	ctrl.children = fs

	_, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       os.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().Error(err, "a spawn whose initial row cannot be written must be refused")

	ck.Require().Eq(1, len(fs.failed), "exactly the initial insert is attempted; got %v", fs.failed)
	childID := fs.failed[0]
	ck.StrContains(err.Error(), childID, "the refusal must name the child id: %v", err)

	if _, ok := ctrl.st.Get(childID); ok {
		t.Fatalf("the refused spawn left %s in the in-memory store", childID)
	}
	if _, ok := ctrl.cm.Get(childID); ok {
		t.Fatalf("the refused spawn left %s in the process map, so a process was left launched", childID)
	}
}

// A refused spawn must also release the per-child MCP credential minted before
// the initial insert: buildEnv mints it, then writeRecord fails. forgetMCPToken
// otherwise runs only from handleChildExit — which never runs for a refused
// spawn — so the credential would stay live until a later mint swept it. Fails
// against the pre-change abortSpawn, which left the token registered.
func TestRefusedSpawnDropsTheMintedMCPToken(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	// The proxy face must be wired for a claude child to mint a token at all
	// (buildEnv → proxyChildEnv is the local path's only mint site).
	ctrl.proxyURL, ctrl.proxyToken = "http://127.0.0.1:1/", "boot-secret"
	fs := &failingChildStore{}
	ctrl.children = fs

	_, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       os.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().Error(err, "the spawn must be refused")
	ck.Require().Eq(1, len(fs.failed), "exactly the initial insert is attempted")
	childID := fs.failed[0]

	if tok, ok := ctrl.mcpTokensByChild[childID]; ok {
		t.Fatalf("the refused spawn left MCP token %q registered for %s", tok, childID)
	}
	for tok, id := range ctrl.mcpTokens {
		if id == childID {
			t.Fatalf("the refused spawn left secret %q mapped to %s in mcpTokens", tok, childID)
		}
	}
}

// The script-output hook registers per-spawn state at spec-build time, BEFORE
// the initial insert, and creates a coalescer on the child's first line; both
// live until handleChildExit takes them. A refused spawn must release them, or
// the state — and a parked flush goroutine — would leak. Fails against the
// pre-change abortSpawn, which touched neither.
func TestAbortSpawnReleasesScriptOutputState(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	// Register per-spawn state and a coalescer exactly as the hook does once the
	// child's first line arrives.
	ctrl.scriptOutputHook("c_abort")
	state := ctrl.scriptOutputState["c_abort"]
	ck.Require().NotNil(state, "the hook must have registered state")
	ck.Require().NotNil(ctrl.registerScriptOutputCoalescer("c_abort", state), "the coalescer must register")

	ctrl.abortSpawn("c_abort", nil)

	ctrl.scriptOutputsMu.Lock()
	_, stillState := ctrl.scriptOutputState["c_abort"]
	_, stillCo := ctrl.scriptOutputs["c_abort"]
	ctrl.scriptOutputsMu.Unlock()
	ck.False(stillState, "abortSpawn left the script-output state registered")
	ck.False(stillCo, "abortSpawn left the coalescer registered")
}

// The other half of the guard: a SUCCESSFUL persist must spawn exactly as
// before, so the refusal above is not over-broad.
func TestSpawnProceedsWhenInitialRowPersists(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.children = stubChildStore{}

	res, err := ctrl.Spawn(t.Context(), protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       os.TempDir(),
		PiBinary:  fakePiBin(t),
		NoSession: true,
	}, users.Identity{})
	ck.Require().NoError(err, "a spawn whose initial row persists must succeed")

	if _, ok := ctrl.st.Get(res.ChildID); !ok {
		t.Fatalf("the spawned child %s is absent from the store", res.ChildID)
	}
	if _, ok := ctrl.cm.Get(res.ChildID); !ok {
		t.Fatalf("the spawned child %s is absent from the process map", res.ChildID)
	}
}
