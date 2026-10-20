// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/tasks"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// The wiring tests name one owner and two claude children of theirs. Claude is
// the kind whose ledger the child id keys: it has no engine conversation id of
// its own (see ensureLedgerConversation).
const (
	taskLedgerWiringUser = "u-alice"
	taskLedgerChildA     = "c_claude_a"
	taskLedgerChildB     = "c_claude_b"
)

// taskLedgerWiringFace builds the real MCP face over a running Controller with
// an in-memory task ledger, and registers two claude children of one owner.
// No process is started: the ledger resolves from the child rows alone.
func taskLedgerWiringFace(t *testing.T) (*mcpFace, *Controller) {
	t.Helper()
	ctrl := displayServiceController(t)
	ctrl.tasks = tasks.NewMemoryStore()
	for _, id := range []string{taskLedgerChildA, taskLedgerChildB} {
		ctrl.st.Insert(&childstore.Session{
			ChildID: id, Kind: protocol.KindClaude, Status: protocol.StatusIdle,
			StartedAt: time.Now(), OwnerUserID: taskLedgerWiringUser,
		})
	}
	face := newMCPFace(discardLogger(), nil, nil, "test")
	face.SetController(ctrl)
	return face, ctrl
}

// registerMCPChild inserts a claude child row for an identity the face is
// about to serve. getServer resolves a child caller's ledger conversation on
// EVERY tool call and refuses one whose child is not registered, so a fixture
// handing the face a child identity needs the row that identity stands for.
func registerMCPChild(t *testing.T, ctrl *Controller, ownerUserID, childID string) {
	t.Helper()
	ctrl.st.Insert(&childstore.Session{
		ChildID: childID, Kind: protocol.KindClaude, Status: protocol.StatusIdle,
		StartedAt: time.Now(), OwnerUserID: ownerUserID,
	})
}

// mcpChildRequestFor is the request a per-child credential leaves behind:
// UserTokenAuth has already authenticated it into the context, with the child's
// own id and the per-child provenance.
func mcpChildRequestFor(userID, childID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	return r.WithContext(server.WithIdentity(r.Context(), &server.Identity{
		UserID: userID, Username: userID, ChildID: childID, Via: server.ProvenanceChildToken,
	}))
}

// mcpToolText calls one tool over the connected session and returns its text,
// failing the test if the tool reported an error.
func mcpToolText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	ck := assert.NewAborting(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	ck.NoError(err, "CallTool %s", name)
	text := ""
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	ck.False(res.IsError, "%s failed: %s", name, text)
	return text
}

// taskAddContent is the one-call shape every test here uses.
func taskAddContent(content string) map[string]any {
	return map[string]any{
		"items": []map[string]any{{"content": content, "active_form": "doing " + content}},
	}
}

// ledgerRows reads one ledger key out of the controller's store. The key itself
// is checked by the caller, so a test that read the wrong ledger fails loudly
// rather than passing on an empty list.
func ledgerRows(t *testing.T, store tasks.Store, convID string) []tasks.Task {
	t.Helper()
	rows, err := store.List(context.Background(), tasks.ListFilter{ConversationID: convID})
	assert.NewAborting(t).NoError(err, "List %s", convID)
	return rows
}

// TestTaskLedgerWiringChildCallersGetSeparateLedgers pins the MCP face's child
// branch: two children of one owner write to their own ledgers, and each
// mutation is announced on the writing child's bus only.
func TestTaskLedgerWiringChildCallersGetSeparateLedgers(t *testing.T) {
	ck := assert.NewAborting(t)
	face, ctrl := taskLedgerWiringFace(t)

	busA, cancelA := ctrl.native.Subscribe(taskLedgerChildA)
	defer cancelA()
	busB, cancelB := ctrl.native.Subscribe(taskLedgerChildB)
	defer cancelB()

	a := mcpConnect(t, face.getServer(mcpChildRequestFor(taskLedgerWiringUser, taskLedgerChildA)))
	mcpToolText(t, a, "task_add", taskAddContent("child A work"))

	b := mcpConnect(t, face.getServer(mcpChildRequestFor(taskLedgerWiringUser, taskLedgerChildB)))
	listing := mcpToolText(t, b, "task_list", nil)
	ck.NotStrContains(listing, "child A work", "child B's task_list shows its sibling's task")

	ck.StrContains(mcpToolText(t, a, "task_list", nil), "child A work", "child A's own task_list")

	ck.Len(ledgerRows(t, ctrl.tasks, "child:"+taskLedgerChildA), 1, "rows in child A's ledger")
	ck.Len(ledgerRows(t, ctrl.tasks, "child:"+taskLedgerChildB), 0, "rows in child B's ledger")

	ck.Len(tasksChangedOn(busA), 1, "TasksChanged events on child A's bus")
	ck.Len(tasksChangedOn(busB), 0, "TasksChanged events on child B's bus")
}

