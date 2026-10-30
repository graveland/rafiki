// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestCaptureStore requires a real PostgreSQL/TimescaleDB instance. Set
// RAFIKI_TEST_DSN to a connection string to run it, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable" go test ./capture/...
//
// It is skipped by default so plain unit test runs (and CI without a
// TimescaleDB instance) stay green.
func TestCaptureStore(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)

	c.NoError(store.Migrate(ctx, pool), "Migrate")

	cs := NewCaptureStore(pool)

	t.Run("EnsureConversationByExternalRef", func(t *testing.T) {
		testEnsureConversationByExternalRef(t, ctx, cs)
	})
	t.Run("InsertTurnIntent and CompleteTurn", func(t *testing.T) {
		testInsertTurnIntentAndCompleteTurn(t, ctx, pool, cs)
	})
	t.Run("conversation model backfill", func(t *testing.T) {
		testConversationModelBackfill(t, ctx, pool, cs)
	})
	t.Run("CompleteTurn served_provider", func(t *testing.T) {
		testCompleteTurnServedProvider(t, ctx, pool, cs)
	})
}

// newTestStore connects a CaptureStore to the RAFIKI_TEST_DSN database,
// following TestCaptureStore's setup above: skip without the DSN, migrate, and
// hand back both the store and the pool the tests read through.
func newTestStore(t *testing.T) (*CaptureStore, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is; the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
	return NewCaptureStore(pool), pool
}

func testEnsureConversationByExternalRef(t *testing.T, ctx context.Context, cs *CaptureStore) {
	c := assert.NewAborting(t)
	ref1 := "ext-ref-" + time.Now().Format(time.RFC3339Nano)
	id1a, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "client", ExternalRef: ref1,
	})
	c.NoError(err, "EnsureConversationByExternalRef (first)")
	id1b, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "client", ExternalRef: ref1,
	})
	c.NoError(err, "EnsureConversationByExternalRef (repeat)")
	c.Eq(id1b, id1a, "same external_ref must resolve to the same conversation id")

	ref2 := "ext-ref-" + time.Now().Add(time.Second).Format(time.RFC3339Nano)
	id2, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "client", ExternalRef: ref2,
	})
	c.NoError(err, "EnsureConversationByExternalRef (distinct ref)")
	c.NotEq(id1a, id2, "distinct external_ref must yield a distinct conversation id, got")
}

func testInsertTurnIntentAndCompleteTurn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cs *CaptureStore) {
	c := assert.NewCollecting(t)
	convID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "server",
	})
	c.Require().NoError(err, "EnsureConversation")

	turnID, createdAt, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID,
		Ordinal:        1,
		Model:          "claude-test",
	})
	c.Require().NoError(err, "InsertTurnIntent")

	want := TurnResult{
		TurnID:              turnID,
		CreatedAt:           createdAt,
		Model:               "claude-test-served", // response's served model overrides the intent
		Response:            []byte(`{"content":[]}`),
		StopReason:          "end_turn",
		Upstream:            "anthropic",
		InputTokens:         10,
		OutputTokens:        20,
		CacheReadTokens:     30,
		CacheCreationTokens: 40,
		LatencyMS:           123,
	}
	c.Require().NoError(cs.CompleteTurn(ctx, want), "CompleteTurn")

	var (
		gotStopReason          string
		gotUpstream            string
		gotModel               string
		gotInputTokens         int64
		gotOutputTokens        int64
		gotCacheReadTokens     int64
		gotCacheCreationTokens int64
		gotLatencyMS           int
	)
	err = pool.QueryRow(ctx, `
		SELECT stop_reason, upstream, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, latency_ms
		  FROM conversations.conversation_turn
		 WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt).Scan(
		&gotStopReason, &gotUpstream, &gotModel, &gotInputTokens, &gotOutputTokens,
		&gotCacheReadTokens, &gotCacheCreationTokens, &gotLatencyMS,
	)
	c.Require().NoError(err, "read back completed turn")
	c.Eq(want.StopReason, gotStopReason, "stop_reason")
	c.Eq(want.Upstream, gotUpstream, "upstream")
	c.Eq("claude-test-served", gotModel, "model")
	c.Eq(want.InputTokens, gotInputTokens, "input_tokens")
	c.Eq(want.OutputTokens, gotOutputTokens, "output_tokens")
	c.Eq(want.CacheReadTokens, gotCacheReadTokens, "cache_read_tokens")
	c.Eq(want.CacheCreationTokens, gotCacheCreationTokens, "cache_creation_tokens")
	c.Eq(want.LatencyMS, gotLatencyMS, "latency_ms")
}

// testCompleteTurnServedProvider pins the served-provider write: a non-empty
// ServedProvider is stored verbatim, an empty one is stored as SQL NULL (the
// "not reported" sentinel — never an empty string), so export's coalesce and
// any NULL-means-unknown reader both see the same thing.
func testCompleteTurnServedProvider(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cs *CaptureStore) {
	c := assert.NewCollecting(t)
	convID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "server",
	})
	c.Require().NoError(err, "EnsureConversation")
	newTurn := func() (string, time.Time) {
		t.Helper()
		id, createdAt, err := cs.InsertTurnIntent(ctx, TurnIntent{
			ConversationID: convID, Ordinal: 1, Model: "claude-test",
		})
		c.Require().NoError(err, "InsertTurnIntent")
		return id, createdAt
	}
	readServedProvider := func(turnID string, createdAt time.Time) any {
		t.Helper()
		var served any
		c.Require().NoError(pool.QueryRow(ctx, `SELECT served_provider FROM conversations.conversation_turn
			 WHERE id=$1::uuid AND created_at=$2`, turnID, createdAt).Scan(&served), "read back served_provider")
		return served
	}

	turnID, createdAt := newTurn()
	c.Require().NoError(cs.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Model: "claude-test-served",
		Response: []byte(`{"content":[]}`), StopReason: "end_turn",
		Upstream: "openrouter", ServedProvider: "Together",
	}), "CompleteTurn (with provider)")
	if got := readServedProvider(turnID, createdAt); got != "Together" {
		t.Errorf("served_provider = %v, want Together", got)
	}

	turnID, createdAt = newTurn()
	c.Require().NoError(cs.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Model: "claude-test-served",
		Response: []byte(`{"content":[]}`), StopReason: "end_turn", Upstream: "anthropic",
	}), "CompleteTurn (no provider)")
	c.Nil(readServedProvider(turnID, createdAt), "served_provider")
}

// testConversationModelBackfill: a client-driven conversation is created with
// no model (session-header only); the first turn backfills it and a later
// different-model turn does NOT overwrite. A library-style conversation that
// pins its model at creation keeps it.
func testConversationModelBackfill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cs *CaptureStore) {
	c := assert.NewAborting(t)
	ref := "backfill-" + time.Now().Format(time.RFC3339Nano)
	convID, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: ref,
	})
	c.NoError(err)
	model := func() any {
		var m any
		c.NoError(pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&m))
		return m
	}
	c.Nil(model(), "pre-turn model")
	if _, _, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Model: "claude-haiku-4-5",
	}); err != nil {
		t.Fatal(err)
	}
	if m := model(); m != "claude-haiku-4-5" {
		t.Fatalf("model after first turn = %v, want backfilled claude-haiku-4-5", m)
	}
	// A later turn with a DIFFERENT model must not overwrite: first-seen wins.
	if _, _, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Model: "openai/gpt-4o",
	}); err != nil {
		t.Fatal(err)
	}
	m := model()
	c.False(m != "claude-haiku-4-5", "model after different-model turn = %v, want unchanged claude-haiku-4-5", m)

	// Library-style: model pinned at creation survives untouched.
	pinnedID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "server", Model: "claude-sonnet-5",
	})
	c.NoError(err)
	if _, _, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: pinnedID, Model: "claude-opus-4-8",
	}); err != nil {
		t.Fatal(err)
	}
	var pinned string
	c.NoError(pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, pinnedID).Scan(&pinned))
	c.Eq("claude-sonnet-5", pinned, "pinned model")
}

func TestDecomposeRequest_MessagesAndPrefix(t *testing.T) {
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

	req := []byte(`{"model":"claude","system":[{"type":"text","text":"S"}],
		"tools":[{"name":"T"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[{"type":"text","text":"yo"}]}
		]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: routing.PrefixHash(req), Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")

	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req), "")
	c.NoError(err, "DecomposeRequest")

	// two message rows, the second hanging off the first, and the head is the
	// second: content stored verbatim.
	var cnt int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1`, convID).Scan(&cnt), "count messages")
	c.Eq(2, cnt, "message count")
	c.Eq(mustNodeByContent(t, ctx, pool, convID, `[{"type":"text","text":"hi"}]`), mustParent(t, ctx, pool, head), "the head hangs off the request's first message")
	c.Eq(head, mustHead(t, ctx, pool, convID), "head_id is the request head")

	// content stored byte-for-byte (modulo JSON normalization) for message 0
	var gotContent string
	c.NoError(pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=0`,
		convID).Scan(&gotContent), "read message 0 content")
	wantContent := `[{"type":"text","text":"hi"}]`
	var gotNorm, wantNorm any
	c.NoError(json.Unmarshal([]byte(gotContent), &gotNorm), "unmarshal got content")
	c.NoError(json.Unmarshal([]byte(wantContent), &wantNorm), "unmarshal want content")
	c.EqDeep(wantNorm, gotNorm, "message 0 content = %s, want %s (verbatim)", gotContent, wantContent)

	// prefix_content stored on this (first) turn
	var pc *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT prefix_content::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&pc), "read prefix_content")
	c.NotNil(pc, "prefix_content is NULL, want the request envelope stored on the first turn")
	c.StrContains(*pc, `"tools"`, "prefix_content = %q, want it to contain \"tools\"", *pc)
}

