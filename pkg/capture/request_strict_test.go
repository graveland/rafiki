// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func strictConv(t *testing.T, ctx context.Context, s *CaptureStore, _ string) string {
	t.Helper()
	id, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	assert.NewAborting(t).NoError(err, "EnsureConversation")
	return id
}

func captureFixtureBody(t *testing.T, dir, name string) []byte {
	t.Helper()
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	assert.NewAborting(t).NoError(err, "read %s", path)
	_, body, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("%s is not an HTTP fixture", path)
	}
	return body
}

// A replayed prefix whose cache_control marker moved is the same message: the
// marker is a per-request caching hint, not content, so the replay hashes to
// the node it already occupies and adds only what is new.
func TestRequestReplayIgnoresMovedCacheControl(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := strictConv(t, ctx, s, "cc")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"yo","cache_control":{"type":"ephemeral"}}]}]}`)
	runTurn(t, ctx, s, convID, req1, 1, 1)

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"yo"}]},{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]}]}`)
	head := runTurn(t, ctx, s, convID, req2, 1, 1)

	c.Eq(3, mustNodeCount(t, ctx, pool, convID), "node count: hi, yo, q — the marker moved, the messages did not")
	c.Eq(mustNodeLike(t, ctx, pool, convID, `"yo"`), mustParent(t, ctx, pool, head), "q hangs off the replayed reply")
	_, _, fallbacks := mustStats(t, ctx, pool, convID)
	c.Eq(0, fallbacks, "a moved cache marker is not a fallback")
}

// Claude Code may replay a prior text-only assistant response as the API's
// plain-string shorthand while capture stored it as a text block. They are the
// same message.
func TestAPlainStringReplayOfAStoredReplyIsTheSameNode(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := strictConv(t, ctx, s, "reply-text")

	runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1, 1)
	mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID),
		[]byte(`{"id":"msg_text","content":[{"type":"text","text":"yo"}]}`), "end_turn")
	replyNode := mustProviderNode(t, ctx, pool, convID, "msg_text")
	nodes := mustNodeCount(t, ctx, pool, convID)

	head := runTurnFrom(t, ctx, s, convID,
		[]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"next"}]}`),
		"msg_text", 1, 1)

	c.Eq(nodes+1, mustNodeCount(t, ctx, pool, convID), "only the new user message is inserted")
	c.Eq(replyNode, mustParent(t, ctx, pool, head), "the new message hangs off the replayed reply")
}

// A user text block and its plain-string shorthand are the same message too, so
// a request that switches between them re-uses the node instead of creating one.
func TestAReplayedTextBlockIsTheSameAsItsPlainString(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := strictConv(t, ctx, s, "text-shorthand")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":[{"type":"text","text":"testing conversation storage","cache_control":{"type":"ephemeral"}}]}]}`)
	runTurn(t, ctx, s, convID, req1, 1, 1)

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"testing conversation storage"},{"role":"assistant","content":"next"}]}`)
	runTurn(t, ctx, s, convID, req2, 1, 1)

	c.Eq(4, mustNodeCount(t, ctx, pool, convID), "node count: hi, yo, the prompt, the reply")
	var roots int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND parent_id IS NULL`,
		convID).Scan(&roots), "count roots")
	c.Eq(1, roots, "one root: the replay attached to the history it already had")
}

// A retried request whose middle message was rewritten no longer matches the
// history it names nothing about, so the rewritten message becomes a sibling of
// the one stored and nothing stored is touched.
func TestARewrittenMessageBecomesASibling(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID := strictConv(t, ctx, s, "rewrite")

	runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`), 1, 1)

	runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"something else"},{"role":"user","content":"more"}]}`), 1, 1)

	c.Eq(5, mustNodeCount(t, ctx, pool, convID), "node count: the original three plus the rewritten pair")
	hi := mustNodeByContent(t, ctx, pool, convID, `"hi"`)
	c.Eq(hi, mustParent(t, ctx, pool, mustNodeByContent(t, ctx, pool, convID, `"something else"`)),
		"the rewritten message is a sibling of the stored one")

	// The stored history is untouched: "yo" and "more" are still where they were.
	c.Eq(mustNodeByContent(t, ctx, pool, convID, `"yo"`), mustParent(t, ctx, pool, mustNodeByContent(t, ctx, pool, convID, `"more"`)),
		"the stored more still hangs off the stored yo")
}

// A DB failure in the attach reaches the caller, which is what fails the turn.
func TestRequestInsertDBFailureIsReturned(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := captureTestEnv(t)
	_, err := s.DecomposeRequest(ctx,
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
		time.Now(),
		[]byte(`{"messages":[{"role":"user","content":"x"}]}`), "", "")
	c.Error(err, "decomposing against a conversation that does not exist")
}
