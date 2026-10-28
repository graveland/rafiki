// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// mustHead reads the conversation's working head.
func mustHead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) string {
	t.Helper()
	var head string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT coalesce(head_id::text, '') FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&head), "read head_id")
	return head
}

// mustNodeCount counts the conversation's message rows.
func mustNodeCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) int {
	t.Helper()
	var n int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid`,
		convID).Scan(&n), "count messages")
	return n
}

// mustStats reads the conversation's tree counters.
func mustStats(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) (nodes, forks, fallbacks int) {
	t.Helper()
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT node_count, fork_count, fallback_count FROM conversations.conversation WHERE id=$1::uuid`,
		convID).Scan(&nodes, &forks, &fallbacks), "read conversation counters")
	return nodes, forks, fallbacks
}

// mustProviderNode finds the node a provider message id names.
func mustProviderNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID, providerMessageID string) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT id::text FROM conversations.conversation_message
		  WHERE conversation_id=$1::uuid AND provider_message_id=$2`,
		convID, providerMessageID).Scan(&id), "resolve provider message id %s", providerMessageID)
	return id
}

// mustNodeByContent finds the single node of the conversation holding content.
func mustNodeByContent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID, content string) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT id::text FROM conversations.conversation_message
		  WHERE conversation_id=$1::uuid AND content=$2::jsonb`,
		convID, content).Scan(&id), "find the node holding %s", content)
	return id
}

// mustNodeLike finds the single node of the conversation whose content text
// contains fragment.
func mustNodeLike(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID, fragment string) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT id::text FROM conversations.conversation_message
		  WHERE conversation_id=$1::uuid AND content::text LIKE '%' || $2 || '%'`,
		convID, fragment).Scan(&id), "find the node whose content contains %q", fragment)
	return id
}

// mustParent reads a node's parent id ("" for a root).
func mustParent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string) string {
	t.Helper()
	var parent string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT coalesce(parent_id::text, '') FROM conversations.conversation_message WHERE id=$1::uuid`,
		nodeID).Scan(&parent), "read parent of %s", nodeID)
	return parent
}