func TestDecomposeRequest_PrefixUnchangedIsNull(t *testing.T) {
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

	req := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"}]}`)
	h := routing.PrefixHash(req)
	t1, c1, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, PrefixHash: h})
	c.NoError(err, "InsertTurnIntent (t1)")
	if _, err := s.DecomposeRequest(ctx, convID, t1, c1, req, h, ""); err != nil {
		t.Fatalf("DecomposeRequest (t1): %v", err)
	}

	// second turn, same prefix hash → prefix_content must be NULL
	req2 := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	t2, c2, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, PrefixHash: h})
	c.NoError(err, "InsertTurnIntent (t2)")
	if _, err := s.DecomposeRequest(ctx, convID, t2, c2, req2, h, ""); err != nil {
		t.Fatalf("DecomposeRequest (t2): %v", err)
	}

	var pc *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT prefix_content::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`, t2, c2).Scan(&pc), "read prefix_content")
	if pc != nil {
		t.Fatalf("prefix_content = %q, want NULL (prefix_hash unchanged from previous turn)", *pc)
	}
}

// TestMessageHasCacheControl checks the structure-aware detection directly (no
// DB): only a real cache_control field on a content block counts; plain-string
// content is never a breakpoint even when its text embeds the literal token.
func TestMessageHasCacheControl(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"block with cache_control", `[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]`, true},
		{"block without cache_control", `[{"type":"text","text":"x"}]`, false},
		{"plain string mentioning the token", `"please explain \"cache_control\" to me"`, false},
		{"plain string", `"hello"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := messageHasCacheControl(json.RawMessage(c.content))
			assert.NewCollecting(t).Eq(c.want, got, "messageHasCacheControl(%s) = %v, want", c.content, got)
		})
	}
}

// TestJSONBSafe checks the U+0000 sanitizer directly (no DB): clean content is
// returned verbatim, NUL-bearing content is stripped and stays valid JSON with
// integers preserved, and invalid JSON is passed through unchanged. It pins
// conv.StorableJSON, the one sanitizer capture and the tree both use.
func TestJSONBSafe(t *testing.T) {
	c := assert.NewCollecting(t)
	clean := `{"type":"text","text":"hello"}`
	c.Eq(clean, string(conv.StorableJSON([]byte(clean))), "clean content changed")

	withNUL := `[{"type":"text","text":"a\u0000b","n":123456789012345678}]`
	out := string(conv.StorableJSON([]byte(withNUL)))
	c.False(strings.Contains(out, `\u0000`) || strings.IndexByte(out, 0) >= 0, "output still carries NUL: %s", out)
	c.StrContains(out, "123456789012345678", "large integer not preserved (UseNumber)")
	var arr []map[string]any
	c.Require().NoError(json.Unmarshal([]byte(out), &arr), "output not valid json")
	if arr[0]["text"] != "ab" {
		t.Errorf("text = %v, want \"ab\" (NUL stripped)", arr[0]["text"])
	}

	bad := `not json \u0000`
	c.Eq(bad, string(conv.StorableJSON([]byte(bad))), "invalid json changed")
}

// TestDecomposeRequest_NullEscapeInContent verifies a message whose content
// carries a \u0000 escape (which a raw jsonb insert rejects with SQLSTATE 22P05)
// is captured with the NUL stripped rather than failing the decompose.
func TestDecomposeRequest_NullEscapeInContent(t *testing.T) {
	c := assert.NewCollecting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.Require().NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.Require().NoError(store.Migrate(ctx, pool), "Migrate")
	s := NewCaptureStore(pool)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.Require().NoError(err, "EnsureConversation")

	req := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"before\u0000after"}]}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, PrefixHash: routing.PrefixHash(req)})
	c.Require().NoError(err, "InsertTurnIntent with a \\u0000 in request")
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req), ""); err != nil {
		t.Fatalf("DecomposeRequest with a \\u0000 in content should succeed, got: %v", err)
	}

	var content string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=0`, convID).Scan(&content), "read content")
	c.Less(0, strings.IndexByte(content, 0), "stored content still contains a NUL byte: %q", content)
	c.StrContains(content, "beforeafter", "content = %q, want the NUL stripped to \"beforeafter\"", content)
}

// TestDecomposeRequest_PrefixChangeReStores verifies on-change prefix detection
// re-fires after an unchanged run: three turns hashed h1, h1, h2 must store
// prefix_content on turns 1 and 3 (NULL on 2).
func TestDecomposeRequest_PrefixChangeReStores(t *testing.T) {
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

	reqA := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"}]}`)
	reqB := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	reqC := []byte(`{"model":"claude","tools":[{"name":"T"},{"name":"U"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"},{"role":"assistant","content":"d"},{"role":"user","content":"e"}]}`)
	hA, hB, hC := routing.PrefixHash(reqA), routing.PrefixHash(reqB), routing.PrefixHash(reqC)
	c.Eq(hB, hA, "precondition: reqA and reqB must share a prefix hash (same envelope), got")
	c.NotEq(hA, hC, "precondition: reqC must differ (new tool), got same hash")

	decompose := func(req []byte, h string) (string, time.Time) {
		id, ca, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, PrefixHash: h})
		c.NoError(err, "InsertTurnIntent")
		if _, err := s.DecomposeRequest(ctx, convID, id, ca, req, h, ""); err != nil {
			t.Fatalf("DecomposeRequest: %v", err)
		}
		return id, ca
	}
	prefixOf := func(id string, ca time.Time) *string {
		var pc *string
		c.NoError(pool.QueryRow(ctx,
			`SELECT prefix_content::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`, id, ca).Scan(&pc), "read prefix_content")
		return pc
	}

	id1, c1 := decompose(reqA, hA)
	id2, c2 := decompose(reqB, hB)
	id3, c3 := decompose(reqC, hC)

	c.NotNil(prefixOf(id1, c1), "turn 1 prefix_content is NULL, want stored (first turn)")
	c.Nil(prefixOf(id2, c2), "turn 2 prefix_content stored, want NULL (unchanged from turn 1)")
	pc3 := prefixOf(id3, c3)
	c.NotNil(pc3, "turn 3 prefix_content is NULL, want re-stored (envelope changed after an unchanged run)")
	c.StrContains(*pc3, `"U"`, "turn 3 prefix_content = %q, want the new tool \"U\" in the envelope", *pc3)
}

