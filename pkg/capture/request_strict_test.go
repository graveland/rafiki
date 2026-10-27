package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/routing"
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

// TestCapturedClaudeSessionReplays proves every request in a real Claude Code
// session (testdata/claude-capture, recorded by `make capture-claude`) can be
// decomposed after capture writes the prior response through
// routing.ParseCapturedResponse. The session covers text, thinking, tool_use
// and tool_result turns whose wire representations change between requests, a
// parallel tool call, a failed tool call, a session killed on an unanswered
// tool_use and resumed (Claude Code replays a synthetic "No response requested."
// where the stored reply was), /compact (whose summarization request rewrites
// the last user message again) and /clear.
//
// Only structure is pinned, not ordinals: a re-record changes the model's
// words, and a different tool-call split would move every ordinal.
func TestCapturedClaudeSessionReplays(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, ref := clearTestConv(t, ctx, s, "captured-session")

	dir := filepath.Join("..", "..", "testdata", "claude-capture")
	requests, err := filepath.Glob(filepath.Join(dir, "*-request.http"))
	c.NoError(err, "glob captured requests")
	c.Greater(0, len(requests), "no captured requests")

	var next int
	for n := 1; n <= len(requests); n++ {
		req := captureFixtureBody(t, dir, fmt.Sprintf("%06d-request.http", n))
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		c.NoError(json.Unmarshal(req, &body), "request %d body", n)
		if n > 1 && len(body.Messages) == 1 {
			// The one-message request after a long history is the turn after
			// /clear. The daemon marks the conversation when it accepts the
			// slash command; this replay has no daemon.
			c.NoError(s.MarkClearPending(ctx, ref), "MarkClearPending before request %d", n)
		}

		turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, Protocol: "anthropic"})
		c.NoError(err, "InsertTurnIntent request %d", n)
		next, err = s.DecomposeRequest(ctx, convID, turnID, createdAt, req, "")
		c.NoError(err, "DecomposeRequest request %d", n)

		_, _, canonical, _, err := routing.ParseCapturedResponse("text/event-stream", captureFixtureBody(t, dir, fmt.Sprintf("%06d-response.http", n)))
		c.NoError(err, "ParseCapturedResponse response %d", n)
		c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 1, 1, "end_turn"), "AppendResponseMessage response %d", n)
	}

	// The shape of the stored conversation, in order, for whoever re-records it.
	rows, err := pool.Query(ctx,
		`SELECT ordinal, role, coalesce(kind, ''), left(regexp_replace(content::text, '\s+', ' ', 'g'), 80)
		   FROM conversations.conversation_message WHERE conversation_id=$1 ORDER BY ordinal`, convID)
	c.NoError(err, "read stored rows")
	defer rows.Close()
	kinds := map[string]int{}
	last := -1
	for rows.Next() {
		var ord int
		var role, kind, text string
		c.NoError(rows.Scan(&ord, &role, &kind, &text), "scan row")
		t.Logf("%3d %-9s %-18s %s", ord, role, kind, text)
		kinds[kind]++
		last = ord
	}
	c.NoError(rows.Err(), "rows")

	c.Eq(1, kinds["compaction_summary"], "one /compact, one boundary")
	c.Eq(1, kinds["clear"], "one /clear, one boundary")
	c.Greater(0, kinds["compaction_tail"], "rewritten history re-stores its prefix as tail copies")
	c.Eq(last, next, "the final reply is stored at the last ordinal")
}

// A replayed prefix whose cache_control marker moved is the same message: the
// marker is a per-request caching hint, not content.
func TestRequestReplayIgnoresMovedCacheControl(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "cc")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"yo","cache_control":{"type":"ephemeral"}}]}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 1, 1), "turn 1")
	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"yo"}]},{"role":"user","content":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 1, 1), "turn 2 replays ordinal 1 without the marker")
}