// TestTaskLedgerWiringUserCredentialKeepsThePerUserLedger pins the other side
// of the switch: a user credential, which is not a child and holds no position
// in the tree, still reads and writes the mcpLedger's per-user ledger — and two
// sessions of that user share it.
func TestTaskLedgerWiringUserCredentialKeepsThePerUserLedger(t *testing.T) {
	ck := assert.NewAborting(t)
	face, ctrl := taskLedgerWiringFace(t)
	userLedger := "user:" + taskLedgerWiringUser

	first := mcpConnect(t, face.getServer(mcpRequestFor(taskLedgerWiringUser)))
	mcpToolText(t, first, "task_add", taskAddContent("coordinate the children"))

	ck.Len(ledgerRows(t, ctrl.tasks, userLedger), 1, "rows in the per-user ledger")
	ck.Len(ledgerRows(t, ctrl.tasks, "child:"+taskLedgerChildA), 0, "a user credential wrote into a child's ledger")

	second := mcpConnect(t, face.getServer(mcpRequestFor(taskLedgerWiringUser)))
	mcpToolText(t, second, "task_add", taskAddContent("review the result"))

	rows := ledgerRows(t, ctrl.tasks, userLedger)
	ck.Require().Len(rows, 2, "a second session of the same user did not append to the per-user ledger")
	ck.StrContains(mcpToolText(t, second, "task_list", nil), "coordinate the children", "the second session's task_list is missing the first session's task")
}

// TestTaskLedgerWiringSpawnTaskHandleResolvesForAClaudeSpawner pins the spawn
// path an agent takes: a claude spawner adds a task through its own MCP ledger
// and spawns with that handle, so Controller.Spawn finds it in the spawner's
// ledger and assigns the new child. A claude child has no conversation id of
// its own, so this only resolves when the spawner hands over the ledger
// conversation ensureLedgerConversation derives from its child id.
//
// Not parallel: it sets CLAUDE_BINARY, and t.Setenv refuses a parallel test.
// The spawner carries no PiBinary (the wire's agent_spawn schema has none), so
// the daemon resolves the binary from the env exactly as production does.
func TestTaskLedgerWiringSpawnTaskHandleResolvesForAClaudeSpawner(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	ctrl.tasks = tasks.NewMemoryStore()
	// A claude child is routed only through the proxy face, which is also the
	// client the spawner's own MCP tools would reach.
	ctrl.proxyURL, ctrl.proxyToken = "http://127.0.0.1:1/", "boot-secret"
	t.Setenv("CLAUDE_BINARY", fakePiBin(t))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	top, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind: protocol.KindClaude, Cwd: t.TempDir(), PiBinary: fakePiBin(t), NoSession: true,
	}, users.Identity{UserID: taskLedgerWiringUser, Username: "alice"})
	ck.NoError(err, "spawn the claude spawner")

	spawnerSnap, ok := ctrl.st.Get(top.ChildID)
	ck.Require().True(ok, "the spawner is registered")
	ck.Eq("", spawnerSnap.SessionID, "a claude child's row carries a conversation id; the fixture no longer exercises the child-id-keyed ledger")

	face := newMCPFace(discardLogger(), nil, nil, "test")
	face.SetController(ctrl)
	sess := mcpConnect(t, face.getServer(mcpChildRequestFor(taskLedgerWiringUser, top.ChildID)))
	mcpToolText(t, sess, "task_add", taskAddContent("delegate the review"))

	spawner := newControllerSpawner(ctrl, top.ChildID)
	worker, err := spawner.Spawn(ctx, tools.SpawnSpec{
		Kind: protocol.KindClaude, Name: "worker", Cwd: t.TempDir(), Task: "1",
	})

	ck.NoError(err, "a spawn naming the handle task_add returned")

	ledgerKey, err := ctrl.ensureLedgerConversation(ctx, spawnerSnap)
	ck.Require().NoError(err, "resolve the spawner's ledger")
	rows := ledgerRows(t, ctrl.tasks, ledgerKey)
	ck.Require().Len(rows, 1, "rows in the spawner's ledger")
	ck.Eq("1", rows[0].Handle, "handle")
	ck.Eq(worker.ChildID, rows[0].Assignee, "the spawned child is the row's assignee")
}