// TestDecomposeRequest_CrossTurnIdempotent verifies each resubmitted message
// persists exactly once at a contiguous ordinal (ON CONFLICT DO NOTHING,
// first-writer-wins) across turns of one conversation.
func TestDecomposeRequest_CrossTurnIdempotent(t *testing.T) {
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

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"a"}]}`)
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	for _, req := range [][]byte{req1, req2} {
		h := routing.PrefixHash(req)
		id, ca, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, PrefixHash: h})
		c.NoError(err, "InsertTurnIntent")
		if _, err := s.DecomposeRequest(ctx, convID, id, ca, req, h, ""); err != nil {
			t.Fatalf("DecomposeRequest: %v", err)
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT ordinal, role FROM conversations.conversation_message WHERE conversation_id=$1 ORDER BY ordinal`, convID)
	c.NoError(err, "query messages")
	defer rows.Close()
	type msg struct {
		ord  int
		role string
	}
	var got []msg
	for rows.Next() {
		var m msg
		c.NoError(rows.Scan(&m.ord, &m.role), "scan")
		got = append(got, m)
	}
	c.NoError(rows.Err(), "rows")
	want := []msg{{0, "user"}, {1, "assistant"}, {2, "user"}}
	c.EqDeep(want, got, "conversation_message")
}

// captureTestEnv is the shared preamble for the capture-path tests: the
// RAFIKI_TEST_DSN skip, connect, migrate, store. The pool is returned for
// direct assertion queries.
func captureTestEnv(t *testing.T) (context.Context, *pgxpool.Pool, *CaptureStore) {
	t.Helper()
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
	return ctx, pool, NewCaptureStore(pool)
}

// runTurn mirrors the proxy's per-turn sequence for one request that names no
// predecessor: InsertTurnIntent, DecomposeRequest, then CompleteTurn —
// completion BEFORE the next turn's decompose is what the compaction root's
// prior-turn input_tokens lookup reads. Returns the request head's node id.
func runTurn(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte, inTok, outTok int64) string {
	t.Helper()
	return runTurnFrom(t, ctx, s, convID, req, "", inTok, outTok)
}

// runTurnFrom is runTurn with the request's diagnostics.previous_message_id,
// which is what a replaying client (Claude Code) sends.
func runTurnFrom(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte, prevMessageID string, inTok, outTok int64) string {
	t.Helper()
	c := assert.NewAborting(t)
	h := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: h, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, h, prevMessageID)
	c.NoError(err, "DecomposeRequest")
	c.NoError(s.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Response: []byte(`{"content":[]}`),
		StopReason: "end_turn", Upstream: "anthropic",
		InputTokens: inTok, OutputTokens: outTok,
	}), "CompleteTurn")
	return head
}

// runFullTurn is runTurnFrom plus the response the request produced, appended
// under the request head: the proxy's whole sequence on ONE turn row, so the
// next turn's prior-usage lookup sees this turn's usage. Returns the request
// head.
func runFullTurn(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte, prevMessageID string, reply []byte, inTok, outTok int64) string {
	t.Helper()
	c := assert.NewAborting(t)
	h := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: h, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, h, prevMessageID)
	c.NoError(err, "DecomposeRequest")
	c.NoError(s.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Response: []byte(`{"content":[]}`),
		StopReason: "end_turn", Upstream: "anthropic",
		InputTokens: inTok, OutputTokens: outTok,
	}), "CompleteTurn")
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, head, reply, inTok, outTok, "end_turn"), "AppendResponseMessage")
	return head
}

// requireKindRow reads one message row's kind/role/content/input_tokens.
func requireKindRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal int) (kind, role *string, inTok *int64, content string) {
	t.Helper()
	assert.NewAborting(t).NoError(pool.QueryRow(ctx,
		`SELECT kind, role, input_tokens, content::text FROM conversations.conversation_message
		  WHERE conversation_id=$1 AND ordinal=$2`, convID, ordinal).
		Scan(&kind, &role, &inTok, &content), "read row ordinal %d", ordinal)
	return kind, role, inTok, content
}

func requireJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	c := assert.NewAborting(t)
	var g, w any
	c.NoError(json.Unmarshal([]byte(got), &g), "unmarshal got %q", got)
	c.NoError(json.Unmarshal([]byte(want), &w), "unmarshal want %q", want)
	c.EqDeep(w, g, "content = %s, want %s (verbatim modulo JSON normalization)", got, want)
}

// summaryContent renders message-0 content that reads as a genuine Claude Code
// compaction summary: one text block opening with the phrases a real summary
// carries. A structurally divergent message without that prose must not be
// treated as one.
func summaryContent(analysis string) string {
	return `[{"type":"text","text":"This session is being continued from a previous conversation that ran out of context. The conversation is summarized below: Analysis: ` + analysis + `"}]`
}