// mustField reads one node's kind, derived-from and depth.
func mustNodeKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string) (kind, derivedFrom string, depth int) {
	t.Helper()
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT coalesce(kind, ''), coalesce(derived_from_id::text, ''), coalesce(depth, -1)
		   FROM conversations.conversation_message WHERE id=$1::uuid`,
		nodeID).Scan(&kind, &derivedFrom, &depth), "read node %s", nodeID)
	return kind, derivedFrom, depth
}

// newConv creates a conversation of its own for a subtest: the test database is
// shared and never reset, so nothing may be asserted about rows it did not
// create.
func newConv(t *testing.T, ctx context.Context, s *CaptureStore) string {
	t.Helper()
	id, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	assert.NewAborting(t).NoError(err, "EnsureConversation")
	return id
}

// TestAttachRules pins the attach decision, one clause per subtest. Every case
// starts from a conversation it created and asserts both the shape it produced
// and the counter that distinguishes a decided attach from a fallback.
func TestAttachRules(t *testing.T) {
	reply := func(id, text string) []byte {
		return []byte(`{"id":"` + id + `","content":[{"type":"text","text":"` + text + `"}]}`)
	}
	cases := []struct {
		name string
		run  func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore)
	}{
		{
			// (a) An id nothing resolves: the history itself places the request.
			name: "unknown previous_message_id falls back to the hash chain",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID := newConv(t, ctx, s)
				runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`), 1, 1)

				head := runTurnFrom(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`),
					"msg_never_stored", 1, 1)

				b := mustNodeByContent(t, ctx, pool, convID, `"b"`)
				c.Eq(b, mustParent(t, ctx, pool, head), "the new message hangs off the deepest hash match")
				_, _, fallbacks := mustStats(t, ctx, pool, convID)
				c.Eq(1, fallbacks, "fallback_count")
				c.Eq(3, mustNodeCount(t, ctx, pool, convID), "node count")
			},
		},
		{
			// (b) The id resolves but the message it should be replaying is not
			// what is stored there, and message 0 is no summary.
			name: "a rewritten attach message falls back to the hash chain",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID := newConv(t, ctx, s)
				runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"a"}]}`), 1, 1)
				mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID), reply("msg_r", "b"), "end_turn")

				head := runTurnFrom(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"rewritten"},{"role":"user","content":"c"}]}`),
					"msg_r", 1, 1)

				a := mustNodeByContent(t, ctx, pool, convID, `"a"`)
				rewritten := mustNodeByContent(t, ctx, pool, convID, `"rewritten"`)
				c.Eq(a, mustParent(t, ctx, pool, rewritten), "the rewritten message is a sibling of the stored reply")
				c.Eq(rewritten, mustParent(t, ctx, pool, head), "the rest of the request hangs off the rewritten message")
				_, _, fallbacks := mustStats(t, ctx, pool, convID)
				c.Eq(1, fallbacks, "fallback_count")
			},
		},
		{
			// (c) The summary replaced the whole history, so the named reply is
			// not at the index it would occupy: the compaction root is the
			// answer, and it is not a fallback.
			name: "a summary whose attach point is out of range is a compaction root",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID := newConv(t, ctx, s)
				runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"a"}]}`), 1, 1)
				mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID), reply("msg_sum", "b"), "end_turn")

				head := runTurnFrom(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":`+summaryContent("out of range")+`}]}`),
					"msg_sum", 1, 1)

				kind, derived, depth := mustNodeKind(t, ctx, pool, head)
				c.Eq("compaction_summary", kind, "kind")
				c.Eq(mustProviderNode(t, ctx, pool, convID, "msg_sum"), derived, "derived_from_id")
				c.Eq(0, depth, "root depth")
				_, _, fallbacks := mustStats(t, ctx, pool, convID)
				c.Eq(0, fallbacks, "a compaction root is a decided attach, not a fallback")
			},
		},
		{
			// (d) Nothing named, nothing stored: the first request of a history.
			name: "a request naming nothing in an empty conversation is a plain root",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID := newConv(t, ctx, s)

				head := runTurn(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`), 1, 1)

				kind, derived, depth := mustNodeKind(t, ctx, pool, head)
				c.Eq("", kind, "kind")
				c.Eq("", derived, "derived_from_id")
				c.Eq(1, depth, "the head is the request's second message")
				c.Eq(mustNodeByContent(t, ctx, pool, convID, `"a"`), mustParent(t, ctx, pool, head), "parent")
				_, _, fallbacks := mustStats(t, ctx, pool, convID)
				c.Eq(0, fallbacks, "fallback_count")
			},
		},
		{
			// (e) An id that resolves, a message that verifies nothing, and a
			// history that matches nothing: the request is a new root, and the
			// node it named is kept as its lineage.
			name: "an unmatched history with a known id is a new root derived from it",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID := newConv(t, ctx, s)
				runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"a"}]}`), 1, 1)
				mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID), reply("msg_lineage", "b"), "end_turn")

				head := runTurnFrom(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":"elsewhere"},{"role":"assistant","content":"x"}]}`),
					"msg_lineage", 1, 1)

				root := mustNodeByContent(t, ctx, pool, convID, `"elsewhere"`)
				c.Eq("", mustParent(t, ctx, pool, root), "the request's first message opened a root of its own")
				kind, derived, depth := mustNodeKind(t, ctx, pool, root)
				c.Eq("", kind, "kind")
				c.Eq(mustProviderNode(t, ctx, pool, convID, "msg_lineage"), derived, "derived_from_id")
				c.Eq(0, depth, "root depth")
				c.Eq(root, mustParent(t, ctx, pool, head), "the rest of the request hangs off its root")
				_, _, fallbacks := mustStats(t, ctx, pool, convID)
				c.Eq(1, fallbacks, "fallback_count")
			},
		},
		{
			// (f) A parent is always in the same conversation, so an id that
			// lives in a sibling conversation is unknown here.
			name: "an id from another conversation is unknown",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				other := newConv(t, ctx, s)
				runTurn(t, ctx, s, other, []byte(`{"messages":[{"role":"user","content":"a"}]}`), 1, 1)
				mustAppendReply(t, ctx, pool, s, other, mustHead(t, ctx, pool, other), reply("msg_elsewhere", "b"), "end_turn")

				convID := newConv(t, ctx, s)
				head := runTurnFrom(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`),
					"msg_elsewhere", 1, 1)

				kind, derived, depth := mustNodeKind(t, ctx, pool, head)
				c.Eq("", kind, "kind")
				c.Eq("", derived, "derived_from_id: the named node is not this conversation's")
				c.Eq(1, depth, "the head is the request's second message")
				_, _, fallbacks := mustStats(t, ctx, pool, convID)
				c.Eq(1, fallbacks, "fallback_count")
			},
		},
		{
			// (g) The retry of the first post-/clear request: the flag is gone
			// by then, and the root the original created is where it lands.
			name: "the first request after a clear is a clear root, and its retry inserts nothing",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID, ref := clearTestConv(t, ctx, s, "attach-clear")
				runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"history"}]}`), 1, 1)
				c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending")

				fresh := []byte(`{"messages":[{"role":"user","content":"fresh after clear"}]}`)
				clearHead := runTurn(t, ctx, s, convID, fresh, 1, 1)
				kind, _, depth := mustNodeKind(t, ctx, pool, clearHead)
				c.Eq("clear", kind, "kind")
				c.Eq(0, depth, "root depth")
				c.False(clearPendingOf(t, ctx, pool, convID), "clear_pending is consumed with the root")
				nodes := mustNodeCount(t, ctx, pool, convID)

				retryHead := runTurn(t, ctx, s, convID, fresh, 1, 1)
				c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "the retry inserts nothing")
				c.Eq(clearHead, retryHead, "the retry lands on the root the original created")

				var clears int
				c.NoError(pool.QueryRow(ctx,
					`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='clear'`,
					convID).Scan(&clears), "count clear roots")
				c.Eq(1, clears, "clear roots")
			},
		},
		{
			// (h) The request replays exactly the history the named reply ends,
			// so there is nothing to insert: the head moves onto it.
			name: "a request whose attach message is its last message only moves the head",
			run: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore) {
				c := assert.NewAborting(t)
				convID := newConv(t, ctx, s)
				runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"a"}]}`), 1, 1)
				mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID), reply("msg_last", "the reply"), "end_turn")
				replyNode := mustProviderNode(t, ctx, pool, convID, "msg_last")
				nodes := mustNodeCount(t, ctx, pool, convID)

				head := runTurnFrom(t, ctx, s, convID,
					[]byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":[{"type":"text","text":"the reply"}]}]}`),
					"msg_last", 1, 1)

				c.Eq(replyNode, head, "the request head is the node it named")
				c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "no node was inserted")
				c.Eq(replyNode, mustHead(t, ctx, pool, convID), "head_id")
				var requestHead string
				c.NoError(pool.QueryRow(ctx, `
					SELECT coalesce(request_head_id::text, '') FROM conversations.conversation_turn
					 WHERE conversation_id=$1::uuid ORDER BY created_at DESC, id DESC LIMIT 1`, convID).Scan(&requestHead), "read request_head_id")
				c.Eq(replyNode, requestHead, "request_head_id")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, s := captureTestEnv(t)
			tc.run(t, ctx, pool, s)
		})
	}
}

