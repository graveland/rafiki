// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// stmtTracer counts every statement a pool sends, by a short normalised
// prefix, so a test can pin how many round trips one captured turn costs.
type stmtTracer struct {
	mu     sync.Mutex
	counts map[string]int
	total  int
}

var wsRE = regexp.MustCompile(`\s+`)

func (t *stmtTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	key := wsRE.ReplaceAllString(strings.TrimSpace(d.SQL), " ")
	if len(key) > 70 {
		key = key[:70]
	}
	t.mu.Lock()
	t.counts[key]++
	t.total++
	t.mu.Unlock()
	return ctx
}

func (t *stmtTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countOf returns how many statements carried exactly sql, under the same
// normalisation the breakdown uses.
func (t *stmtTracer) countOf(sql string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[wsRE.ReplaceAllString(strings.TrimSpace(sql), " ")]
}

func (t *stmtTracer) reset() {
	t.mu.Lock()
	t.counts, t.total = map[string]int{}, 0
	t.mu.Unlock()
}

func (t *stmtTracer) snapshot() (int, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.counts))
	for k := range t.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return t.counts[keys[i]] > t.counts[keys[j]] || (t.counts[keys[i]] == t.counts[keys[j]] && keys[i] < keys[j])
	})
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %3d  %s\n", t.counts[k], k)
	}
	return t.total, b.String()
}

// capturedTurn replays the store calls one proxy turn of a claude session makes,
// in the proxy's order, and returns the response message id.
func capturedTurn(ctx context.Context, t *testing.T, s *CaptureStore, session string, k int, prevMsg string) string {
	t.Helper()
	c := assert.NewAborting(t)
	if prevMsg != "" {
		_, err := s.ThreadOfPredecessorInSession(ctx, session, prevMsg)
		c.NoError(err, "ThreadOfPredecessorInSession")
	}
	_, err := s.SessionFamilyExists(ctx, session)
	c.NoError(err, "SessionFamilyExists")
	convID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session, Model: "claude-test"})
	c.NoError(err, "EnsureConversationByExternalRef")

	var msgs []string
	for i := 1; i <= k; i++ {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":"question %d"}`, i))
		if i < k {
			msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"answer %d"}]}`, i))
		}
	}
	req := []byte(`{"model":"claude-test","messages":[` + strings.Join(msgs, ",") + `]}`)
	prefix := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Model: "claude-test", Source: "claude", PrefixHash: prefix})
	c.NoError(err, "InsertTurnIntent")

	ownMsg := fmt.Sprintf("msg_%d_%s", k, uuid.NewString()[:8])
	canonical := []byte(fmt.Sprintf(`{"id":%q,"role":"assistant","content":[{"type":"text","text":"answer %d"}],"stop_reason":"end_turn"}`, ownMsg, k))
	// The priced-turn read the proxy takes before the stream.
	_, err = s.ConversationTokens(ctx, convID)
	c.NoError(err, "ConversationTokens")
	c.NoError(s.CompleteCapturedTurn(ctx, TurnCompletion{
		TurnResult: TurnResult{TurnID: turnID, CreatedAt: createdAt, ConversationID: convID, Model: "claude-test",
			StopReason: "end_turn", Upstream: "anthropic", InputTokens: 5, OutputTokens: 5, LatencyMS: 10},
		Session:       session,
		PrevMessageID: prevMsg,
		OwnMessageID:  ownMsg,
		ReqBody:       req,
		PrefixHash:    prefix,
		Canonical:     canonical,
	}), "CompleteCapturedTurn")
	return ownMsg
}

// A captured turn is a fixed number of round trips, however long the session
// has run: this pins it, so a new read on the capture path shows up here.
func TestCapturedTurnStatementCount(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	c := assert.NewAborting(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	tr := &stmtTracer{counts: map[string]int{}}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
	s := NewCaptureStore(pool)

	session := "stmtcount-" + uuid.NewString()
	prev := ""
	var warm int
	for k := 1; k <= 6; k++ {
		tr.reset()
		start := time.Now()
		prev = capturedTurn(ctx, t, s, session, k, prev)
		total, detail := tr.snapshot()
		cs := s.ConvStore().CacheStats()
		t.Logf("turn %d: %d statements in %s  cache: nodes=%d path hit/miss=%d/%d node hit/miss=%d/%d head served=%d installs=%d\n%s", k, total, time.Since(start).Round(time.Millisecond),
			cs.Nodes, cs.PathHits, cs.PathMisses, cs.NodeHits, cs.NodeMisses, cs.HeadServed, cs.HeadInstalls, detail)
		warm = total
	}
	// The ratchet: a warm turn of a long session costs this many round trips,
	// one of them the commit that carries the whole call. Lower it when a read
	// is removed; a new read on the capture path fails here.
	const warmBudget = 19
	c.True(warm <= warmBudget, "a warm captured turn sent %d statements, budget %d", warm, warmBudget)
}

// The model backfill is first-seen-wins, so once the column is set a later
// UPDATE cannot match. Skipping it must not break the first fill, must not
// rewrite a set model, and must retry after a failed attempt.
func TestModelBackfillFillsOnceAndIsThenSkipped(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	convID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: "model-" + uuid.NewString()})
	c.NoError(err, "ensure")
	model := func() string {
		var m *string
		c.NoError(pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&m), "read model")
		if m == nil {
			return ""
		}
		return *m
	}
	c.Eq("", model(), "a client-driven conversation starts with no model")

	_, _, err = s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "first-model"})
	c.NoError(err, "turn 1")
	c.Eq("first-model", model(), "the first turn's model fills it")
	c.True(s.memo.modelKnown(convID), "and the store remembers it is set")

	_, _, err = s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "second-model"})
	c.NoError(err, "turn 2")
	c.Eq("first-model", model(), "first-seen wins: a later model never rewrites it")

	// A store that has not seen the conversation (a restart) must still fill a NULL.
	fresh := NewCaptureStore(pool)
	other, err := fresh.EnsureConversationByExternalRef(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: "model2-" + uuid.NewString()})
	c.NoError(err, "ensure other")
	_, _, err = fresh.InsertTurnIntent(ctx, TurnIntent{ConversationID: other, Model: "late-model"})
	c.NoError(err, "fresh store turn")
	var got string
	c.NoError(pool.QueryRow(ctx, `SELECT coalesce(model,'') FROM conversations.conversation WHERE id=$1::uuid`, other).Scan(&got), "read")
	c.Eq("late-model", got, "an unknown conversation is still backfilled")
}

func TestCompleteTurnBackfillsTheModelWhenTheIntentHadNone(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	convID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: "cm-" + uuid.NewString()})
	c.NoError(err, "ensure")
	turnID, at, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID})
	c.NoError(err, "intent without a model")
	c.False(s.memo.modelKnown(convID), "nothing was set yet")
	c.NoError(s.CompleteTurn(ctx, TurnResult{TurnID: turnID, CreatedAt: at, ConversationID: convID, Model: "served-model", StopReason: "end_turn"}), "complete")
	var got string
	c.NoError(pool.QueryRow(ctx, `SELECT coalesce(model,'') FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&got), "read")
	c.Eq("served-model", got, "the response model fills a conversation whose request named none")
}