// TestACompactionSummaryAfterAKnownReplyIsARoot: a request that names the reply
// it continues but whose message 0 is a summary no longer replays that reply as
// its first message (the summary replaced the history), so it opens a
// compaction root derived from the named node, carrying the prior turn's
// input_tokens as the size of the context it replaced.
func TestACompactionSummaryAfterAKnownReplyIsARoot(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	runFullTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`), "",
		[]byte(`{"id":"msg_a","content":[{"type":"text","text":"yo"}]}`), 1400, 60)
	replyID := mustProviderNode(t, ctx, pool, convID, "msg_a")

	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("the user asked for a refactor") + `},{"role":"user","content":"next question"}]}`)
	head := runTurnFrom(t, ctx, s, convID, req2, "msg_a", 90, 8)

	// The summary is a root: no parent, kind compaction_summary, derived from
	// the reply it names, and as large as the context it replaced.
	summary := mustParent(t, ctx, pool, head)
	c.Eq("", mustParent(t, ctx, pool, summary), "the summary opened a root of its own")
	kind, derived, depth := mustNodeKind(t, ctx, pool, summary)
	c.Eq("compaction_summary", kind, "kind")
	c.Eq(replyID, derived, "derived_from_id is the reply the request named")
	c.Eq(0, depth, "root depth")

	var inTok *int64
	var content string
	c.NoError(pool.QueryRow(ctx,
		`SELECT input_tokens, content::text FROM conversations.conversation_message WHERE id=$1::uuid`, summary).Scan(&inTok, &content), "read the summary")
	c.False(inTok == nil || *inTok != 1400, "input_tokens = %v, want 1400 (the replaced context's size)", inTok)
	requireJSONEqual(t, content, summaryContent("the user asked for a refactor"))

	// The pre-compaction history is untouched: "hi" is still the plain root.
	var rootRole, rootContent string
	c.NoError(pool.QueryRow(ctx, `
		SELECT role, content::text FROM conversations.conversation_message
		 WHERE conversation_id=$1::uuid AND parent_id IS NULL AND kind IS NULL`, convID).Scan(&rootRole, &rootContent), "read the plain root")
	c.Eq("user", rootRole, "plain root role")
	requireJSONEqual(t, rootContent, `"hi"`)
}

// TestReplayingACompactionSummaryAddsNoRows: the next request carrying the SAME
// summary as message 0 hashes to the root that already exists, so it inserts
// nothing and only moves the head; the root's kind and input_tokens are
// first-seen and untouched.
func TestReplayingACompactionSummaryAddsNoRows(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	runFullTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`), "",
		[]byte(`{"id":"msg_b","content":[{"type":"text","text":"yo"}]}`), 1234, 10)

	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("the user asked for a refactor") + `},{"role":"user","content":"next question"}]}`)
	runTurnFrom(t, ctx, s, convID, req2, "msg_b", 90, 8)
	before := mustNodeCount(t, ctx, pool, convID)

	// The same summary again: it hashes to the root and its child, so nothing is
	// inserted and no second boundary appears.
	runTurnFrom(t, ctx, s, convID, req2, "msg_b", 91, 9)
	c.Eq(before, mustNodeCount(t, ctx, pool, convID), "node count after replaying the same summary")

	var summaries int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='compaction_summary'`, convID).Scan(&summaries), "count summary roots")
	c.Eq(1, summaries, "compaction_summary roots")

	var inTok *int64
	c.NoError(pool.QueryRow(ctx,
		`SELECT input_tokens FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='compaction_summary'`, convID).Scan(&inTok), "read the summary's input_tokens")
	c.False(inTok == nil || *inTok != 1234, "summary input_tokens = %v, want 1234 (first-seen, untouched by the replay)", inTok)
}

// TestTwoDifferentSummariesAreTwoRoots: a second, different compaction summary
// is a different root (a summary is unique model output, so equal content is
// the same history and different content is not), and the first root's row is
// untouched.
func TestTwoDifferentSummariesAreTwoRoots(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	runFullTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`), "",
		[]byte(`{"id":"msg_c","content":[{"type":"text","text":"yo"}]}`), 1234, 10)

	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-A details") + `},{"role":"user","content":"q1"}]}`)
	runTurnFrom(t, ctx, s, convID, req2, "msg_c", 90, 8)
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-B details") + `},{"role":"user","content":"q2"}]}`)
	runTurnFrom(t, ctx, s, convID, req3, "msg_c", 60, 6)

	var summaries int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND kind='compaction_summary'`, convID).Scan(&summaries), "count summary roots")
	c.Eq(2, summaries, "compaction_summary roots")

	// The first summary's content and size survive: nothing is deleted or
	// rewritten when a second root is cut.
	var content string
	var inTok *int64
	c.NoError(pool.QueryRow(ctx, `
		SELECT content::text, input_tokens FROM conversations.conversation_message
		 WHERE conversation_id=$1::uuid AND kind='compaction_summary' ORDER BY ordinal LIMIT 1`, convID).Scan(&content, &inTok), "read the first summary")
	requireJSONEqual(t, content, summaryContent("SUMMARY-A details"))
	c.False(inTok == nil || *inTok != 1234, "first summary input_tokens = %v, want 1234", inTok)
}

// TestARewindAttachesAtTheNamedNode: a client that resumes from an older reply
// names it and replays the history up to it, so the request's later messages
// hang off that node — the messages already stored there are the same nodes, so
// only the new tail is inserted and the abandoned branch stays where it was.
func TestARewindAttachesAtTheNamedNode(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	runFullTurn(t, ctx, s, convID, []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`), "",
		[]byte(`{"id":"msg_r1","content":[{"type":"text","text":"r1"}]}`), 100, 10)
	firstReply := mustProviderNode(t, ctx, pool, convID, "msg_r1")

	// The branch that will be abandoned: "q1" and its reply.
	runFullTurn(t, ctx, s, convID,
		[]byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"r1"}]},{"role":"user","content":"q1"}]}`),
		"msg_r1", []byte(`{"id":"msg_r2","content":[{"type":"text","text":"r2"}]}`), 110, 11)
	before := mustNodeCount(t, ctx, pool, convID)

	// The rewind: the client resumes from the FIRST reply and asks something
	// else, replaying that reply at the index the tree holds it at.
	rewound := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"r1"}]},{"role":"user","content":"another"}]}`)
	head := runTurnFrom(t, ctx, s, convID, rewound, "msg_r1", 130, 13)

	c.Eq(before+1, mustNodeCount(t, ctx, pool, convID), "the rewind inserts only its new message")
	c.Eq(firstReply, mustParent(t, ctx, pool, head), "the new message hangs off the reply the request named")

	// The abandoned branch is untouched: "q1" and its reply are still there.
	var q1 int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid AND content='"q1"'::jsonb`, convID).Scan(&q1), "count q1")
	c.Eq(1, q1, "the abandoned branch's q1 row")
}

// TestTheFirstRequestIsAPlainRoot: a brand-new conversation's first request has
// nothing to attach to, so its first message is a plain root at depth 0 and its
// rest is a chain under it. No boundary is recorded.
func TestTheFirstRequestIsAPlainRoot(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req := []byte(`{"model":"claude","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]}`)
	head := runTurn(t, ctx, s, convID, req, 50, 5)

	var kindCount int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind IS NOT NULL`, convID).Scan(&kindCount), "count kind rows")
	c.Eq(0, kindCount, "kind-tagged rows")

	rows, err := pool.Query(ctx, `
		SELECT role, coalesce(depth, -1), parent_id IS NULL FROM conversations.conversation_message
		 WHERE conversation_id=$1::uuid ORDER BY ordinal`, convID)
	c.NoError(err, "read rows")
	defer rows.Close()
	var roles []string
	var depths []int
	var roots []bool
	for rows.Next() {
		var role string
		var depth int
		var isRoot bool
		c.NoError(rows.Scan(&role, &depth, &isRoot), "scan row")
		roles = append(roles, role)
		depths = append(depths, depth)
		roots = append(roots, isRoot)
	}
	c.NoError(rows.Err(), "rows")
	c.EqDeep([]string{"user", "assistant", "user"}, roles, "roles")
	c.EqDeep([]int{0, 1, 2}, depths, "depths")
	c.EqDeep([]bool{true, false, false}, roots, "roots")

	var headDepth int
	c.NoError(pool.QueryRow(ctx,
		`SELECT coalesce(depth,-1) FROM conversations.conversation_message WHERE id=$1::uuid`, head).Scan(&headDepth), "read head depth")
	c.Eq(2, headDepth, "the head is the request's last node")
}

// TestDecomposeRequest_CacheBreakpoints verifies breakpoint detection is
// structure-aware: only messages with an actual cache_control field on a
// content block are recorded, not messages whose text merely contains the
// literal substring "cache_control".
func TestDecomposeRequest_CacheBreakpoints(t *testing.T) {
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

	req := []byte(`{"model":"claude","tools":[{"name":"T"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"plain, no marker"}]},
			{"role":"assistant","content":[{"type":"text","text":"cached block","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"this text literally says cache_control but has no such field"}]}
		]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: routing.PrefixHash(req)})
	c.NoError(err, "InsertTurnIntent")
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req), ""); err != nil {
		t.Fatalf("DecomposeRequest: %v", err)
	}

	var bp *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT cache_breakpoints::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&bp), "read cache_breakpoints")
	c.NotNil(bp, "cache_breakpoints is NULL, want [1]")
	var got []int
	c.NoError(json.Unmarshal([]byte(*bp), &got), "unmarshal cache_breakpoints %q", *bp)
	want := []int{1}
	c.EqDiff(want, got, "cache_breakpoints")
}