// TestARequestWithNoMessagesAppendsNothing: a request carrying no history has
// nothing to attach, so the response hangs off the head the conversation
// already has. A conversation with no head has nowhere to put it, which is an
// error rather than a silent no-op.
func TestARequestWithNoMessagesAppendsNothing(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)

	empty := newConv(t, ctx, s)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: empty, Model: "m"})
	c.NoError(err, "InsertTurnIntent")
	_, err = s.DecomposeRequest(ctx, empty, turnID, createdAt, []byte(`{"messages":[]}`), "", "")
	c.Error(err, "a request with no messages in an empty conversation has no head to append under")

	convID := newConv(t, ctx, s)
	head := runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1, 1)
	nodes := mustNodeCount(t, ctx, pool, convID)

	turnID, createdAt, err = s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.NoError(err, "InsertTurnIntent")
	got, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, []byte(`{"messages":[]}`), "", "")
	c.NoError(err, "DecomposeRequest")
	c.Eq(head, got, "the request head is the conversation's head")
	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "no node was inserted")
}

// TestTwoRepliesToOneHistoryAreSiblings is the 868-failure class: a second
// reply to the same history used to collide with the first at its ordinal and
// be dropped. On the tree it is a sibling, and the fork counter says so.
func TestTwoRepliesToOneHistoryAreSiblings(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := newConv(t, ctx, s)
	head := runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1, 1)

	mustAppendReply(t, ctx, pool, s, convID, head,
		[]byte(`{"id":"msg_first","content":[{"type":"text","text":"first"}]}`), "end_turn")
	mustAppendReply(t, ctx, pool, s, convID, head,
		[]byte(`{"id":"msg_second","content":[{"type":"text","text":"second"}]}`), "end_turn")

	first := mustProviderNode(t, ctx, pool, convID, "msg_first")
	second := mustProviderNode(t, ctx, pool, convID, "msg_second")
	c.NotEq(first, second, "two different replies are two nodes")
	c.Eq(head, mustParent(t, ctx, pool, first), "first reply parent")
	c.Eq(head, mustParent(t, ctx, pool, second), "second reply parent")

	_, forks, _ := mustStats(t, ctx, pool, convID)
	c.Eq(1, forks, "fork_count: one parent with two children")
}