// A replay that rewrites a stored message mid-history (Claude Code swapping an
// orphaned tool_use for a synthetic reply, or the user rewinding) is a fork:
// the horizon moves past every stored row and the whole request is stored
// again, its unchanged prefix as compaction_tail copies readers skip. Nothing
// stored is touched.
func TestRequestRewriteOfStoredMessageForksTheHorizon(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "fork")

	c.Eq(3, runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`), 1, 1), "seed")

	req := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"something else"},{"role":"user","content":"more"}]}`)
	c.Eq(6, runTurn(t, ctx, s, convID, req, 1, 1), "next ordinal after the fork")
	c.Eq(3, storedHorizon(t, ctx, pool, convID), "horizon moved past the stored rows")

	for ord, want := range map[int]string{0: "hi", 1: "yo", 2: "more", 3: "hi", 4: "something else", 5: "more"} {
		_, _, _, content := requireKindRow(t, ctx, pool, convID, ord)
		c.Eq(fmt.Sprintf("%q", want), content, "content at ordinal %d", ord)
	}
	kind3, _, _, _ := requireKindRow(t, ctx, pool, convID, 3)
	c.Eq("compaction_tail", *kind3, "unchanged prefix is a tail copy")
	kind4, _, _, _ := requireKindRow(t, ctx, pool, convID, 4)
	c.Nil(kind4, "the rewritten message is a real row")

	// The next request replays the new history and appends at the new horizon.
	next := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"something else"},{"role":"user","content":"more"},{"role":"assistant","content":"ok"}]}`)
	c.Eq(7, runTurn(t, ctx, s, convID, next, 1, 1), "replay after the fork")
	c.Eq(3, storedHorizon(t, ctx, pool, convID), "no second fork")
}

// A divergent head with no compaction summary and no pending /clear used to
// warn and drop the request's messages; it is now an error.
func TestDivergentHeadIsAnError(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "div")

	c.Eq(2, runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"m0"},{"role":"assistant","content":"m1"}]}`), 1, 1), "seed")

	req := []byte(`{"messages":[{"role":"user","content":"fresh"}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	_, err = s.DecomposeRequest(ctx, convID, turnID, createdAt, req, "h")
	c.ErrorIs(err, ErrOrdinalOccupied, "divergent head")

	var n int
	c.NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1`, convID).Scan(&n), "count")
	c.Eq(2, n, "no row written for the divergent request")
}

// A DB failure in a request insert reaches the caller.
func TestRequestInsertDBFailureIsReturned(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	err := s.appendRequestMessage(ctx, "00000000-0000-0000-0000-000000000001", 0, "user", []byte(`"x"`), nil, nil)
	c.Error(err, "insert against a conversation that does not exist")
	c.False(errors.Is(err, ErrOrdinalOccupied), "a DB failure is not an occupant conflict")
}

// storedReplyCanonical is what AppendResponseMessage actually persists: the
// marshalled anthropic.Message, so every ContentBlockUnion field is present
// (zero-valued ones included) and a field the SDK does not model is gone.
const storedReplyCanonical = `{"id":"msg_1","type":"message","content":[
  {"type":"thinking","thinking":"plan","signature":"sig-1","id":"","data":"","name":"","text":"","input":null,
   "content":{"type":"web_search_tool_result_error","error_code":"","OfWebSearchToolResultBlockArray":null},
   "citations":null,"tool_use_id":""},
  {"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"x"},"data":"","text":"",
   "content":{"type":"web_search_tool_result_error","error_code":"","OfWebSearchToolResultBlockArray":null},
   "thinking":"","citations":null,"signature":"","tool_use_id":""}]}`

// The client replays the reply capture stored, and the replay is never
// content-equal to what was stored: the reply is persisted as the marshalled
// SDK message (zero-valued fields the provider never sent, `caller` and
// cache_control lost) while the replay carries the provider's own JSON. The
// provider-only fields are discarded by SDK canonicalization, so it must not
// fail the turn.
func TestRequestReplayOfStoredReplyIsNotOrdinalOccupied(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "reply")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h1 := routing.PrefixHash(req1)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req1, PrefixHash: h1, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req1, h1)
	c.NoError(err, "turn 1 decompose")
	c.NoError(s.RecordThread(ctx, "", convID, turnID, createdAt, "", "msg_1", false), "RecordThread")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, []byte(storedReplyCanonical), 1, 1, "tool_use"),
		"AppendResponseMessage")

	req2 := []byte(`{"messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":[
	    {"type":"thinking","thinking":"plan","signature":"sig-1"},
	    {"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"x"},
	     "caller":{"type":"direct"},"cache_control":{"type":"ephemeral"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 1, 1), "turn 2 replays the stored reply")

	// First-seen wins: the replayed echo does not overwrite capture's own row.
	var role, content string
	c.NoError(pool.QueryRow(ctx,
		`SELECT role, content::text FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND ordinal=1`,
		convID).Scan(&role, &content), "read ordinal 1")
	c.Eq("assistant", role, "role")
	c.StrContains(content, `"OfWebSearchToolResultBlockArray"`, "the stored reply is untouched")
}

const storedTextReplyCanonical = `{"id":"msg_text","type":"message","content":[{"type":"text","text":"yo"}]}`

// Claude Code may use the API's plain-string shorthand when replaying a prior
// text-only assistant response. Capture stored it as the SDK's text block, so
// the two forms must compare as the same response.
func TestRequestPlainTextReplayOfStoredReplyIsNotOrdinalOccupied(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "reply-text")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h1 := routing.PrefixHash(req1)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req1, PrefixHash: h1, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req1, h1)
	c.NoError(err, "turn 1 decompose")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, []byte(storedTextReplyCanonical), 1, 1, "end_turn"), "AppendResponseMessage")

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"next"}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 1, 1), "plain-text replay")
}

// Claude Code can change a user text block into Anthropic's equivalent plain
// string shorthand on the next request. The cache marker is also per-request,
// so this replay must not be treated as rewritten history.
func TestRequestReplayTextBlockAsPlainString(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "text-shorthand")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":[{"type":"text","text":"testing conversation storage","cache_control":{"type":"ephemeral"}}]}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req1, 1, 1), "seed text block")

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"testing conversation storage"},{"role":"assistant","content":"next"}]}`)
	c.Eq(4, runTurn(t, ctx, s, convID, req2, 1, 1), "replay plain string")
}