// TestAppendResponseMessage verifies the canonical assistant response is
// appended as a child of the caller-supplied request head (verbatim content,
// token usage, stop_reason, the response's provider id) and that the turn's
// response_ordinal is set to that node's ordinal.
func TestAppendResponseMessage(t *testing.T) {
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

	req := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: routing.PrefixHash(req)})
	c.NoError(err, "InsertTurnIntent")
	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req), "")
	c.NoError(err, "DecomposeRequest")

	canonical := []byte(`{"id":"msg_hello","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, head, canonical, 10, 5, "end_turn"), "AppendResponseMessage")

	replyNode := mustProviderNode(t, ctx, pool, convID, "msg_hello")
	c.Eq(head, mustParent(t, ctx, pool, replyNode), "the reply hangs off the request head")

	var role, stop string
	var out int64
	c.NoError(pool.QueryRow(ctx,
		`SELECT role, coalesce(stop_reason,''), coalesce(output_tokens,0) FROM conversations.conversation_message
		  WHERE id=$1::uuid`, replyNode).Scan(&role, &stop, &out), "read conversation_message")
	c.Eq("assistant", role, "role")
	c.Eq("end_turn", stop, "stop_reason")
	c.Eq(5, out, "output_tokens")

	var gotContent string
	c.NoError(pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE id=$1::uuid`,
		replyNode).Scan(&gotContent), "read content")
	wantContent := `[{"type":"text","text":"hello"}]`
	var gotNorm, wantNorm any
	c.NoError(json.Unmarshal([]byte(gotContent), &gotNorm), "unmarshal got content")
	c.NoError(json.Unmarshal([]byte(wantContent), &wantNorm), "unmarshal want content")
	c.EqDeep(wantNorm, gotNorm, "content = %s, want %s (verbatim canonical.content)", gotContent, wantContent)

	var respOrd int
	c.NoError(pool.QueryRow(ctx,
		`SELECT response_ordinal FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&respOrd), "read response_ordinal")
	var replyOrdinal int
	c.NoError(pool.QueryRow(ctx,
		`SELECT ordinal FROM conversations.conversation_message WHERE id=$1::uuid`, replyNode).Scan(&replyOrdinal), "read the reply's ordinal")
	c.Eq(replyOrdinal, respOrd, "response_ordinal is the reply node's ordinal")
}

// TestAppendResponseMessageFailsWhenTheTurnKeyMoved drives the zero-rows branch
// of AppendResponseMessage's response_ordinal UPDATE. The turn's key is
// (id, created_at) — created_at is the hypertable's partition column — so moving
// created_at leaves the UPDATE matching nothing. That is the state a lost turn
// row puts the reply in: the node is already committed by AppendChain, and
// without the pointer nothing on the turn names it. The call must fail the turn
// rather than return success over a half-recorded response.
func TestAppendResponseMessageFailsWhenTheTurnKeyMoved(t *testing.T) {
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

	req := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, PrefixHash: routing.PrefixHash(req)})
	c.NoError(err, "InsertTurnIntent")
	head, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req), "")
	c.NoError(err, "DecomposeRequest")

	// Move the turn's key out from under the caller's createdAt, which is what a
	// row that was rewritten or re-partitioned underneath the append looks like.
	// The move is deliberately sub-second: created_at is the hypertable's
	// partition column and each chunk carries a check constraint bounding it, so
	// a whole-hour move crosses the chunk boundary and Postgres refuses the
	// UPDATE. Any difference in the key produces the zero-row match this test
	// needs.
	_, err = pool.Exec(ctx, `
		UPDATE conversations.conversation_turn SET created_at = created_at + interval '1 millisecond'
		 WHERE id = $1 AND created_at = $2`, turnID, createdAt)
	c.Require().NoError(err, "move the turn's created_at key")

	canonical := []byte(`{"id":"msg_hello","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	err = s.AppendResponseMessage(ctx, convID, turnID, createdAt, head, canonical, 10, 5, "end_turn")
	c.Require().Error(err, "an UPDATE that matches no turn row must fail the response append")
	c.ErrorContains(err, "expected 1 row", "the failure is the zero-rows branch, not an earlier step")

	// The pointer is what the branch protects: it must still be NULL, because
	// nothing on the turn names the reply the append stored.
	var respOrd *int
	c.NoError(pool.QueryRow(ctx,
		`SELECT response_ordinal FROM conversations.conversation_turn WHERE id = $1::uuid`, turnID).Scan(&respOrd),
		"read response_ordinal")
	c.Nil(respOrd, "response_ordinal stays unset when the turn row cannot be found")
}

func TestIsRetryableDB(t *testing.T) {
	liveCtx := context.Background()
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name  string
		err   error
		ctx   context.Context
		retry bool
	}{
		{"nil", nil, liveCtx, false},
		{"cancelled context", context.DeadlineExceeded, cancelledCtx, false},
		{"deadline exceeded", context.DeadlineExceeded, liveCtx, true},
		{"net.OpError", &net.OpError{Op: "read", Err: errors.New("connection refused")}, liveCtx, true},
		{"plain error", errors.New("something failed"), liveCtx, false},
		{"a lost lease as the store reports it", fmt.Errorf("append chain: %w", store.ErrLeaseLost), liveCtx, false},
		{"pgconn deadlock", &pgconn.PgError{Code: "40P01"}, liveCtx, true},
		{"pgconn serialization", &pgconn.PgError{Code: "40001"}, liveCtx, true},
		{"pgconn not-null violation", &pgconn.PgError{Code: "23502"}, liveCtx, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryableDB(tt.err, tt.ctx)
			assert.NewCollecting(t).Eq(tt.retry, got, "isRetryableDB(%v) = %v, want", tt.err, got)
		})
	}
}

func TestRetryDB_NonTransientNoRetry(t *testing.T) {
	c := assert.NewAborting(t)
	calls := 0
	err := retryDB(context.Background(), "test", func(ctx context.Context) error {
		calls++
		return errors.New("permanent")
	})
	c.Error(err, "expected error")
	c.Eq(1, calls, "expected 1 call, got")
}

func TestRetryDB_TransientRecovers(t *testing.T) {
	c := assert.NewAborting(t)
	withShortDBRetryDelays(t)
	calls := 0
	err := retryDB(context.Background(), "test", func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return &net.OpError{Op: "read", Err: errors.New("connection reset")}
		}
		return nil
	})
	c.NoError(err, "expected recovery, got")
	c.Eq(2, calls, "expected 2 calls, got")
}

func TestRetryDB_ExhaustedRetries(t *testing.T) {
	c := assert.NewAborting(t)
	withShortDBRetryDelays(t)
	calls := 0
	err := retryDB(context.Background(), "test", func(ctx context.Context) error {
		calls++
		return context.DeadlineExceeded
	})
	c.Error(err, "expected error after exhausted retries")
	c.Eq(4, calls, "expected 4 calls, got") // initial + 3 retries
}

func TestRetryDB_ParentContextCanceled(t *testing.T) {
	c := assert.NewAborting(t)
	withShortDBRetryDelays(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryDB(ctx, "test", func(ctx context.Context) error {
		calls++
		return context.DeadlineExceeded
	})
	c.Error(err, "expected error from cancelled parent")
	// With a cancelled parent, the select at the end of the retry loop
	// immediately returns ctx.Err(). Should not retry.
	c.LessOrEqual(1, calls, "expected at most 1 call with cancelled parent, got")
}

// withShortDBRetryDelays shrinks dbRetryDelays to microseconds for the
// duration of the test so a test exercising the full retry sequence doesn't
// sleep through the real 1s/3s/5s backoff.
func withShortDBRetryDelays(t *testing.T) {
	t.Helper()
	orig := dbRetryDelays
	dbRetryDelays = []time.Duration{time.Microsecond, time.Microsecond, time.Microsecond}
	t.Cleanup(func() { dbRetryDelays = orig })
}

// TestConversationTokensGroupsByModel proves the rollup keeps models apart. A
// conversation that failed over mid-flight has turns priced at two different
// rates, and a flat SUM would bill all of them at whichever model the caller
// happened to price with.
func TestConversationTokensGroupsByModel(t *testing.T) {
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "Migrate")
	cs := NewCaptureStore(pool)

	convID, err := cs.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "test", DrivenBy: "server"})
	c.NoError(err, "EnsureConversation")

	// Two turns on one model, one on another, plus an errored turn that must
	// NOT be counted.
	for _, tc := range []struct {
		model           string
		in, out, cr, cw int64
		fail            bool
	}{
		{model: "claude-sonnet-5", in: 100, out: 10, cr: 1000, cw: 5},
		{model: "claude-sonnet-5", in: 200, out: 20, cr: 2000, cw: 5},
		{model: "deepseek/deepseek-v4-pro", in: 300, out: 30, cr: 3000, cw: 0},
		{model: "claude-sonnet-5", in: 999, out: 999, cr: 999, cw: 999, fail: true},
	} {
		turnID, createdAt, err := cs.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: tc.model})
		c.NoError(err, "InsertTurnIntent")
		if tc.fail {
			c.NoError(cs.FailTurn(ctx, turnID, createdAt, "deliberate"), "FailTurn")
			continue
		}
		c.NoError(cs.CompleteTurn(ctx, TurnResult{
			TurnID: turnID, CreatedAt: createdAt, Model: tc.model, Upstream: "test",
			InputTokens: tc.in, OutputTokens: tc.out, CacheReadTokens: tc.cr, CacheCreationTokens: tc.cw,
		}), "CompleteTurn")
	}

	got, err := cs.ConversationTokens(ctx, convID)
	c.NoError(err, "ConversationTokens")
	byModel := map[string]ModelTokens{}
	for _, m := range got {
		byModel[m.Model] = m
	}
	c.Len(byModel, 2, "got %d model groups (%v), want 2 — the errored turn must be excluded", len(byModel), byModel)
	if s := byModel["claude-sonnet-5"]; s.InputTokens != 300 || s.OutputTokens != 30 || s.CacheReadTokens != 3000 || s.CacheCreationTokens != 10 {
		t.Errorf("claude-sonnet-5 = %+v, want in=300 out=30 cr=3000 cw=10", s)
	}
	if s := byModel["deepseek/deepseek-v4-pro"]; s.InputTokens != 300 || s.CacheReadTokens != 3000 {
		t.Errorf("deepseek = %+v, want in=300 cr=3000", s)
	}
}

