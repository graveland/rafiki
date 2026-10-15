// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// A claude child's conversation is captured by the proxy under
// external_ref = child id (X-Rafiki-Session); its session id is claude's own
// and names no conversation row. ConversationExport must therefore accept the
// child id. The proxy's upstream is fixed at api.anthropic.com, so the test
// seeds the rows the capture path writes rather than driving a model turn;
// the spawn, the daemon and the Connect export are real.
func TestConversationExportResolvesAClaudeChildID(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	c := assert.NewCollecting(t)
	ck := assert.NewAborting(t)

	d, dumps := bootMCPChildDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := d.control(t).Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  protocol.KindClaude,
		Model: "anthropic/sonnet-latest",
	}))
	ck.NoError(err, "spawn claude child")
	childID := resp.Msg.GetChildId()
	waitClaudeDump(t, d, dumps, childID)

	export := func() (*rafikiv1.ConversationExportResponse, error) {
		r, err := d.control(t).ConversationExport(ctx, connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: childID}))
		if err != nil {
			return nil, err
		}
		return r.Msg, nil
	}

	// Before the first captured request there is nothing to export, and the
	// error says to retry instead of claiming claude children have none.
	_, err = export()
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "export before first request")
	c.True(err != nil && strings.Contains(err.Error(), "retry"), "error should say to retry: %v", err)

	convID := seedCapturedClaudeTurns(t, dsn, childID, "forty-two", "")

	tr, err := export()
	ck.NoError(err, "export by child id")
	c.Eq(convID, tr.GetConversationId(), "resolved conversation")
	ck.Eq(2, len(tr.GetTurns()), "turns")
	last := tr.GetTurns()[1]
	c.Eq("assistant", last.GetRole(), "last turn role")
	c.True(strings.Contains(string(last.GetContent()), "forty-two"), "last turn = %s", last.GetContent())
}

// seedCapturedClaudeTurns writes what the capture path writes for a claude
// child's one turn: a client-driven conversation keyed by external_ref = child
// id (owned by the token's user, or unowned when empty), a user message and an assistant answer. Returns the conversation id.
func seedCapturedClaudeTurns(t *testing.T, dsn, childID, answer, ownerToken string) string {
	t.Helper()
	ck := assert.NewAborting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	ck.NoError(err, "open pool")
	defer pool.Close()
	var ownerID any // NULL = the unattributed (local-trust) bucket
	if ownerToken != "" {
		var id string
		ck.NoError(pool.QueryRow(ctx,
			`SELECT user_id::text FROM conversations.user_token WHERE token_sha256 = $1`, users.HashToken(ownerToken)).Scan(&id), "look up the token's user")
		ownerID = id
	}
	var convID string
	ck.NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref, owner_user_id)
		 VALUES ('claude','client',$1,$2::uuid) RETURNING id::text`, childID, ownerID).Scan(&convID), "seed conversation")
	for i, m := range []struct{ role, content string }{
		{"user", `[{"type":"text","text":"what is the answer"}]`},
		{"assistant", `[{"type":"text","text":"` + answer + `"}]`},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content) VALUES ($1::uuid,$2,$3,$4)`,
			convID, i, m.role, []byte(m.content))
		ck.NoError(err, "seed message")
	}
	return convID
}
