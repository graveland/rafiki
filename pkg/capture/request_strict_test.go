package capture

import (
	"context"
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"
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