// TestSameReplyTwiceMovesHead: the same reply re-appended is the same node — no
// row, but the head still moves onto it, because the head follows the last
// appended chain whether or not anything was inserted.
func TestSameReplyTwiceMovesHead(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := newConv(t, ctx, s)
	head := runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1, 1)

	replyA := []byte(`{"id":"msg_a","content":[{"type":"text","text":"a"}]}`)
	replyB := []byte(`{"id":"msg_b","content":[{"type":"text","text":"b"}]}`)
	nodeA := mustAppendReply(t, ctx, pool, s, convID, head, replyA, "end_turn")
	nodeB := mustAppendReply(t, ctx, pool, s, convID, head, replyB, "end_turn")
	c.NotEq(nodeA, nodeB, "a different reply is a different node")

	nodes := mustNodeCount(t, ctx, pool, convID)
	mustAppendReply(t, ctx, pool, s, convID, head, replyA, "end_turn")
	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "the same reply twice inserts nothing")
	c.Eq(nodeA, mustHead(t, ctx, pool, convID), "head_id moved back to the re-appended reply")
}

// TestToolResultsWithSameIDDifferentContentAreSiblings is the regression the
// SDK round trip caused: decoding content into the API's block union dropped
// tool_result.content, so two different tool results looked like one message
// and the second was silently taken as a replay of the first.
func TestToolResultsWithSameIDDifferentContentAreSiblings(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := newConv(t, ctx, s)
	runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1, 1)
	mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID),
		[]byte(`{"id":"msg_tool","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"x"}}]}`),
		"tool_use")

	request := func(result string) []byte {
		return []byte(`{"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"x"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"` + result + `"}]}]}`)
	}
	runTurnFrom(t, ctx, s, convID, request("A"), "msg_tool", 1, 1)
	runTurnFrom(t, ctx, s, convID, request("B"), "msg_tool", 1, 1)

	nodeA := mustNodeByContent(t, ctx, pool, convID, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"A"}]`)
	nodeB := mustNodeByContent(t, ctx, pool, convID, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"B"}]`)
	c.NotEq(nodeA, nodeB, "two different tool results are two nodes")
	reply := mustProviderNode(t, ctx, pool, convID, "msg_tool")
	c.Eq(reply, mustParent(t, ctx, pool, nodeA), "tool result A's parent")
	c.Eq(reply, mustParent(t, ctx, pool, nodeB), "tool result B's parent")
}

