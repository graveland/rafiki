// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"testing"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// seedRowsWithOrigin appends fixture rows as the conversation's initial history
// under one writer, the shape a compaction test needs to seed a history another
// harness wrote.
func seedRowsWithOrigin(t *testing.T, conv *Conversation, origin convpkg.Origin, rows ...*convpkg.Node) {
	t.Helper()
	nodes := make([]convpkg.NewNode, len(rows))
	for i, r := range rows {
		nodes[i] = convpkg.NewNode{
			Msg: r.Msg, Content: r.Content, Kind: r.Kind, StopReason: r.StopReason,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, Origin: origin,
		}
	}
	_, err := conv.handle().Append(context.Background(), convpkg.Append{Root: convtree.RootKindRoot, Nodes: nodes})
	assert.NewAborting(t).NoError(err, "seed fixture rows")
}

// TestOriginLLMAppendsAreFundi pins that both of this package's node writers —
// SeedHistory and appendMessage — record OriginFundi. The stored column is
// checked, not only the decoded node: a writer that set the field but lost it
// on the way to the row would leave the record unclassified all the same.
func TestOriginLLMAppendsAreFundi(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondText("a reply"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("anthropic/sonnet-latest"))
	ck.Require().NoError(err, "Conversation")

	ck.Require().NoError(conv.SeedHistory(ctx, []msg.Message{
		{Role: msg.User, Content: []msg.Block{msg.Text{Text: "seeded prompt"}}},
		{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "seeded reply"}}},
	}), "SeedHistory")
	_, err = conv.Send(ctx, UserText("hello"))
	ck.Require().NoError(err, "Send")

	var origins []string
	rows, err := pool.Query(ctx, `SELECT coalesce(origin, '') FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid ORDER BY ordinal`, conv.ID)
	ck.Require().NoError(err, "read rows")
	for rows.Next() {
		var o string
		ck.Require().NoError(rows.Scan(&o), "scan origin")
		origins = append(origins, o)
	}
	rows.Close()
	ck.Require().NoError(rows.Err(), "read rows")
	ck.Require().Len(origins, 4, "the seeded pair plus the turn's user and assistant rows")
	for i, o := range origins {
		ck.Eq(string(convpkg.OriginFundi), o, "row %d stored origin", i)
	}

	hist, err := conv.History(ctx)
	ck.Require().NoError(err, "read the working set")
	ck.Require().Len(hist, 4, "working set")
	for _, n := range hist {
		ck.Eq(convpkg.OriginFundi, n.Origin, "node %d origin", n.Ordinal)
	}
}

// TestOriginCompactionTailKeepsSourceOrigin pins the one asymmetric writer:
// compaction re-hangs a tail row under its summary, and that copy is the same
// row rather than new content, so it keeps the writer that first recorded it.
// The summary beside it is this package's own content and is OriginFundi.
func TestOriginCompactionTailKeepsSourceOrigin(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(*msg.Request) (*msg.Response, error){
		respondUsage(10, 5, 0, 0, "<summary>handover</summary>"),
	}}
	cl := memClient(t, sender)
	conv, err := cl.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	c.Require().NoError(err)
	seedRowsWithOrigin(t, conv, convpkg.OriginClaudeCode,
		textRow(0, msg.User, "a"),
		textRow(1, msg.Assistant, "b"),
		textRow(2, msg.User, "c"),
		textRow(3, msg.Assistant, "d"))

	ok, err := conv.Compact(ctx, "")
	c.Require().NoError(err)
	c.True(ok, "Compact reported nothing written")

	tails, summaries := 0, 0
	for _, n := range allRows(t, conv) {
		switch n.Kind {
		case convpkg.KindCompactionTail:
			tails++
			c.Eq(convpkg.OriginClaudeCode, n.Origin, "a tail copy must keep its source node's writer")
		case convpkg.KindCompactionSummary:
			summaries++
			c.Eq(convpkg.OriginFundi, n.Origin, "the summary is content this package wrote")
		}
	}
	c.Eq(1, summaries, "summary rows")
	c.Greater(0, tails, "no tail copies were written")
}
