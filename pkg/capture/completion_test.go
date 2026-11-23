// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// newTracedStore is newTestStore with a statement tracer on the pool, for the
// tests that pin what one completion costs.
func newTracedStore(t *testing.T) (*CaptureStore, *pgxpool.Pool, *stmtTracer) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is; the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	tr := &stmtTracer{counts: map[string]int{}}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
	return NewCaptureStore(pool), pool, tr
}

// newCompletionConversation creates a conversation of its own for a completion
// test: the test database is shared and never reset, so every assertion is
// scoped to a conversation the test created.
func newCompletionConversation(t *testing.T, s *CaptureStore, session string) string {
	t.Helper()
	convID, err := s.EnsureConversationByExternalRef(context.Background(), ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session})
	assert.NewAborting(t).NoError(err, "EnsureConversationByExternalRef")
	return convID
}

// insertCompletionTurn write-aheads one turn's intent and returns its key.
func insertCompletionTurn(t *testing.T, s *CaptureStore, convID, model, prefix string) (string, time.Time) {
	t.Helper()
	turnID, createdAt, err := s.InsertTurnIntent(context.Background(), TurnIntent{
		ConversationID: convID, Model: model, Source: "claude", PrefixHash: prefix})
	assert.NewAborting(t).NoError(err, "InsertTurnIntent")
	return turnID, createdAt
}

// completion builds a TurnCompletion for one call: the outcome, and the request
// and response to record.
func completion(turnID string, createdAt time.Time, convID, session, prev, own string, req []byte, prefix string, canonical []byte) TurnCompletion {
	return TurnCompletion{
		TurnResult: TurnResult{TurnID: turnID, CreatedAt: createdAt, ConversationID: convID,
			Model: "m", StopReason: "end_turn", Upstream: "anthropic",
			InputTokens: 10, OutputTokens: 4, CacheReadTokens: 3, CacheCreationTokens: 2, LatencyMS: 7},
		Session:       session,
		PrevMessageID: prev,
		OwnMessageID:  own,
		ReqBody:       req,
		PrefixHash:    prefix,
		Canonical:     canonical,
	}
}

// canonicalReply renders a canonical Anthropic response envelope.
func canonicalReply(id, text string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}`, id, text))
}

// completedTurn is the turn row a completion writes, as the test reads it back.
type completedTurn struct {
	Status                 string
	Model                  string
	Upstream               string
	StopReason             string
	Error                  string
	Response               string
	ResponseID             string
	ServedProvider         string
	Input, Output          int64
	CacheRead, CacheCreate int64
	Latency                int
	ResponseMessageID      string
	ThreadID               string
	RequestHeadID          string
	ResponseOrdinal        int
	CacheBreakpoints       string
	PrefixContent          string
}

func readCompletedTurn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, turnID string) completedTurn {
	t.Helper()
	var r completedTurn
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT status, coalesce(model,''), coalesce(upstream,''), coalesce(stop_reason,''), coalesce(error,''),
		        coalesce(response::text,''), coalesce(response->>'id',''), coalesce(served_provider,''),
		        coalesce(input_tokens,0), coalesce(output_tokens,0), coalesce(cache_read_tokens,0), coalesce(cache_creation_tokens,0),
		        coalesce(latency_ms,0),
		        coalesce(response_message_id,''), coalesce(thread_id::text,''), coalesce(request_head_id::text,''),
		        coalesce(response_ordinal,-1), coalesce(cache_breakpoints::text,''), coalesce(prefix_content::text,'')
		   FROM conversations.conversation_turn WHERE id=$1::uuid`, turnID).Scan(
		&r.Status, &r.Model, &r.Upstream, &r.StopReason, &r.Error, &r.Response, &r.ResponseID, &r.ServedProvider,
		&r.Input, &r.Output, &r.CacheRead, &r.CacheCreate, &r.Latency,
		&r.ResponseMessageID, &r.ThreadID, &r.RequestHeadID, &r.ResponseOrdinal,
		&r.CacheBreakpoints, &r.PrefixContent), "read the turn row")
	return r
}