func TestRecordThreadLinksConsecutiveTurns(t *testing.T) {
	c := assert.NewCollecting(t)
	s, pool := newTestStore(t) // follow the file's existing helper
	ctx := context.Background()
	convID, err := s.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client",
	})
	c.Require().NoError(err, "EnsureConversation")
	// No session header, so the predecessor lookup scopes to the turn's own
	// conversation: the pre-session shape of the same rule.
	const session = ""

	threadOf := func(turnID string) string {
		var got string
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
			turnID).Scan(&got), "read thread_id")
		return got
	}

	// Turn 1: the main thread's first turn. thread_id NULL is the convention
	// that keeps the session's root conversation on the bare external_ref.
	t1, t1At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent")
	c.Require().NoError(s.RecordThread(ctx, session, convID, t1, t1At, "", "msg_one", false), "RecordThread turn 1")

	// Turn 2: chains to turn 1. The predecessor IS the main thread (its
	// thread_id is NULL), so turn 2 keeps NULL too.
	t2, t2At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent")
	c.Require().NoError(s.RecordThread(ctx, session, convID, t2, t2At, "msg_one", "msg_two", false), "RecordThread turn 2")

	// Turn 3: a SECOND root, as a concurrent subagent's first turn is. The
	// billing header marks it cc_is_subagent, so it founds its own thread.
	t3, t3At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent")
	c.Require().NoError(s.RecordThread(ctx, session, convID, t3, t3At, "", "msg_three", true), "RecordThread turn 3")

	// Turn 4: the subagent's second turn, chaining to its own first.
	t4, t4At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent")
	c.Require().NoError(s.RecordThread(ctx, session, convID, t4, t4At, "msg_three", "msg_four", true), "RecordThread turn 4")

	c.Eq("", threadOf(t1), "turn 1 (main thread root) thread_id")
	c.Eq("", threadOf(t2), "turn 2 (main thread, chained) thread_id")
	c.NotEq("", threadOf(t3), "turn 3 (subagent first turn) thread_id is NULL, want its own id: a subagent with no predecessor founds a new thread")
	c.Eq(t3, threadOf(t3), "turn 3 thread")
	c.Eq(t3, threadOf(t4), "turn 4 thread")
}

func TestRequestMessageReplayStaysLenient(t *testing.T) {
	// The other half of the rule: re-decomposing the same request is normal and
	// must stay silent, or every turn of a conversation errors.
	s, _ := newTestStore(t)
	ctx := context.Background()
	convID, _ := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	decompose := func() error {
		id, at, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
		if err != nil {
			return err
		}
		_, err = s.DecomposeRequest(ctx, convID, id, at, body, "h", "")
		return err
	}
	if err := decompose(); err != nil {
		t.Fatalf("first decompose: %v", err)
	}
	assert.NewAborting(t).NoError(decompose(), "replay decompose must stay lenient, got")
}

func TestRecordThreadUnresolvablePredecessorStaysMainThreadUnlessSubagent(t *testing.T) {
	c := assert.NewCollecting(t)
	// A predecessor from before this column existed, or from a conversation the
	// proxy did not capture, must not silently attach the turn to an unrelated
	// thread. For the main thread that means staying the main thread (thread_id
	// NULL, on the conversation the routing already chose); for a subagent it
	// means founding a new thread with its own id.
	s, pool := newTestStore(t)
	ctx := context.Background()
	convID, _ := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})

	id, at, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent main")
	c.Require().NoError(s.RecordThread(ctx, "", convID, id, at, "msg_never_seen", "msg_mine", false), "RecordThread main")

	subID, subAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent subagent")
	c.Require().NoError(s.RecordThread(ctx, "", convID, subID, subAt, "msg_also_never_seen", "msg_sub_mine", true), "RecordThread subagent")

	threadOf := func(turnID string) string {
		var got string
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
			turnID).Scan(&got), "read thread_id")
		return got
	}
	c.Eq("", threadOf(id), "main thread with an unresolvable predecessor: thread_id")
	c.Eq(subID, threadOf(subID), "subagent with an unresolvable predecessor: thread_id")
}

// TestConcurrentThreadsDoNotShareATree: two threads of one session are two
// conversations, so each has its own tree and neither can land a message where
// the other's history sits.
func TestConcurrentThreadsDoNotShareATree(t *testing.T) {
	ck := assert.NewCollecting(t)
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name() + "_" + time.Now().Format("150405.000000000")

	root, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "")
	ck.Require().NoError(err, "ResolveThreadConversation root")
	branch, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "thread-b")
	ck.Require().NoError(err, "ResolveThreadConversation branch")
	ck.Require().NotEq(branch, root, "a non-root thread must get its own conversation row")

	// Both threads open the same-shaped history. In one conversation the second
	// would be a sibling of the first; in separate trees each gets its own root.
	body := []byte(`{"messages":[{"role":"user","content":"same shape"}]}`)
	for _, conv := range []string{root, branch} {
		turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: conv, Model: "m"})
		ck.Require().NoError(err, "InsertTurnIntent")
		if _, err := s.DecomposeRequest(ctx, conv, turnID, createdAt, body, "h", ""); err != nil {
			t.Fatalf("decompose %s: %v", conv, err)
		}
	}
	for _, c := range []struct{ name, id string }{{"root", root}, {"branch", branch}} {
		var n int
		ck.Require().NoError(pool.QueryRow(ctx,
			`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid`,
			c.id).Scan(&n), "count %s", c.name)
		ck.Eq(1, n, "%s conversation has %d messages, want 1", c.name, n)
	}

	// The branch must be discoverable from the session for cost rollup.
	var ref string
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, branch).Scan(&ref), "read external_ref")
	ck.Eq(session+":thread-b", ref, "branch external_ref")
}

func TestRootThreadKeepsTheBareSessionExternalRef(t *testing.T) {
	c := assert.NewCollecting(t)
	// Every existing conversation, every cost rollup keyed on external_ref and
	// every `rafiki logs <child>` depends on this staying unchanged.
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()
	id, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "")
	c.Require().NoError(err, "ResolveThreadConversation")
	var ref string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, id).Scan(&ref), "read external_ref")
	c.Eq(session, ref, "root external_ref")
}