// fixtureReplay drives testdata/claude-capture through capture exactly as the
// proxy does: each request attaches by its diagnostics.previous_message_id and
// its response is appended under the request head. ref is the conversation's
// external_ref, which MarkClearPending resolves. before, when non-nil, is called
// with the conversation's head immediately before each request is attached, so a
// test can pin what a boundary was cut from.
func fixtureReplay(t *testing.T, ctx context.Context, s *CaptureStore, convID, ref string, before func(n int, headBefore string)) {
	t.Helper()
	c := assert.NewAborting(t)
	dir := filepath.Join("..", "..", "testdata", "claude-capture")
	for n := 1; ; n++ {
		name := fmt.Sprintf("%06d-request.http", n)
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			break
		}
		req := captureFixtureBody(t, dir, name)
		var body struct {
			Messages    []json.RawMessage `json:"messages"`
			Diagnostics struct {
				PreviousMessageID string `json:"previous_message_id"`
			} `json:"diagnostics"`
		}
		c.NoError(json.Unmarshal(req, &body), "request %d body", n)
		if before != nil {
			var headBefore string
			c.NoError(s.pool.QueryRow(ctx,
				`SELECT coalesce(head_id::text, '') FROM conversations.conversation WHERE id = $1::uuid`,
				convID).Scan(&headBefore), "read the head before request %d", n)
			before(n, headBefore)
		}
		if n > 1 && len(body.Messages) == 1 {
			// The one-message request after a long history is the turn after
			// /clear. The daemon marks the conversation when it accepts the
			// slash command; this replay has no daemon.
			c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending before request %d", n)
		}
		turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, Protocol: "anthropic"})
		c.NoError(err, "InsertTurnIntent request %d", n)
		head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, "", body.Diagnostics.PreviousMessageID)
		c.NoError(err, "DecomposeRequest request %d", n)

		_, _, canonical, _, err := routing.ParseCapturedResponse("text/event-stream", captureFixtureBody(t, dir, fmt.Sprintf("%06d-response.http", n)))
		c.NoError(err, "ParseCapturedResponse response %d", n)
		c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, head, canonical, 1, 1, "end_turn"), "AppendResponseMessage response %d", n)
	}
}

// TestReplayTwiceAddsNoRows: the whole fixture, replayed a second time, is the
// same tree — every node hashes to the node it already occupies.
func TestReplayTwiceAddsNoRows(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "replay-twice")

	fixtureReplay(t, ctx, s, convID, ref, nil)
	first := mustNodeCount(t, ctx, pool, convID)
	c.Greater(0, first, "the first replay inserted nodes")

	fixtureReplay(t, ctx, s, convID, ref, nil)
	c.Eq(first, mustNodeCount(t, ctx, pool, convID), "node count after the second replay")
}

