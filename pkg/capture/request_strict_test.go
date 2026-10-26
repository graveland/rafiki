package capture

import (
	"context"
	"errors"
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

// An ordinal already holding a DIFFERENT message is a request message that was
// not stored; the caller must hear about it.
func TestRequestDifferentOccupantIsOrdinalOccupied(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "occ")

	c.Eq(2, runTurn(t, ctx, s, convID, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`), 1, 1), "seed")

	req := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"something else"}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	_, err = s.DecomposeRequest(ctx, convID, turnID, createdAt, req, "h")
	c.ErrorIs(err, ErrOrdinalOccupied, "decompose over a different occupant")
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

// A differing assistant message remains strict even at a captured response
// ordinal. Its meaningful tool-use input differs from the stored reply.
func TestRequestDifferentAssistantContentOnStoredReplyIsOrdinalOccupied(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
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
	_, err = s.DecomposeRequest(ctx, convID, turn2, createdAt2, req2, "h")
	c.ErrorIs(err, ErrOrdinalOccupied, "different assistant content")
}

// The leniency is for an ASSISTANT echo only. A user message landing on a
// stored reply is rewritten history and is still reported.
func TestRequestUserMessageOnStoredReplyIsOrdinalOccupied(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, _, s := horizonTestEnv(t)
	convID := strictConv(t, ctx, s, "reply-user")

	req1 := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	h1 := routing.PrefixHash(req1)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req1, PrefixHash: h1, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req1, h1)
	c.NoError(s.RecordThread(ctx, "", convID, turnID, createdAt, "", "msg_1", false), "RecordThread")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, []byte(storedReplyCanonical), 1, 1, "tool_use"),
		"AppendResponseMessage")

	req2 := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"rewritten"}]}`)
	turn2, createdAt2, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req2, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	_, err = s.DecomposeRequest(ctx, convID, turn2, createdAt2, req2, "h")
	c.ErrorIs(err, ErrOrdinalOccupied, "a user message on a stored reply")
}