// TestChainedMainThreadRequestsStayOnTheBareSessionRow pins the routing rule
// end to end against the real database: the main thread's second and later
// requests DO carry a resolvable predecessor, and that predecessor's thread_id
// is NULL, so every one of them keeps landing on the bare-ref conversation and
// the bare ref is never suffixed. Before the NULL-thread_id convention, the
// resolver forked a fresh conversation per request and cost rollups keyed on
// external_ref counted only the first one.
func TestChainedMainThreadRequestsStayOnTheBareSessionRow(t *testing.T) {
	c := assert.NewCollecting(t)
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	root, err := s.ResolveThreadConversation(ctx, ref, "")
	c.Require().NoError(err, "ResolveThreadConversation root")

	// Request 1: no predecessor (the main thread's first turn).
	t1, t1At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent 1")
	c.Require().NoError(s.RecordThread(ctx, session, root, t1, t1At, "", "msg_"+t1, false), "RecordThread 1")

	// Request 2: chains to request 1's response, exactly as Claude Code does.
	tid1, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t1)
	c.Require().NoError(err, "ThreadOfPredecessorInSession 1")
	c.Require().Eq("", tid1, "turn 1 is the main thread: lookup")
	conv2, err := s.ResolveThreadConversation(ctx, ref, tid1)
	c.Require().NoError(err, "ResolveThreadConversation 2")
	c.Require().Eq(root, conv2, "request 2 landed on")
	t2, t2At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: conv2, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent 2")
	c.Require().NoError(s.RecordThread(ctx, session, conv2, t2, t2At, "msg_"+t1, "msg_"+t2, false), "RecordThread 2")

	// Request 3: same again, proving the chain stays main-thread past turn 2.
	tid2, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t2)
	c.Require().NoError(err, "ThreadOfPredecessorInSession 2")
	c.Require().Eq("", tid2, "turn 2 chains to the main thread: lookup")
	conv3, err := s.ResolveThreadConversation(ctx, ref, tid2)
	c.Require().NoError(err, "ResolveThreadConversation 3")
	c.Require().Eq(root, conv3, "request 3 landed on")

	for i, turnID := range []string{t1, t2} {
		var got string
		c.Require().NoError(pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
			turnID).Scan(&got), "read thread_id %d", i+1)
		c.Eq("", got, "main-thread turn %d thread_id = %s, want NULL", i+1, got)
	}

	// The bare ref itself, and that no branch row was ever created for this
	// session: the invariant every external_ref-keyed consumer relies on.
	var refVal string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, root).Scan(&refVal), "read external_ref")
	c.Eq(session, refVal, "root external_ref")
	var n int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE external_ref = $1 OR external_ref LIKE $1 || ':%'`,
		session).Scan(&n), "count family")
	c.Eq(1, n, "session family has")
}

// TestSubagentTurn2RoutesToItsOwnBranch pins the other half: a subagent whose
// first turn sat on the root row (no predecessor to resolve) is routed to its
// own branch the moment its second turn chains back, and its thread id is what
// names the branch.
func TestSubagentTurn2RoutesToItsOwnBranch(t *testing.T) {
	c := assert.NewCollecting(t)
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	root, err := s.ResolveThreadConversation(ctx, ref, "")
	c.Require().NoError(err, "ResolveThreadConversation root")

	// Subagent turn 1: no predecessor, so it lands on the root row for this
	// one request and is stamped a new thread root with its own id.
	t1, t1At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent 1")
	c.Require().NoError(s.RecordThread(ctx, session, root, t1, t1At, "", "msg_"+t1, true), "RecordThread 1")
	var t1Thread string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
		t1).Scan(&t1Thread), "read thread_id 1")
	c.Require().Eq(t1, t1Thread, "subagent turn 1 thread_id")

	// Subagent turn 2: predecessor = turn 1's response, whose thread_id is
	// turn 1's own id. The lookup must answer exactly that id and route to the
	// branch named after it, while turn 1's messages stay on the root row.
	tid, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t1)
	c.Require().NoError(err, "ThreadOfPredecessorInSession")
	c.Require().Eq(t1, tid, "lookup")
	branch, err := s.ResolveThreadConversation(ctx, ref, tid)
	c.Require().NoError(err, "ResolveThreadConversation branch")
	c.Require().NotEq(root, branch, "subagent turn 2 must leave the root row: its thread got a branch")
	var branchRef string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, branch).Scan(&branchRef), "read branch external_ref")
	c.Eq(session+":"+t1, branchRef, "branch external_ref")

	// The branch turn records its membership through the family-scoped lookup:
	// its predecessor's turn lives on the ROOT conversation, so a lookup scoped
	// to the turn's own row would miss and fork yet another thread.
	t2, t2At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: branch, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent 2")
	c.Require().NoError(s.RecordThread(ctx, session, branch, t2, t2At, "msg_"+t1, "msg_"+t2, true), "RecordThread 2")
	var t2Thread string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
		t2).Scan(&t2Thread), "read thread_id 2")
	c.Eq(t1, t2Thread, "subagent turn 2 thread_id")
	tid3, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t2)
	c.Require().NoError(err, "ThreadOfPredecessorInSession 3")
	c.Require().Eq(t1, tid3, "turn 3 lookup")
	branch3, err := s.ResolveThreadConversation(ctx, ref, tid3)
	c.Require().NoError(err, "ResolveThreadConversation 3")
	c.Eq(branch, branch3, "subagent turn 3 landed on")
}

// TestThreadOfPredecessorInSessionEscapesTheSessionWildcard calls the lookup
// against the real database with a session carrying both LIKE wildcards. The
// escape literal in the SQL is a raw-string backslash: get it wrong twice and
// PostgreSQL rejects every call with SQLSTATE 22025, get the replacer wrong
// and a decoy conversation of another session over-matches. Either regression
// fails here.
func TestThreadOfPredecessorInSessionEscapesTheSessionWildcard(t *testing.T) {
	c := assert.NewCollecting(t)
	s, _ := newTestStore(t)
	ctx := context.Background()
	// % inside and _ at the end, so the UNESCAPED pattern is all wildcards.
	session := "c_esc%" + t.Name() + "_"
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	root, err := s.ResolveThreadConversation(ctx, ref, "")
	c.Require().NoError(err, "ResolveThreadConversation root")

	// The session's own family: a main-thread turn on the root, a subagent
	// root beside it, and the subagent's second turn on its branch.
	tr, atr, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent main")
	c.Require().NoError(s.RecordThread(ctx, session, root, tr, atr, "", "msg_"+tr, false), "RecordThread main")
	ts, ats, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent subagent")
	c.Require().NoError(s.RecordThread(ctx, session, root, ts, ats, "", "msg_"+ts, true), "RecordThread subagent")
	branch, err := s.ResolveThreadConversation(ctx, ref, ts)
	c.Require().NoError(err, "ResolveThreadConversation branch")
	tb, atb, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: branch, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent branch")
	c.Require().NoError(s.RecordThread(ctx, session, branch, tb, atb, "msg_"+ts, "msg_"+tb, true), "RecordThread branch")

	// A decoy conversation of ANOTHER session that the unescaped pattern
	// matches: c_esc [any] <name> [one] : [any]. Only the escaped pattern may
	// keep it out.
	decoyRef := "c_escX" + t.Name() + "Z:decoy"
	decoy, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: decoyRef,
	})
	c.Require().NoError(err, "EnsureConversationByExternalRef decoy")
	td, atd, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: decoy, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent decoy")
	c.Require().NoError(s.RecordThread(ctx, decoyRef, decoy, td, atd, "", "msg_"+td, true), "RecordThread decoy")

	lookup := func(prevMessageID, want string) {
		t.Helper()
		got, err := s.ThreadOfPredecessorInSession(ctx, session, prevMessageID)
		c.Require().NoError(err, "ThreadOfPredecessorInSession(%q)", prevMessageID)
		c.Eq(want, got, "ThreadOfPredecessorInSession(%q) = %q, want", prevMessageID, got)
	}

	lookup("msg_"+tr, "")        // found on the root: the main thread
	lookup("msg_"+ts, ts)        // found on the root: a new thread root's id, verbatim
	lookup("msg_"+tb, ts)        // found on the branch through the LIKE arm
	lookup("msg_"+td, "")        // the decoy is another session's family; it must not match
	lookup("msg_never_seen", "") // a miss, never a most-recent-turn guess
}

// TestSessionFamilyExistsIsTheFoundingDiscriminator pins the one-query
// discriminator the proxy's beginCapture runs pre-upstream: family absent on
// a fresh session (the main thread's founding turn), present once the bare
// root row exists, empty session never a family.
func TestSessionFamilyExistsIsTheFoundingDiscriminator(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name() + "_" + time.Now().Format("150405.000000000")

	if got, err := s.SessionFamilyExists(ctx, ""); err != nil || got {
		t.Fatalf("empty session: got (%v, %v), want (false, nil)", got, err)
	}
	if got, err := s.SessionFamilyExists(ctx, session); err != nil || got {
		t.Fatalf("fresh session: got (%v, %v), want (false, nil): family absent means MAIN turn 1", got, err)
	}
	if _, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, ""); err != nil {
		t.Fatalf("ResolveThreadConversation: %v", err)
	}
	if got, err := s.SessionFamilyExists(ctx, session); err != nil || !got {
		t.Fatalf("after root creation: got (%v, %v), want (true, nil): family present means an unresolvable predecessor is an INDEPENDENT thread", got, err)
	}
}

// TestIndependentFoundingRequestLandsOnItsOwnBranch is the reviewer's probe
// scenario, now green: with the main thread's rows occupying ordinals 0..2 on
// the root row, an independent thread's founding request (its turn id
// pre-minted by the proxy) routes to its own branch, its response appends
// there without colliding, its thread_id is its own id, its turn 2 resolves
// to the same branch, and nothing it wrote appears on the root conversation
// the parent child renders.
func TestIndependentFoundingRequestLandsOnItsOwnBranch(t *testing.T) {
	c := assert.NewCollecting(t)
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name() + "_" + time.Now().Format("150405.000000000")
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	// Main turn 1 on the bare ref, then occupy the root row's ordinals 0..2
	// exactly as a two-message main request plus its response does.
	root, err := s.ResolveThreadConversation(ctx, ref, "")
	c.Require().NoError(err, "ResolveThreadConversation root")
	tMain, atMain, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent main")
	c.Require().NoError(s.RecordThread(ctx, session, root, tMain, atMain, "", "msg_main", false), "RecordThread main")
	mainHead, err := s.DecomposeRequest(ctx, root, tMain, atMain, []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`), "h", "")
	c.Require().NoError(err, "decompose main")
	c.Require().NoError(s.AppendResponseMessage(ctx, root, tMain, atMain, mainHead, []byte(`{"id":"msg_main","role":"assistant","content":[]}`), 1, 1, "end_turn"), "append main response")

	// The founding request: family exists, no resolvable predecessor. The
	// proxy mints the turn id and routes to <session>:<id> BEFORE the row
	// exists; InsertTurnIntent must reserve that id.
	exists, err := s.SessionFamilyExists(ctx, session)
	c.Require().False(err != nil || !exists, "SessionFamilyExists = (%v, %v), want (true, nil)", exists, err)
	minted := uuid.Must(uuid.NewV7()).String()
	branch, err := s.ResolveThreadConversation(ctx, ref, minted)
	c.Require().NoError(err, "ResolveThreadConversation branch")
	c.Require().NotEq(root, branch, "an independent founding request must land on its own branch row, not the root")
	tF, atF, err := s.InsertTurnIntent(ctx, TurnIntent{ID: minted, ConversationID: branch, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent founding")
	c.Require().Eq(minted, tF, "founding turn id")
	c.Require().NoError(s.RecordThread(ctx, session, branch, tF, atF, "", "msg_sub1", true), "RecordThread founding")

	// The probe's failing step, now green: the founding response appends on
	// the BRANCH, under the branch's own tree, while the root row keeps its
	// three nodes.
	foundingHead, err := s.DecomposeRequest(ctx, branch, tF, atF, []byte(`{"messages":[{"role":"user","content":"quick task"}]}`), "h", "")
	c.Require().NoError(err, "decompose founding")
	c.Require().NoError(s.AppendResponseMessage(ctx, branch, tF, atF, foundingHead, []byte(`{"id":"msg_sub1","role":"assistant","content":[]}`), 1, 1, "end_turn"), "append founding response")

	// Thread identity: the founding turn is its own thread root, and turn 2
	// resolves to the same branch.
	var thread string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
		tF).Scan(&thread), "read founding thread_id")
	c.Eq(minted, thread, "founding turn thread_id")
	tid2, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_sub1")
	c.Require().NoError(err, "ThreadOfPredecessorInSession turn 2")
	c.Require().Eq(minted, tid2, "subagent turn 2 resolved thread")
	conv2, err := s.ResolveThreadConversation(ctx, ref, tid2)
	c.Require().NoError(err, "ResolveThreadConversation turn 2")
	c.Require().Eq(branch, conv2, "subagent turn 2 landed on")

	// MINOR 4's assertion: nothing the founding turn wrote is on the root
	// conversation, so the parent child's rendered logs carry no ghost reply.
	var rootMsgs int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid`,
		root).Scan(&rootMsgs), "count root messages")
	c.Eq(3, rootMsgs, "root conversation has")
	var wantRef string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, branch).Scan(&wantRef), "read branch external_ref")
	c.Eq(wantRef, session+":"+minted, "branch external_ref")
}

// TestMainTurn1OnAFreshSessionKeepsNullThreadID pins the root invariant the
// founding discriminator must preserve: the session's MAIN turn 1 (family
// absent, no predecessor) stays on the bare session row with thread_id NULL,
// and the pre-minted-id path is never taken for it.
func TestMainTurn1OnAFreshSessionKeepsNullThreadID(t *testing.T) {
	c := assert.NewCollecting(t)
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name() + "_" + time.Now().Format("150405.000000000")
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	exists, err := s.SessionFamilyExists(ctx, session)
	c.Require().False(err != nil || exists, "SessionFamilyExists on a fresh session = (%v, %v), want (false, nil)", exists, err)
	root, err := s.ResolveThreadConversation(ctx, ref, "")
	c.Require().NoError(err, "ResolveThreadConversation")
	t1, at1, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	c.Require().NoError(err, "InsertTurnIntent")
	c.Require().NoError(s.RecordThread(ctx, session, root, t1, at1, "", "msg_"+t1, false), "RecordThread")
	var got, threadID string
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT external_ref, coalesce(thread_id::text, '') FROM conversations.conversation c
		   JOIN conversations.conversation_turn t ON t.conversation_id = c.id
		  WHERE t.id=$1::uuid`, t1).Scan(&got, &threadID), "read turn row")
	c.Eq(session, got, "conversation external_ref")
	c.Eq("", threadID, "main turn 1 thread_id")
}