// TestBackfillMatchesTreeCapture: the backfill over the interim (pre-tree)
// capture rows and live tree capture of the same session must agree node for
// node — the same set of path hashes, each under the same parent hash. It is
// the cutover's real question: does the tree we build from stored rows equal
// the tree capture builds now?
func TestBackfillMatchesTreeCapture(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)

	raw, err := os.ReadFile(filepath.Join("testdata", "interim-capture-rows.json"))
	c.NoError(err, "read the interim capture snapshot")
	var snapshot struct {
		Messages []struct {
			Ordinal     int             `json:"ordinal"`
			Role        string          `json:"role"`
			Content     json.RawMessage `json:"content"`
			Kind        *string         `json:"kind"`
			InputTokens *int64          `json:"input_tokens"`
		} `json:"messages"`
	}
	c.NoError(json.Unmarshal(raw, &snapshot), "unmarshal the interim capture snapshot")
	c.Greater(0, len(snapshot.Messages), "snapshot messages")

	// A: the rows the pre-tree capture wrote, then the backfill.
	backfilled := newConv(t, ctx, s)
	for _, m := range snapshot.Messages {
		var kind any
		if m.Kind != nil {
			kind = *m.Kind
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO conversations.conversation_message
				(conversation_id, ordinal, role, content, kind, input_tokens)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)`,
			backfilled, m.Ordinal, m.Role, m.Content, kind, m.InputTokens); err != nil {
			t.Fatalf("insert interim row %d: %v", m.Ordinal, err)
		}
	}
	tx, err := pool.Begin(ctx)
	c.NoError(err, "begin")
	report, err := store.BackfillConversationTree(ctx, tx, backfilled)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("BackfillConversationTree: %v", err)
	}
	c.NoError(tx.Commit(ctx), "commit")
	c.Eq(0, report.UnmatchedCopies, "the backfill matched every interim copy")

	// B: the same session captured onto the tree.
	convID, ref := clearTestConv(t, ctx, s, "backfill-parity")
	fixtureReplay(t, ctx, s, convID, ref, nil)

	want := treeShape(t, ctx, pool, backfilled)
	got := treeShape(t, ctx, pool, convID)
	c.Greater(0, len(got), "tree capture placed nodes")
	c.EqDeep(want, got, "the backfilled tree and the captured tree")
}

// treeShape maps every placed node's path hash to its parent's path hash ("" at
// a root).
func treeShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string) map[string]string {
	t.Helper()
	c := assert.NewAborting(t)
	rows, err := pool.Query(ctx, `
		SELECT encode(m.path_hash, 'hex'), coalesce(encode(p.path_hash, 'hex'), '')
		  FROM conversations.conversation_message m
		  LEFT JOIN conversations.conversation_message p ON p.id = m.parent_id
		 WHERE m.conversation_id = $1::uuid AND m.path_hash IS NOT NULL`, convID)
	c.NoError(err, "read tree shape")
	defer rows.Close()
	shape := map[string]string{}
	for rows.Next() {
		var hash, parent string
		c.NoError(rows.Scan(&hash, &parent), "scan tree shape")
		shape[hash] = parent
	}
	c.NoError(rows.Err(), "tree shape")
	return shape
}

// TestCapturedClaudeSessionReplays proves every request in a real Claude Code
// session (testdata/claude-capture, recorded by `make capture-claude`) attaches
// by its diagnostics.previous_message_id: no request falls back to hash
// matching or to a new root, and no request re-stores history as tail copies.
// The session covers text, thinking, tool_use and tool_result turns, a parallel
// tool call, a failed tool call, a session killed on an unanswered tool_use and
// resumed (Claude Code replays a synthetic "No response requested." where the
// stored reply was), /compact (whose summarization request rewrites the last
// user message) and /clear.
//
// Only structure is pinned, not ordinals: a re-record changes the model's
// words, and a different tool-call split would move every ordinal.
func TestCapturedClaudeSessionReplays(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "captured-session")

	// The head each request starts from, so a boundary can be pinned to the
	// node it actually cut rather than merely to "some node".
	heads := map[int]string{}
	fixtureReplay(t, ctx, s, convID, ref, func(n int, headBefore string) { heads[n] = headBefore })

	_, _, fallbacks := mustStats(t, ctx, pool, convID)
	c.Eq(0, fallbacks, "no fixture request needed the hash-chain fallback or a new root")

	var tails int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='compaction_tail'`,
		convID).Scan(&tails), "count tail copies")
	c.Eq(0, tails, "a rewritten message is a sibling, not a re-stored prefix")

	// Request 14 (the resume after the killed tool call) names request 12's
	// reply, so its synthetic assistant message lands beside the tool_use that
	// request 13's response stored: same parent, different node.
	synthetic := mustNodeLike(t, ctx, pool, convID, "No response requested.")
	toolUse := mustProviderNode(t, ctx, pool, convID, "msg_011CgTEqnHbu2fv4EvpwgekL")
	c.NotEq(synthetic, toolUse, "the synthetic message is not the stored tool_use")
	c.Eq(mustParent(t, ctx, pool, toolUse), mustParent(t, ctx, pool, synthetic), "the synthetic message is a sibling of the stored tool_use")

	// Request 18 (the summarization request's sibling) extends one user message
	// with the summarization instruction: a second user node under the same
	// parent. The original is that one tool_result block alone; the extension
	// carries a second block.
	var original, extended string
	extRows, err := pool.Query(ctx, `
		SELECT id::text,
		       CASE WHEN jsonb_typeof(content) = 'array' THEN jsonb_array_length(content) ELSE 0 END,
		       CASE WHEN jsonb_typeof(content) = 'array' THEN coalesce(content->0->>'tool_use_id', '') ELSE '' END
		  FROM conversations.conversation_message WHERE conversation_id=$1::uuid`, convID)
	c.NoError(err, "read the extended history")
	defer extRows.Close()
	for extRows.Next() {
		var (
			id        string
			blocks    int
			toolUseID string
		)
		c.NoError(extRows.Scan(&id, &blocks, &toolUseID), "scan node")
		if toolUseID != "toolu_01NPaxnkx7ggZAtjuPJ7S7ZT" {
			continue
		}
		switch blocks {
		case 1:
			original = id
		case 2:
			extended = id
		}
	}
	c.NoError(extRows.Err(), "nodes")
	c.NotEq("", original, "the original user message")
	c.NotEq("", extended, "the extended user message")
	c.NotEq(original, extended, "the extended message is its own node")
	c.Eq(mustParent(t, ctx, pool, original), mustParent(t, ctx, pool, extended), "the extended message is a sibling of the original")

	// Request 19 opens the post-compaction history as a root derived from the
	// reply it names (request 17's).
	var derived string
	var kind string
	c.NoError(pool.QueryRow(ctx, `
		SELECT coalesce(derived_from_id::text, ''), coalesce(kind, '')
		  FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='compaction_summary'`,
		convID).Scan(&derived, &kind), "read the compaction root")
	c.Eq("compaction_summary", kind, "kind")
	c.Eq(mustProviderNode(t, ctx, pool, convID, "msg_011CgTErFWwoKjKDr6q9xVGb"), derived, "the compaction root is derived from the reply request 19 names")

	// Request 20 is the /clear: a clear root, derived from the node that was the
	// conversation's head immediately before that request — request 19's reply.
	c.NotEq("", heads[20], "the head before the /clear request")
	var clearDerived string
	var clearParent *string
	c.NoError(pool.QueryRow(ctx, `
		SELECT coalesce(derived_from_id::text, ''), parent_id::text
		  FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='clear'`,
		convID).Scan(&clearDerived, &clearParent), "read the clear root")
	c.Nil(clearParent, "the clear root has no parent")
	c.Eq(heads[20], clearDerived, "the clear root is derived from the node it cut, not from another node")
	c.Eq(mustProviderNode(t, ctx, pool, convID, "msg_011CgTEsRpqvQnCmgkCDZFrV"), clearDerived,
		"the clear root is derived from request 19's reply")

	// The brief's acceptance clause, for this conversation: the row count equals
	// the number of distinct non-NULL path_hash values, so every row is exactly
	// one node — no row without a tree position and no node stored twice.
	var rows, placed int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT path_hash) FROM conversations.conversation_message WHERE conversation_id=$1::uuid`,
		convID).Scan(&rows, &placed), "count rows and path hashes")
	c.Eq(rows, placed, "row count equals the number of distinct non-NULL path_hash values")

	var responses, distinct int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT provider_message_id) FROM conversations.conversation_message
		 WHERE conversation_id=$1::uuid AND provider_message_id IS NOT NULL`,
		convID).Scan(&responses, &distinct), "count response nodes")
	c.Eq(20, responses, "every captured response is a node with its provider id")
	c.Eq(20, distinct, "each response id names exactly one node")
}