func TestRequestReplayDifferentPlainTextIsOrdinalOccupied(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "text-shorthand-different")

	req1 := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"original"}]}]}`)
	c.Eq(1, runTurn(t, ctx, s, convID, req1, 1, 1), "seed text block")

	req2 := []byte(`{"messages":[{"role":"user","content":"rewritten"}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req2, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	_, err = s.DecomposeRequest(ctx, convID, turnID, createdAt, req2, "h")
	c.ErrorIs(err, ErrOrdinalOccupied, "different text")
}

// A differing assistant message at a captured response ordinal is a rewrite of
// history, not a replay: the stored reply stays and the request forks the
// horizon.
func TestRequestDifferentAssistantContentOnStoredReplyForks(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "reply-other-id")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h1 := routing.PrefixHash(req1)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req1, PrefixHash: h1, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req1, h1)
	c.NoError(err, "turn 1 decompose")
	c.NoError(s.RecordThread(ctx, "", convID, turnID, createdAt, "", "msg_1", false), "RecordThread")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, []byte(storedReplyCanonical), 1, 1, "tool_use"), "AppendResponseMessage")

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"rewritten"}}]}]}`)
	turn2, createdAt2, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req2, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next2, err := s.DecomposeRequest(ctx, convID, turn2, createdAt2, req2, "h")
	c.NoError(err, "different assistant content forks")
	c.Eq(4, next2, "next ordinal after the fork")
	_, _, _, stored := requireKindRow(t, ctx, pool, convID, 1)
	c.True(strings.Contains(stored, "plan"), "the stored reply is untouched: %s", stored)
}

// The leniency is for an ASSISTANT echo only. A user message landing on a
// stored reply is rewritten history and forks the horizon.
func TestRequestUserMessageOnStoredReplyForks(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "reply-user")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h1 := routing.PrefixHash(req1)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req1, PrefixHash: h1, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req1, h1)
	c.NoError(err, "DecomposeRequest")
	c.NoError(s.RecordThread(ctx, "", convID, turnID, createdAt, "", "msg_1", false), "RecordThread")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, []byte(storedReplyCanonical), 1, 1, "tool_use"),
		"AppendResponseMessage")

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"rewritten"}]}`)
	turn2, createdAt2, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req2, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next2, err := s.DecomposeRequest(ctx, convID, turn2, createdAt2, req2, "h")
	c.NoError(err, "a user message on a stored reply forks")
	c.Eq(4, next2, "next ordinal after the fork")
	_, role, _, _ := requireKindRow(t, ctx, pool, convID, 1)
	c.Eq("assistant", *role, "the stored reply is untouched")
}

func TestDescribeOccupantMismatchNamesPathsNotContent(t *testing.T) {
	c := assert.NewAborting(t)
	stored := []byte(`[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"SECRET one"}}]`)
	replay := []byte(`[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"SECRET two"}}]`)
	got := describeOccupantMismatch("assistant", "assistant", stored, replay)
	c.True(strings.Contains(got, "block 0 differs at /input/command"), "got %s", got)
	c.True(strings.Contains(got, "blocks stored=[tool_use] replay=[tool_use]"), "got %s", got)
	c.True(!strings.Contains(got, "SECRET"), "diagnostic leaked content: %s", got)
}

// Postgres jsonb reorders object keys, so the stored tool_use input and the
// client's replay of it can differ only in key order.
func TestSameAnthropicContentIgnoresToolInputKeyOrder(t *testing.T) {
	c := assert.NewAborting(t)
	stored := []byte(`[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"query": "select:x", "max_results": 1}}]`)
	replay := []byte(`[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"max_results":1,"query":"select:x"},"caller":{"type":"direct"}}]`)
	c.True(sameAnthropicContent(stored, replay), "key order of a tool_use input is not content")

	changed := []byte(`[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"max_results":2,"query":"select:x"}}]`)
	c.True(!sameAnthropicContent(stored, changed), "a changed input value is content")
}