// mustReplyOrdinal reads the tree ordinal of the node a provider message id names.
func mustReplyOrdinal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID, providerMessageID string) int {
	t.Helper()
	var ord int
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT ordinal FROM conversations.conversation_message
		  WHERE conversation_id=$1::uuid AND provider_message_id=$2`, convID, providerMessageID).Scan(&ord),
		"read the reply's ordinal")
	return ord
}

// The whole call is one transaction: the request's nodes, the reply and the
// turn row's outcome commit together, so nothing can record a reply the tree
// does not hold.
func TestCompleteCapturedTurnWritesTheWholeCallInOneTransaction(t *testing.T) {
	s, pool, tr := newTracedStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "one-tx-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "m", prefix)

	tr.reset()
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turnID, createdAt, convID, session, "",
		"msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))), "CompleteCapturedTurn")
	c.Eq(1, tr.countOf("commit"), "the whole call commits exactly once")

	row := readCompletedTurn(t, ctx, pool, turnID)
	c.Eq("complete", row.Status, "status")
	c.Eq(int64(10), row.Input, "input tokens")
	c.Eq(int64(4), row.Output, "output tokens")
	c.Eq(int64(3), row.CacheRead, "cache read tokens")
	c.Eq(int64(2), row.CacheCreate, "cache creation tokens")
	c.Eq(7, row.Latency, "latency")
	c.Eq("anthropic", row.Upstream, "upstream")
	c.Eq("msg_reply", row.ResponseMessageID, "response message id")
	c.Eq("", row.ThreadID, "thread id stays NULL on the main thread")
	c.Eq("[]", row.CacheBreakpoints, "no message carries a cache_control marker")
	c.NotEmpty(row.PrefixContent, "the first turn stores its prefix")

	requestNode := mustNodeByContent(t, ctx, pool, convID, `"hi"`)
	c.Eq(requestNode, row.RequestHeadID, "the turn points at the request's head")
	c.Eq(mustReplyOrdinal(t, ctx, pool, convID, "msg_reply"), row.ResponseOrdinal, "the turn points at the reply's ordinal")
	c.Eq(requestNode, mustParent(t, ctx, pool, mustProviderNode(t, ctx, pool, convID, "msg_reply")),
		"the reply hangs under the request node")
}

// A key that no longer names one row fails the call and takes the appended
// nodes with it: the reply and the turn row never disagree.
func TestCompleteCapturedTurnStaleKeyRollsBackTheNodes(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "stale-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "m", prefix)

	stale := completion(turnID, createdAt, convID, session, "", "msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))
	stale.CreatedAt = stale.CreatedAt.Add(time.Second)
	c.ErrorContains(s.CompleteCapturedTurn(ctx, stale), "expected 1 row", "a skewed key is refused")

	c.Eq(0, mustNodeCount(t, ctx, pool, convID), "the append rolled back with the turn update")
	c.Eq("pending", readCompletedTurn(t, ctx, pool, turnID).Status, "the real turn row was never completed")
}

// The turn row carries the call's whole outcome: the stored response body, the
// stop reason, the served provider, the response model and the thread the call
// was classified into.
func TestCompleteCapturedTurnRecordsTheCallsOutcomeAndThread(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "outcome-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "", prefix)

	thread := uuid.Must(uuid.NewV7()).String()
	s.resolveThreadFn = func(context.Context, string, string, string, string, bool) (string, error) {
		return thread, nil
	}
	t.Cleanup(func() { s.resolveThreadFn = nil })

	call := completion(turnID, createdAt, convID, session, "", "msg_out", req, prefix, canonicalReply("msg_out", "ok"))
	call.IsSubagent = true
	call.Response = canonicalReply("msg_out", "ok")
	call.ServedProvider = "anthropic"
	call.StopReason = "tool_use"
	c.NoError(s.CompleteCapturedTurn(ctx, call), "CompleteCapturedTurn")

	row := readCompletedTurn(t, ctx, pool, turnID)
	c.Eq("complete", row.Status, "status")
	c.Eq(thread, row.ThreadID, "the classified thread is stamped")
	c.Eq("tool_use", row.StopReason, "stop reason")
	c.Eq("anthropic", row.ServedProvider, "served provider")
	c.Eq("m", row.Model, "the response model fills the turn")
	c.Eq("msg_out", row.ResponseID, "the canonical response body is stored")
	c.NotEmpty(row.Response, "and its content came with it")
}

// A re-issued completion (the commit landed, the caller saw an error) writes
// the same rows and the same turn values: the hook is a constant of the call.
func TestCompleteCapturedTurnTwiceIsIdempotent(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "twice-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "m", prefix)
	call := completion(turnID, createdAt, convID, session, "", "msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))

	c.NoError(s.CompleteCapturedTurn(ctx, call), "first")
	before := turnJSON(t, ctx, pool, turnID)
	nodes := mustNodeCount(t, ctx, pool, convID)

	c.NoError(s.CompleteCapturedTurn(ctx, call), "second")
	c.Eq(before, turnJSON(t, ctx, pool, turnID), "the turn row is unchanged by the replay")
	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "no node is written twice")
}

func turnJSON(t *testing.T, ctx context.Context, pool *pgxpool.Pool, turnID string) string {
	t.Helper()
	var out string
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT to_jsonb(t)::text FROM conversations.conversation_turn t WHERE t.id=$1::uuid`, turnID).Scan(&out),
		"read the turn row as JSON")
	return out
}