// TestARequestNamingItsOwnReplyIsTheSameNode pins the tolerance capture needs on
// real data: the stored reply is the SDK's marshalled message (every block-union
// field present, zero-valued when unused) while the client's replay of it is the
// provider's own JSON. They must be the same message, or every straight
// extension in a real session would fork.
func TestARequestNamingItsOwnReplyIsTheSameNode(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := newConv(t, ctx, s)

	head := runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1, 1)
	mustAppendReply(t, ctx, pool, s, convID, head,
		[]byte(`{"id":"msg_union","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"x"}}]}`),
		"tool_use")
	replyNode := mustProviderNode(t, ctx, pool, convID, "msg_union")
	nodes := mustNodeCount(t, ctx, pool, convID)

	// The client's replay: no zero-valued union fields, and a provider field the
	// SDK does not model.
	replay := []byte(`{"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"x"},"caller":{"type":"direct"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	newHead := runTurnFrom(t, ctx, s, convID, replay, "msg_union", 1, 1)

	c.Eq(nodes+1, mustNodeCount(t, ctx, pool, convID), "only the tool result is new")
	c.Eq(replyNode, mustParent(t, ctx, pool, newHead), "the tool result hangs off the replayed reply")
	_, _, fallbacks := mustStats(t, ctx, pool, convID)
	c.Eq(0, fallbacks, "a replayed reply is not a fallback")
}

// mustAppendReply inserts a turn row and appends canonical under parentID, the
// way the proxy does, and returns the conversation's head afterwards.
func mustAppendReply(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *CaptureStore, convID, parentID string, canonical []byte, stopReason string) string {
	t.Helper()
	c := assert.NewAborting(t)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.NoError(err, "InsertTurnIntent")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, parentID, canonical, 1, 1, stopReason), "AppendResponseMessage")
	return mustHead(t, ctx, pool, convID)
}
