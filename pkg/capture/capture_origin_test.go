// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestOriginCaptureNodesAreClaudeCode pins that every node a capture write
// records carries OriginClaudeCode. Capture's node writers are reached only
// from the Anthropic proxy face — DecomposeRequest's request chain and
// AppendResponseMessage's reply — so the origin is unconditional there, and a
// node left unclassified is a writer that forgot to declare itself.
func TestOriginCaptureNodesAreClaudeCode(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
	s := NewCaptureStore(pool)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req := []byte(`{"model":"claude","messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"text","text":"yo"}]}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: routing.PrefixHash(req), Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req), "")
	c.NoError(err, "DecomposeRequest")

	canonical := []byte(`{"id":"msg_origin","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, head, canonical, 10, 5, "end_turn"), "AppendResponseMessage")

	// Every node the conversation holds: the two request messages and the reply.
	sn, err := s.ConvStore().Open(convID, conv.AsOwner()).Snapshot(ctx)
	c.NoError(err, "read the tree")
	nodes := 0
	for n := range sn.History() {
		nodes++
		c.Eq(conv.OriginClaudeCode, n.Origin, "node %d (role %s) origin", n.Ordinal, n.Msg.Role)
	}
	c.Eq(3, nodes, "node count: two request messages plus the reply")

	// And the stored column, not just the decoded node.
	var unclassified int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message
		  WHERE conversation_id=$1::uuid AND origin IS DISTINCT FROM $2`,
		convID, string(conv.OriginClaudeCode)).Scan(&unclassified), "count unclassified rows")
	c.Eq(0, unclassified, "rows whose stored origin is not claude-code")
}