// A request whose whole history the conversation already holds appends nothing:
// the reply hangs under the matched head and no request node is duplicated.
func TestCompleteCapturedTurnMatchedRequestAttachesTheReplyUnderTheHead(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "matched-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)

	req1 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix1 := routing.PrefixHash(req1)
	turn1, at1 := insertCompletionTurn(t, s, convID, "m", prefix1)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn1, at1, convID, session, "",
		"msg_1", req1, prefix1, canonicalReply("msg_1", "ok"))), "turn 1")

	req2 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`)
	prefix2 := routing.PrefixHash(req2)
	turn2, at2 := insertCompletionTurn(t, s, convID, "m", prefix2)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn2, at2, convID, session, "msg_1",
		"msg_2", req2, prefix2, canonicalReply("msg_2", "second"))), "turn 2")

	head := mustProviderNode(t, ctx, pool, convID, "msg_1")
	reply := mustProviderNode(t, ctx, pool, convID, "msg_2")
	c.Eq(head, mustParent(t, ctx, pool, reply), "the reply hangs under the matched head")
	c.Eq(3, mustNodeCount(t, ctx, pool, convID), "the re-sent history added no node")
	// A request that appended nothing is still the turn's request head, so the
	// head comes from the plan's parent rather than from a node we inserted.
	row := readCompletedTurn(t, ctx, pool, turn2)
	c.Eq(head, row.RequestHeadID, "the turn points at the matched head")
	c.Eq(mustReplyOrdinal(t, ctx, pool, convID, "msg_2"), row.ResponseOrdinal, "and at the reply's ordinal")
}

// A request carrying no history appends nothing and hangs its reply under the
// head the conversation already has.
func TestCompleteCapturedTurnWithNoHistoryHangsUnderTheHead(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "nohistory-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)

	req1 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix1 := routing.PrefixHash(req1)
	turn1, at1 := insertCompletionTurn(t, s, convID, "m", prefix1)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn1, at1, convID, session, "",
		"msg_1", req1, prefix1, canonicalReply("msg_1", "ok"))), "turn 1")
	head := mustHead(t, ctx, pool, convID)
	c.Eq(mustProviderNode(t, ctx, pool, convID, "msg_1"), head, "turn 1's reply is the head")

	req2 := []byte(`{"model":"m","messages":[]}`)
	prefix2 := routing.PrefixHash(req2)
	turn2, at2 := insertCompletionTurn(t, s, convID, "m", prefix2)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn2, at2, convID, session, "",
		"msg_2", req2, prefix2, canonicalReply("msg_2", "second"))), "turn 2")

	c.Eq(head, mustParent(t, ctx, pool, mustProviderNode(t, ctx, pool, convID, "msg_2")),
		"the reply hangs under the head the request appended nothing to")
}

// A re-issued empty-history completion attaches to the head it read the first
// time: reading the head per attempt would hang the reply under the reply the
// first attempt stored.
func TestCompleteCapturedTurnWithNoHistoryTwiceIsIdempotent(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "nohistory-twice-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)

	req1 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix1 := routing.PrefixHash(req1)
	turn1, at1 := insertCompletionTurn(t, s, convID, "m", prefix1)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn1, at1, convID, session, "",
		"msg_1", req1, prefix1, canonicalReply("msg_1", "ok"))), "turn 1")
	head := mustHead(t, ctx, pool, convID)

	req2 := []byte(`{"model":"m","messages":[]}`)
	prefix2 := routing.PrefixHash(req2)
	turn2, at2 := insertCompletionTurn(t, s, convID, "m", prefix2)
	call := completion(turn2, at2, convID, session, "", "msg_2", req2, prefix2, canonicalReply("msg_2", "second"))
	c.NoError(s.CompleteCapturedTurn(ctx, call), "turn 2")
	after := readCompletedTurn(t, ctx, pool, turn2)
	nodes := mustNodeCount(t, ctx, pool, convID)

	c.NoError(s.CompleteCapturedTurn(ctx, call), "the same call again")
	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "the replay adds no node")
	c.Eq(head, mustParent(t, ctx, pool, mustProviderNode(t, ctx, pool, convID, "msg_2")),
		"the reply still hangs under the head the first attempt read")
	c.Eq(after, readCompletedTurn(t, ctx, pool, turn2), "and the turn row is unchanged")
}

// TestCompleteCapturedTurnReplayDoesNotRewindTheHead: a re-issued completion of
// a turn that already completed is a no-op. Its append would otherwise find the
// reply held and move the head back onto it, dropping every later turn from the
// working path.
func TestCompleteCapturedTurnReplayDoesNotRewindTheHead(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "rewind-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "m", prefix)
	call := completion(turnID, createdAt, convID, session, "", "msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))
	c.NoError(s.CompleteCapturedTurn(ctx, call), "the call")

	// A later turn advances the conversation past this call's reply.
	later := mustAppendReply(t, ctx, pool, s, convID, mustHead(t, ctx, pool, convID),
		canonicalReply("msg_later", "later"), "end_turn")
	nodes := mustNodeCount(t, ctx, pool, convID)
	before := readCompletedTurn(t, ctx, pool, turnID)

	c.NoError(s.CompleteCapturedTurn(ctx, call), "the replay is a success")
	c.Eq(later, mustHead(t, ctx, pool, convID), "the head stays on the later turn's node")
	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "the replay stores no node")
	c.Eq(before, readCompletedTurn(t, ctx, pool, turnID), "and rewrites no turn value")
}

// A failed commit after the hook ran leaves the model unwritten and the memo
// cold, so the next completion backfills it.
func TestCompleteCapturedTurnModelBackfillRollsBackWithTheCommit(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "backfillrollback-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "", prefix)

	s = s.WithConv(conv.NewStore(refusesAfterTheHook{Backend: store.NewConvBackend(pool)}, conv.Options{}))
	call := completion(turnID, createdAt, convID, session, "", "msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))
	c.Error(s.CompleteCapturedTurn(ctx, call), "a refused commit fails the call")

	c.False(s.memo.modelKnown(convID), "a refused commit leaves the memo cold")
	var model *string
	c.NoError(pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&model), "read model")
	c.True(model == nil, "and the backfill it ran was rolled back")
}

// refusesAfterTheHook is a backend that runs the commit's Finalize hook and then
// refuses the commit, so a statement the hook wrote is rolled back after it ran.
type refusesAfterTheHook struct{ conv.Backend }

func (b refusesAfterTheHook) Commit(ctx context.Context, cm conv.Commit) (time.Time, error) {
	if cm.Finalize != nil {
		if err := cm.Finalize(ctx, hookRanTx{}); err != nil {
			return time.Time{}, err
		}
	}
	return time.Time{}, errors.New("commit refused after the hook")
}

// hookRanTx is the transaction a refusing commit hands the hook: every statement
// reports one row, so the hook runs to its end.
type hookRanTx struct{}

func (hookRanTx) Exec(context.Context, string, ...any) (int64, error) { return 1, nil }

// A re-issued completion of a turn that already completed stores nothing, even
// when the reply carries no provider id: the turn row's own final state is the
// record, not an anchor looked up in the tree.
func TestCompleteCapturedTurnReplayWithoutAReplyIDStoresNothing(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "noid-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)

	req1 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix1 := routing.PrefixHash(req1)
	turn1, at1 := insertCompletionTurn(t, s, convID, "m", prefix1)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn1, at1, convID, session, "",
		"msg_1", req1, prefix1, canonicalReply("msg_1", "ok"))), "turn 1")

	req2 := []byte(`{"model":"m","messages":[]}`)
	prefix2 := routing.PrefixHash(req2)
	turn2, at2 := insertCompletionTurn(t, s, convID, "m", prefix2)
	call := completion(turn2, at2, convID, session, "", "", req2, prefix2,
		[]byte(`{"role":"assistant","content":[{"type":"text","text":"no id"}]}`))
	c.NoError(s.CompleteCapturedTurn(ctx, call), "turn 2")
	before := readCompletedTurn(t, ctx, pool, turn2)
	nodes := mustNodeCount(t, ctx, pool, convID)

	c.NoError(s.CompleteCapturedTurn(ctx, call), "the re-issued completion")
	c.Eq(before, readCompletedTurn(t, ctx, pool, turn2), "the completed turn is unchanged")
	c.Eq(nodes, mustNodeCount(t, ctx, pool, convID), "and the replay stores no node")
}

// A thread lookup that fails costs the turn its thread, never its record.
func TestCompleteCapturedTurnThreadFailureDoesNotFailTheCompletion(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "threadfail-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "m", prefix)

	s.resolveThreadFn = func(context.Context, string, string, string, string, bool) (string, error) {
		return "", errors.New("thread lookup exploded")
	}
	t.Cleanup(func() { s.resolveThreadFn = nil })

	c.NoError(s.CompleteCapturedTurn(ctx, completion(turnID, createdAt, convID, session, "",
		"msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))), "the completion still succeeds")

	row := readCompletedTurn(t, ctx, pool, turnID)
	c.Eq("complete", row.Status, "status")
	c.Eq("", row.ThreadID, "thread id stays NULL")
	// NULL thread_id reads as "the main thread", so the chain key must be NULL
	// too: a later turn must re-found its thread rather than inherit a guess.
	c.Eq("", row.ResponseMessageID, "and nothing chains through the missing thread")
	c.Eq(2, mustNodeCount(t, ctx, pool, convID), "the call's nodes are stored anyway")
}

// The conversation's model is remembered only once the transaction that wrote
// it commits: a failed completion leaves the memo cold for the next attempt. The
// backfill's own rollback is the hook contract's, pinned by
// TestConvBackendFinalizeErrorRollsBackTheInsertsAndTheHooksWrites (pkg/store): a
// hook statement that fails, or is followed by one that does, takes its writes
// with the commit.
func TestCompleteCapturedTurnMarksTheModelOnlyAfterCommit(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "modelmark-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	// The intent names no model, so nothing has marked the memo yet.
	turnID, createdAt := insertCompletionTurn(t, s, convID, "", prefix)
	model := func() string {
		var m *string
		c.NoError(pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&m), "read model")
		if m == nil {
			return ""
		}
		return *m
	}
	c.False(s.memo.modelKnown(convID), "nothing marked the model before the completion")
	c.Eq("", model(), "the conversation names no model yet")

	stale := completion(turnID, createdAt, convID, session, "", "msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))
	stale.CreatedAt = stale.CreatedAt.Add(time.Second)
	stale.Model = "served-model"
	c.ErrorContains(s.CompleteCapturedTurn(ctx, stale), "expected 1 row", "the stale key fails")
	c.False(s.memo.modelKnown(convID), "a failed completion does not mark the model")
	c.Eq("", model(), "and writes no model")

	c.NoError(s.CompleteCapturedTurn(ctx, completion(turnID, createdAt, convID, session, "",
		"msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))), "the retry succeeds")
	c.True(s.memo.modelKnown(convID), "a committed completion marks the model")
	c.Eq("m", model(), "and the response model fills the conversation")
}

// prefix_content is stored only when the static prefix changed; the markers are
// recorded every turn.
func TestCompleteCapturedTurnStoresPrefixOnlyWhenItChanged(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "prefix-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)

	body1 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix1 := routing.PrefixHash(body1)
	turn1, at1 := insertCompletionTurn(t, s, convID, "m", prefix1)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn1, at1, convID, session, "",
		"msg_1", body1, prefix1, canonicalReply("msg_1", "ok"))), "turn 1")
	c.NotEmpty(readCompletedTurn(t, ctx, pool, turn1).PrefixContent, "the first turn stores its prefix")

	turn2, at2 := insertCompletionTurn(t, s, convID, "m", prefix1)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn2, at2, convID, session, "",
		"msg_2", body1, prefix1, canonicalReply("msg_2", "second"))), "turn 2")
	row2 := readCompletedTurn(t, ctx, pool, turn2)
	c.Eq("", row2.PrefixContent, "an unchanged prefix is not stored again")
	c.Eq("[]", row2.CacheBreakpoints, "the markers are recorded on every turn")

	body3 := []byte(`{"model":"m2","messages":[{"role":"user","content":"hi"}]}`)
	prefix3 := routing.PrefixHash(body3)
	turn3, at3 := insertCompletionTurn(t, s, convID, "m2", prefix3)
	c.NoError(s.CompleteCapturedTurn(ctx, completion(turn3, at3, convID, session, "",
		"msg_3", body3, prefix3, canonicalReply("msg_3", "third"))), "turn 3")
	c.NotEmpty(readCompletedTurn(t, ctx, pool, turn3).PrefixContent, "a changed prefix is stored")
}

// A failed call keeps everything it learned about the call.
func TestFailCompletedTurnKeepsTheCallsUsage(t *testing.T) {
	s, pool, tr := newTracedStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "fail-usage-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "", "")

	tr.reset()
	c.NoError(s.FailCompletedTurn(ctx, TurnResult{TurnID: turnID, CreatedAt: createdAt, ConversationID: convID,
		Model: "served-model", StopReason: "max_tokens", Upstream: "anthropic",
		InputTokens: 11, OutputTokens: 5, CacheReadTokens: 3, CacheCreationTokens: 2, LatencyMS: 13}, "upstream exploded"),
		"FailCompletedTurn")
	total, detail := tr.snapshot()
	c.Eq(1, total, "the whole failure is one statement:\n%s", detail)

	row := readCompletedTurn(t, ctx, pool, turnID)
	c.Eq("error", row.Status, "status")
	c.Eq("upstream exploded", row.Error, "error text")
	c.Eq(int64(11), row.Input, "input tokens")
	c.Eq(int64(5), row.Output, "output tokens")
	c.Eq(int64(3), row.CacheRead, "cache read tokens")
	c.Eq(int64(2), row.CacheCreate, "cache creation tokens")
	c.Eq(13, row.Latency, "latency")
	c.Eq("max_tokens", row.StopReason, "stop reason")
	c.Eq("anthropic", row.Upstream, "upstream")
	c.Eq("served-model", row.Model, "model")
}

// A turn that already completed is never downgraded: the commit landed and the
// caller saw an error on the way back, so the call IS recorded and marking it
// errored would make the row disagree with the tree.
func TestFailCompletedTurnLeavesACompletedTurnAlone(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "failcomplete-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "m", prefix)
	call := completion(turnID, createdAt, convID, session, "", "msg_reply", req, prefix, canonicalReply("msg_reply", "ok"))
	c.NoError(s.CompleteCapturedTurn(ctx, call), "the call")
	before := readCompletedTurn(t, ctx, pool, turnID)

	c.NoError(s.FailCompletedTurn(ctx, call.TurnResult, "capture completion failed: ambiguous commit"),
		"a completed turn is not an error")
	c.Eq(before, readCompletedTurn(t, ctx, pool, turnID), "the completed row is untouched")
}

// A failure keyed to no row is refused rather than silently no-op'd.
func TestFailCompletedTurnStaleKeyIsAnError(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "fail-stale-" + uuid.NewString()
	convID := newCompletionConversation(t, s, session)
	turnID, createdAt := insertCompletionTurn(t, s, convID, "", "")

	c.ErrorContains(s.FailCompletedTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt.Add(time.Second), ConversationID: convID}, "boom"),
		"expected 1 row", "a skewed key is refused")
}