// TestInsertTurnIntentRecordsRequestDescriptor pins the descriptor columns
// migration 0060 adds: the id of the last path row a request covered and the
// half-open range a trim dropped are written verbatim, and an intent carrying
// neither stores SQL NULL in all three.
func TestInsertTurnIntentRecordsRequestDescriptor(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.Require().NoError(err, "EnsureConversation")

	res, err := conv.NewStore(store.NewConvBackend(pool), conv.Options{}).Open(convID, conv.AsOwner()).
		Append(ctx, conv.Append{
			Root:  convtree.RootKindRoot,
			Nodes: []conv.NewNode{{Msg: msg.Message{Role: msg.User}, Content: []byte(`"hi"`)}},
		})
	c.Require().NoError(err, "append seed")
	c.Require().Eq(1, len(res.Nodes), "append node count")
	headID := res.Nodes[0].ID

	from, to := 1, 4
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Model: "claude-test",
		RequestHeadID: headID, TrimFrom: &from, TrimTo: &to,
	})
	c.Require().NoError(err, "InsertTurnIntent")

	var gotHead *string
	var gotFrom, gotTo *int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT request_head_id::text, request_trim_from, request_trim_to
		   FROM conversations.conversation_turn WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt).Scan(&gotHead, &gotFrom, &gotTo), "read back the request descriptor")
	c.False(gotHead == nil || *gotHead != headID, "request_head_id = %v, want %s", gotHead, headID)
	c.False(gotFrom == nil || *gotFrom != 1, "request_trim_from = %v, want 1", gotFrom)
	c.False(gotTo == nil || *gotTo != 4, "request_trim_to = %v, want 4", gotTo)

	t.Run("no descriptor stores NULL", func(t *testing.T) {
		sub := assert.NewAborting(t)
		turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
			ConversationID: convID, Model: "claude-test",
		})
		sub.Require().NoError(err, "InsertTurnIntent")

		var gotHead *string
		var gotFrom, gotTo *int
		sub.Require().NoError(pool.QueryRow(ctx,
			`SELECT request_head_id::text, request_trim_from, request_trim_to
			   FROM conversations.conversation_turn WHERE id=$1::uuid AND created_at=$2`,
			turnID, createdAt).Scan(&gotHead, &gotFrom, &gotTo), "read back the request descriptor")
		sub.Nil(gotHead, "request_head_id")
		sub.Nil(gotFrom, "request_trim_from")
		sub.Nil(gotTo, "request_trim_to")
	})
}

// TestInsertTurnIntentTrimRangeBothOrNeither pins the guard: a trim range is
// meaningful only as a pair, and a range whose start is past its end is a
// caller bug. Each refusal happens before the pool is touched, so no turn row
// is written.
func TestInsertTurnIntentTrimRangeBothOrNeither(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := captureTestEnv(t)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.Require().NoError(err, "EnsureConversation")

	from, to, five, two := 1, 4, 5, 2
	for _, tc := range []struct {
		name     string
		from, to *int
		want     string
	}{
		{"from without to", &from, nil, "both or neither"},
		{"to without from", nil, &to, "both or neither"},
		{"inverted", &five, &two, "inverted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := assert.NewAborting(t)
			_, _, err := s.InsertTurnIntent(ctx, TurnIntent{
				ConversationID: convID, Model: "claude-test",
				TrimFrom: tc.from, TrimTo: tc.to,
			})
			sub.ErrorContains(err, tc.want, "InsertTurnIntent trim range %s", tc.name)
		})
	}

	var turns int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_turn WHERE conversation_id=$1::uuid`,
		convID).Scan(&turns), "count turns")
	c.Eq(0, turns, "turns written by refused intents")
}