// RecordThread reuses the predecessor lookup made at the start of the turn; the
// result must be exactly what a fresh lookup gives, including "did not resolve".
func TestRecordThreadReusingTheStartOfTurnLookupMatchesAFreshOne(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	session := "thr-" + uuid.NewString()
	convID, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session})
	c.NoError(err, "ensure")
	threadOf := func(turnID string) string {
		var got string
		c.NoError(pool.QueryRow(ctx, `SELECT coalesce(thread_id::text,'') FROM conversations.conversation_turn WHERE id=$1::uuid`, turnID).Scan(&got), "thread_id")
		return got
	}
	turn := func(prev, own string, subagent, warm bool) string {
		id, at, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
		c.NoError(err, "intent")
		if warm {
			_, err = s.ThreadOfPredecessorInSession(ctx, session, prev) // fills the memo
			c.NoError(err, "start-of-turn lookup")
		}
		c.NoError(s.RecordThread(ctx, session, convID, id, at, prev, own, subagent), "RecordThread")
		return id
	}

	t1 := turn("", "msg_a", false, false)
	c.Eq("", threadOf(t1), "main thread root")
	// A subagent root, then its chained follow-up: warm (memo) and cold (query) agree.
	sub := turn("", "msg_b", true, false)
	c.Eq(sub, threadOf(sub), "subagent founds its own thread")
	warmChild := turn("msg_b", "msg_c", true, true)
	c.Eq(sub, threadOf(warmChild), "memoised predecessor thread: the subagent's follow-up stays on its thread")
	coldChild := turn("msg_c", "msg_d", true, false)
	c.Eq(sub, threadOf(coldChild), "a fresh lookup agrees")
	// An unresolvable predecessor: the not-found answer is reused too.
	warmMiss := turn("msg_does_not_exist", "msg_e", true, true)
	c.Eq(warmMiss, threadOf(warmMiss), "memoised not-found: a subagent with a broken chain founds a new thread")
	mainMiss := turn("msg_also_missing", "msg_f", false, true)
	c.Eq("", threadOf(mainMiss), "memoised not-found on the main thread stays NULL")

	// The memo entry is consumed: a retry of RecordThread asks the database again.
	c.Eq(0, len(s.memo.threads), "entries are taken, so the table does not grow with the session")
}

func TestEnsureConversationByExternalRefIsRaceSafeAndLooksUpFirst(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	c := assert.NewAborting(t)
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: "race-" + uuid.NewString()}

	const n = 12
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = s.EnsureConversationByExternalRef(ctx, ref)
		}()
	}
	wg.Wait()
	for i := range n {
		c.NoError(errs[i], "caller %d", i)
		c.Eq(ids[0], ids[i], "every concurrent caller gets the one conversation")
	}
	var rows int
	c.NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation WHERE external_ref=$1`, ref.ExternalRef).Scan(&rows), "count")
	c.Eq(1, rows, "exactly one row was created")
}
