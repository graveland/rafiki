// SPDX-License-Identifier: Apache-2.0

package integration_test

// The Connect plane's spawn, end to end on the real daemon: what an empty
// kind must resolve to, and who owns what it spawns.
//
// An empty SpawnRequest.kind is the fundi default, and Controller.Spawn
// resolves it once (cmd/rafikid/controller.go) so every downstream consumer
// sees the same kind. Before that resolution landed, a raw empty kind fell
// through agentRunner's switch (no case matches "") to the nil-Runner
// subprocess path: the fundi engine ran as a `rafikid fundi` subprocess that
// cannot know its owner, so the child's conversation row landed unattributed
// (owner_user_id NULL) while the childstore row carried the caller — and
// every owner-scoped read over that conversation, ConversationExport first
// among them, answered not-found. The Python SDK's spawn is the Connect
// caller that exposed it, but any Connect client hits the same trap, so
// this pin is Go-only and needs no python3.

import (
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestConnectSpawnDefaultsEmptyKindToFundi(t *testing.T) {
	t.Parallel()
	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	t.Cleanup(func() { os.RemoveAll(d.homeDir) })

	token, configDir := scriptUser(t, d)
	client := faceClient(t, d, token)

	// Spawn with NO kind: the default-fundi shape any Connect client sends.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := emptyKindSpawn(configDir)
	resp, err := client.Spawn(ctx, connect.NewRequest(&req))
	if err != nil {
		t.Fatalf("spawn with an empty kind: %v", err)
	}
	childID := resp.Msg.GetChildId()

	pool := openPool(t, os.Getenv("RAFIKI_TEST_DSN"))
	defer pool.Close()

	// The childstore row carries the RESOLVED kind, not the raw "".
	var kind string
	if err := pool.QueryRow(context.Background(),
		`SELECT kind FROM conversations.child WHERE child_id = $1`, childID).Scan(&kind); err != nil {
		t.Fatalf("read child row: %v", err)
	}
	if kind != "fundi" {
		t.Fatalf("conversations.child.kind = %q, want fundi: an empty Connect kind must resolve, not fall through to the subprocess path", kind)
	}

	// The conversation the engine created is attributed to the SAME owner
	// the childstore row carries — the two writers must agree, and the
	// unattributed conversation is exactly what the subprocess path produced.
	var sameOwner bool
	if err := pool.QueryRow(context.Background(),
		`SELECT cc.owner_user_id IS NOT NULL AND cc.owner_user_id = c2.owner_user_id
		 FROM conversations.conversation cc
		 JOIN conversations.child c2 ON c2.child_id = cc.external_ref
		 WHERE cc.external_ref = $1`, childID).Scan(&sameOwner); err != nil {
		t.Fatalf("read conversation owner: %v", err)
	}
	if !sameOwner {
		t.Fatal("the spawned child's conversation is unattributed (owner_user_id NULL or a different owner): the engine ran on the subprocess path")
	}
}

// emptyKindSpawn is a SpawnRequest with every kind-dependent field unset:
// cwd (required by the plane), a name, and a model — no kind, so the
// daemon's fundi default must resolve it.
func emptyKindSpawn(cwd string) rafikiv1.SpawnRequest {
	return rafikiv1.SpawnRequest{
		Cwd:  cwd,
		Name: "empty-kind-probe",
		// A model the shipped registry resolves; no prompt is ever sent, so
		// no LLM call happens and the blanked keys in noRealProviderEnv
		// never matter.
		Model: "anthropic/claude-sonnet-4-5",
	}
}
