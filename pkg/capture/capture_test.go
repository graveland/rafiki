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

func mustTurn(t *testing.T, s *CaptureStore, convID string) string {
	t.Helper()
	id, _, err := s.InsertTurnIntent(context.Background(), TurnIntent{ConversationID: convID, Model: "m"})
	assert.NewAborting(t).NoError(err, "InsertTurnIntent")
	return id
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
		Request:        []byte(`{"messages":[]}`),
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
			ConversationID: convID, Ordinal: 1, Model: "claude-test", Request: []byte(`{"messages":[]}`),
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
		ConversationID: convID, Model: "claude-haiku-4-5", Request: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if m := model(); m != "claude-haiku-4-5" {
		t.Fatalf("model after first turn = %v, want backfilled claude-haiku-4-5", m)
	}
	// A later turn with a DIFFERENT model must not overwrite: first-seen wins.
	if _, _, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Model: "openai/gpt-4o", Request: []byte(`{}`),
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
		ConversationID: pinnedID, Model: "claude-opus-4-8", Request: []byte(`{}`),
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
		ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req), Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")

	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req))
	c.NoError(err, "DecomposeRequest")
	c.Eq(2, next, "next ordinal")

	// two message rows, content stored verbatim
	var cnt int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1`, convID).Scan(&cnt), "count messages")
	c.Eq(2, cnt, "message count")

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
	t1, c1, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: h})
	c.NoError(err, "InsertTurnIntent (t1)")
	if _, err := s.DecomposeRequest(ctx, convID, t1, c1, req, h); err != nil {
		t.Fatalf("DecomposeRequest (t1): %v", err)
	}

	// second turn, same prefix hash → prefix_content must be NULL
	req2 := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	t2, c2, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req2, PrefixHash: h})
	c.NoError(err, "InsertTurnIntent (t2)")
	if _, err := s.DecomposeRequest(ctx, convID, t2, c2, req2, h); err != nil {
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
// integers preserved, and invalid JSON is passed through unchanged.
func TestJSONBSafe(t *testing.T) {
	c := assert.NewCollecting(t)
	clean := `{"type":"text","text":"hello"}`
	c.Eq(clean, string(jsonbSafe([]byte(clean))), "clean content changed")

	withNUL := `[{"type":"text","text":"a\u0000b","n":123456789012345678}]`
	out := string(jsonbSafe([]byte(withNUL)))
	c.False(strings.Contains(out, `\u0000`) || strings.IndexByte(out, 0) >= 0, "output still carries NUL: %s", out)
	c.StrContains(out, "123456789012345678", "large integer not preserved (UseNumber)")
	var arr []map[string]any
	c.Require().NoError(json.Unmarshal([]byte(out), &arr), "output not valid json")
	if arr[0]["text"] != "ab" {
		t.Errorf("text = %v, want \"ab\" (NUL stripped)", arr[0]["text"])
	}

	bad := `not json \u0000`
	c.Eq(bad, string(jsonbSafe([]byte(bad))), "invalid json changed")
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
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req)})
	c.Require().NoError(err, "InsertTurnIntent with a \\u0000 in request")
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req)); err != nil {
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
		id, ca, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: h})
		c.NoError(err, "InsertTurnIntent")
		if _, err := s.DecomposeRequest(ctx, convID, id, ca, req, h); err != nil {
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
		id, ca, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: h})
		c.NoError(err, "InsertTurnIntent")
		if _, err := s.DecomposeRequest(ctx, convID, id, ca, req, h); err != nil {
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

// horizonTestEnv is the shared preamble for the horizon-rebase tests: the
// RAFIKI_TEST_DSN skip, connect, migrate, store. The pool is returned for
// direct assertion queries.
func horizonTestEnv(t *testing.T) (context.Context, *pgxpool.Pool, *CaptureStore) {
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

// runTurn mirrors the proxy's per-turn sequence: InsertTurnIntent, then
// DecomposeRequest, then CompleteTurn — completion BEFORE the next turn's
// decompose is what the boundary path's prior-turn input_tokens lookup reads.
// Returns DecomposeRequest's horizon-aware ordinal.
func runTurn(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte, inTok, outTok int64) int {
	t.Helper()
	c := assert.NewAborting(t)
	h := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: h, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, h)
	c.NoError(err, "DecomposeRequest")
	c.NoError(s.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Response: []byte(`{"content":[]}`),
		StopReason: "end_turn", Upstream: "anthropic",
		InputTokens: inTok, OutputTokens: outTok,
	}), "CompleteTurn")
	return next
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
// carries. The fixtures these tests used before ("SUMMARY: earlier
// conversation", "SUMMARY-A") encoded the bug resolveHorizon's classifier
// exists to fix: structurally divergent, but no summary prose, so they must
// not be tagged a boundary.
func summaryContent(analysis string) string {
	return `[{"type":"text","text":"This session is being continued from a previous conversation that ran out of context. The conversation is summarized below: Analysis: ` + analysis + `"}]`
}

// TestDecomposeRequest_FirstPostCompactRebases: the first request whose message
// 0 diverges from the stored anchor rebases to a new horizon, tags message 0
// kind='compaction_summary', and returns horizon+len(messages).
func TestDecomposeRequest_FirstPostCompactRebases(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	// Pre-compact turns: accumulate rows 0..1, then 2.
	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 1234, 50), "turn 1 next ordinal")
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 1400, 60), "turn 2 next ordinal")

	// Post-compact: Claude Code sends a compaction summary as message 0.
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("the user asked for a refactor") + `},{"role":"user","content":"next question"}]}`)
	next := runTurn(t, ctx, s, convID, req3, 90, 8)
	c.Eq(5, next, "turn 3 next ordinal")

	// Horizon advanced to the boundary (H' = max stored ordinal + 1 = 3).
	var horizon int
	c.NoError(pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon), "read resume_from_ordinal")
	c.Eq(3, horizon, "resume_from_ordinal")

	// Boundary row: kind, role user, prior turn's input_tokens as the
	// approximate replaced-context size, summary content verbatim.
	kind, role, inTok, content := requireKindRow(t, ctx, pool, convID, 3)
	c.False(kind == nil || *kind != "compaction_summary", "ordinal 3 kind = %v, want compaction_summary", kind)
	c.False(role == nil || *role != "user", "ordinal 3 role = %v, want user", role)
	c.False(inTok == nil || *inTok != 1400, "ordinal 3 input_tokens = %v, want 1400 (the immediately prior turn's usage)", inTok)
	requireJSONEqual(t, content, summaryContent("the user asked for a refactor"))

	// The post-boundary ordinary row carries no kind.
	kind4, _, _, content4 := requireKindRow(t, ctx, pool, convID, 4)
	c.Nil(kind4, "ordinal 4 kind")
	requireJSONEqual(t, content4, `"next question"`)
}

// TestDecomposeRequest_SecondPostCompactDedups: the next request carrying the
// SAME summary as message 0 dedups positionally (DO NOTHING) — horizon
// unchanged, no second marker row, the boundary row's kind and input_tokens
// survive untouched.
func TestDecomposeRequest_SecondPostCompactDedups(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 1234, 10), "turn 1 next ordinal")
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("the user asked for a refactor") + `},{"role":"user","content":"next question"}]}`)
	c.Eq(4, runTurn(t, ctx, s, convID, req2, 90, 8), "turn 2 next ordinal")

	// Same summary again: stable prefix from the horizon, DO NOTHING.
	req3 := req2
	c.Eq(4, runTurn(t, ctx, s, convID, req3, 91, 9), "turn 3 next ordinal")

	var horizon int
	c.NoError(pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon), "read resume_from_ordinal")
	c.Eq(2, horizon, "resume_from_ordinal")
	var cnt int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1`, convID).Scan(&cnt), "count messages")
	c.Eq(4, cnt, "message count")
	// The boundary row survived the DO NOTHING: kind and approximate
	// input_tokens are still the first boundary's.
	kind, _, inTok, _ := requireKindRow(t, ctx, pool, convID, 2)
	c.False(kind == nil || *kind != "compaction_summary", "ordinal 2 kind = %v, want compaction_summary (untouched by the dedup insert)", kind)
	c.False(inTok == nil || *inTok != 1234, "ordinal 2 input_tokens = %v, want 1234 (DO NOTHING keeps first-seen)", inTok)
}

// TestDecomposeRequest_SecondDifferentSummaryRebasesAgain: a second, different
// compaction summary diverges again and rebases forward; the first boundary's
// marker row is untouched (append-only — nothing deleted or renumbered).
func TestDecomposeRequest_SecondDifferentSummaryRebasesAgain(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 1234, 10), "turn 1 next ordinal")
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-A details") + `},{"role":"user","content":"q1"}]}`)
	c.Eq(4, runTurn(t, ctx, s, convID, req2, 90, 8), "turn 2 next ordinal")

	// A different summary: rebase again.
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-B details") + `},{"role":"user","content":"q2"}]}`)
	next := runTurn(t, ctx, s, convID, req3, 60, 6)
	c.Eq(6, next, "turn 3 next ordinal")

	var horizon int
	c.NoError(pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon), "read resume_from_ordinal")
	c.Eq(4, horizon, "resume_from_ordinal")

	// The FIRST boundary's marker row is untouched.
	kind, role, inTok, content := requireKindRow(t, ctx, pool, convID, 2)
	c.False(kind == nil || *kind != "compaction_summary" || role == nil || *role != "user", "ordinal 2 kind/role = %v/%v, want compaction_summary/user (first boundary preserved)", kind, role)
	requireJSONEqual(t, content, summaryContent("SUMMARY-A details"))
	c.False(inTok == nil || *inTok != 1234, "ordinal 2 input_tokens = %v, want 1234 (first boundary preserved)", inTok)

	// The SECOND boundary row sits at the new horizon.
	kind, _, inTok, content = requireKindRow(t, ctx, pool, convID, 4)
	c.False(kind == nil || *kind != "compaction_summary", "ordinal 4 kind = %v, want compaction_summary", kind)
	requireJSONEqual(t, content, summaryContent("SUMMARY-B details"))
	// Prior turn by created_at is turn 2 (turn 1 is older), whose usage was 90.
	c.False(inTok == nil || *inTok != 90, "ordinal 4 input_tokens = %v, want 90 (the immediately prior turn's usage)", inTok)
}

// TestDecomposeRequest_ReAnchorRewind: a request whose messages 0 and 1 match
// two consecutive EARLIER stored ordinals (a rewind/resume from an older
// session) re-anchors the horizon to that match — appending positionally from
// it — WITHOUT recording a new kind='compaction_summary' boundary row.
func TestDecomposeRequest_ReAnchorRewind(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 100, 10), "turn 1 next ordinal")
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 110, 11), "turn 2 next ordinal")
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-A details") + `},{"role":"user","content":"q2"}]}`)
	c.Eq(5, runTurn(t, ctx, s, convID, req3, 120, 12), "turn 3 next ordinal")

	var boundaryCount int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind='compaction_summary'`, convID).Scan(&boundaryCount), "count boundary rows")
	c.Eq(1, boundaryCount, "boundary rows")

	// Rewind: the client resumes from the older, pre-compact session.
	req4 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"},{"role":"assistant","content":"r1"}]}`)
	next := runTurn(t, ctx, s, convID, req4, 130, 13)
	c.Eq(4, next, "turn 4 next ordinal")

	// No NEW boundary was recorded: still exactly one marker row, still the
	// turn-3 one (the rewound request's insert at its ordinal DO NOTHING'd).
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind='compaction_summary'`, convID).Scan(&boundaryCount), "count boundary rows")
	c.Eq(1, boundaryCount, "boundary rows")
	_, _, _, content := requireKindRow(t, ctx, pool, convID, 3)
	requireJSONEqual(t, content, summaryContent("SUMMARY-A details"))

	// Re-anchor is returned-only in this implementation: the stored horizon is
	// untouched, so the next request re-resolves from it.
	var horizon int
	c.NoError(pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon), "read resume_from_ordinal")
	c.Eq(3, horizon, "resume_from_ordinal")
}

// TestDecomposeRequest_RewindResponseCollisionIsLoud pins the response-side
// consequence of the re-anchor rewind, strict since task 4.2: a rewound request
// re-anchors to an earlier horizon, so DecomposeRequest's returned ordinal lands
// on an already-occupied row, and AppendResponseMessage at that ordinal FAILS
// with ErrOrdinalOccupied rather than silently dropping the reply. The earlier
// lenient pin (review finding 1, coordinator-accepted then as design §3's
// documented leniency) recorded 22 dropped responses on one real conversation
// before it was reversed. runTurn never calls AppendResponseMessage, which is
// why the other horizon tests cannot see this behavior; the request-side
// inserts of the same rewind stay lenient (the "r1" row DO NOTHINGs against the
// stored SUMMARY-A boundary row and DecomposeRequest still returns 4).
func TestDecomposeRequest_RewindResponseCollisionIsLoud(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 100, 10), "turn 1 next ordinal")
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req2, 110, 11), "turn 2 next ordinal")
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + summaryContent("SUMMARY-A details") + `},{"role":"user","content":"q2"}]}`)
	c.Eq(5, runTurn(t, ctx, s, convID, req3, 120, 12), "turn 3 next ordinal")

	// Rewind turn, run manually (not via runTurn) so the response can be
	// appended against THIS turn's row, the way the proxy does post-stream.
	req4 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"},{"role":"assistant","content":"r1"}]}`)
	h := routing.PrefixHash(req4)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req4, PrefixHash: h, Protocol: "anthropic"})
	c.NoError(err, "InsertTurnIntent")
	// Re-anchors to 0: messages 0..2 match rows 0..2 positionally, and "r1" at
	// ordinal 3 DO NOTHINGs against the stored "SUMMARY-A" boundary row. The
	// returned ordinal 4 is ALREADY occupied by the "q2" row from turn 3.
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req4, h)
	c.NoError(err, "DecomposeRequest")
	c.Eq(4, next, "rewind turn next ordinal")
	c.NoError(s.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Response: []byte(`{"content":[]}`),
		StopReason: "end_turn", Upstream: "anthropic",
		InputTokens: 130, OutputTokens: 13,
	}), "CompleteTurn")

	// The colliding response must surface, not vanish: a response landing on an
	// occupied ordinal is always a lost reply, never a benign replay.
	canonical := []byte(`{"content":[{"type":"text","text":"rewound assistant reply"}]}`)
	err = s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 14, 2, "end_turn")
	c.Error(err, "a rewind response at an occupied ordinal must not be silently dropped")
	c.ErrorIs(err, ErrOrdinalOccupied, "err")

	// No assistant row exists at that ordinal: the insert is refused, not
	// relocated.
	var present bool
	c.NoError(pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=$2 AND role='assistant')`,
		convID, next).Scan(&present), "read response row")
	c.False(present, "assistant row present at ordinal %d, want absent (the collision must write no row)", next)
	// The pre-existing occupant's first-seen content wins, untouched.
	_, role, _, content := requireKindRow(t, ctx, pool, convID, next)
	c.False(role == nil || *role != "user", "ordinal %d role = %v, want user (occupant untouched by the collision)", next, role)
	requireJSONEqual(t, content, `"q2"`)

	// A refused append must not stamp the turn row: previously both turns kept
	// response_ordinal pointing at a user row.
	var respOrd *int
	c.NoError(pool.QueryRow(ctx,
		`SELECT response_ordinal FROM conversations.conversation_turn WHERE id=$1::uuid`, turnID).Scan(&respOrd), "read response_ordinal")
	if respOrd != nil {
		t.Fatalf("response_ordinal = %d, want NULL (the failed append must not stamp the turn)", *respOrd)
	}
}

// TestDecomposeRequest_BootstrapNoBoundary: a brand-new conversation's first
// request has no anchor row at the horizon — it proceeds unchanged, records no
// boundary, and returns the plain message count.
func TestDecomposeRequest_BootstrapNoBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req := []byte(`{"model":"claude","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]}`)
	c.Eq(3, runTurn(t, ctx, s, convID, req, 50, 5), "next ordinal")

	// resume_from_ordinal still NULL (never bumped) — coalesce reads 0.
	var resume *int
	c.NoError(pool.QueryRow(ctx,
		`SELECT resume_from_ordinal FROM conversations.conversation WHERE id=$1`, convID).Scan(&resume), "read resume_from_ordinal")
	if resume != nil {
		t.Fatalf("resume_from_ordinal = %d, want NULL (bootstrap recorded no boundary)", *resume)
	}
	var kindCount int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind IS NOT NULL`, convID).Scan(&kindCount), "count kind rows")
	c.Eq(0, kindCount, "kind-tagged rows")
}

// TestASessionPreambleIsNotACompactionBoundary pins the classifier on the
// shape that mis-tagged 19 rows: a thread's first message is its own session
// preamble, and it always diverges structurally.
func TestASessionPreambleIsNotACompactionBoundary(t *testing.T) {
	// All 19 compaction_summary rows in the investigated database were session
	// preambles: CLAUDE.md contents, the userEmail system-reminder, the
	// environment block. Not one was a summary.
	preamble := `[{"type":"text","text":"<system-reminder>\nCodebase and user instructions are shown below.\n# userEmail\nThe user's email address is someone@example.com.\n</system-reminder>"}]`
	assert.NewCollecting(t).False(looksLikeCompactionSummary([]byte(preamble)), "a session preamble must not be classified as a compaction summary")
}

func TestARealCompactionSummaryIsRecognised(t *testing.T) {
	summary := `[{"type":"text","text":"This session is being continued from a previous conversation that ran out of context. The conversation is summarized below:\nAnalysis: the user asked for..."}]`
	assert.NewCollecting(t).True(looksLikeCompactionSummary([]byte(summary)), "a real compaction summary must be recognised")
}

// TestABareStringCompactionSummaryIsRecognised pins the shape observed in
// production: Claude Code sends the compaction summary with `content` as a bare
// JSON string, not a block array. The array-only decode returned false, so the
// boundary went unrecorded and the response append collided with a
// pre-compaction row (ErrOrdinalOccupied), failing the turn.
func TestABareStringCompactionSummaryIsRecognised(t *testing.T) {
	c := assert.NewCollecting(t)
	summary := `"This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier turns.\nAnalysis: ..."`
	c.True(looksLikeCompactionSummary([]byte(summary)), "a bare-string compaction summary must be recognised")
	c.False(looksLikeCompactionSummary([]byte(`"take a look at ssh greyshift and check on the cluster"`)), "an ordinary bare-string prompt must not be classified as a summary")
}

// TestDecomposeRequest_DivergentPreambleDoesNotRebase pins the guard itself,
// end to end: a request whose message 0 diverges from the stored anchor but
// carries no summary prose (a thread's own session preamble, the shape all 19
// mis-tagged rows had) inserts at the existing horizon untagged. Without the
// prose check this request would move the resume point and tag a boundary.
func TestDecomposeRequest_DivergentPreambleDoesNotRebase(t *testing.T) {
	c := assert.NewAborting(t)
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	c.NoError(err, "EnsureConversation")

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req1, 1234, 10), "turn 1 next ordinal")

	preamble := `[{"type":"text","text":"<system-reminder>\nCodebase and user instructions are shown below.\n</system-reminder>"}]`
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":` + preamble + `},{"role":"user","content":"q1"}]}`)
	c.Eq(2, runTurn(t, ctx, s, convID, req2, 90, 8), "turn 2 next ordinal")

	// The horizon did not move: resume_from_ordinal is still NULL.
	var resume *int
	c.NoError(pool.QueryRow(ctx,
		`SELECT resume_from_ordinal FROM conversations.conversation WHERE id=$1`, convID).Scan(&resume), "read resume_from_ordinal")
	if resume != nil {
		t.Fatalf("resume_from_ordinal = %d, want NULL (a preamble must not move the horizon)", *resume)
	}
	var kindCount int
	c.NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind IS NOT NULL`, convID).Scan(&kindCount), "count kind rows")
	c.Eq(0, kindCount, "kind-tagged rows")
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
		ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req)})
	c.NoError(err, "InsertTurnIntent")
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req)); err != nil {
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
// appended as a conversation_message at the caller-supplied ordinal (verbatim
// content, token usage, stop_reason) and that the turn's response_ordinal is
// set to that same ordinal.
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
		ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req)})
	c.NoError(err, "InsertTurnIntent")
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req))
	c.NoError(err, "DecomposeRequest")
	c.Eq(1, next, "next ordinal")

	canonical := []byte(`{"role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	c.NoError(s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 10, 5, "end_turn"), "AppendResponseMessage")

	var role, stop string
	var out int64
	c.NoError(pool.QueryRow(ctx,
		`SELECT role, coalesce(stop_reason,''), coalesce(output_tokens,0) FROM conversations.conversation_message
		  WHERE conversation_id=$1 AND ordinal=$2`, convID, next).Scan(&role, &stop, &out), "read conversation_message")
	c.Eq("assistant", role, "role")
	c.Eq("end_turn", stop, "stop_reason")
	c.Eq(5, out, "output_tokens")

	var gotContent string
	c.NoError(pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=$2`,
		convID, next).Scan(&gotContent), "read content")
	wantContent := `[{"type":"text","text":"hello"}]`
	var gotNorm, wantNorm any
	c.NoError(json.Unmarshal([]byte(gotContent), &gotNorm), "unmarshal got content")
	c.NoError(json.Unmarshal([]byte(wantContent), &wantNorm), "unmarshal want content")
	c.EqDeep(wantNorm, gotNorm, "content = %s, want %s (verbatim canonical.content)", gotContent, wantContent)

	var respOrd int
	c.NoError(pool.QueryRow(ctx,
		`SELECT response_ordinal FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&respOrd), "read response_ordinal")
	c.Eq(next, respOrd, "response_ordinal")
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
		{"ErrOrdinalOccupied wrapped as the proxy delivers it", fmt.Errorf("append response: insert: %w", ErrOrdinalOccupied), liveCtx, false},
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

func TestOrdinalCollisionOnAResponseIsLoud(t *testing.T) {
	c := assert.NewAborting(t)
	// A replayed request prefix legitimately re-inserts identical content and
	// stays lenient. A SECOND assistant reply at one ordinal is always a lost
	// response, so it must surface rather than vanish.
	s, _ := newTestStore(t)
	ctx := context.Background()
	convID, _ := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})

	t1, at1, _ := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	first := []byte(`{"id":"msg_a","content":[{"type":"text","text":"first"}]}`)
	c.NoError(s.AppendResponseMessage(ctx, convID, t1, at1, 5, first, 1, 1, "end_turn"), "first append")

	t2, at2, _ := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	second := []byte(`{"id":"msg_b","content":[{"type":"text","text":"second"}]}`)
	err := s.AppendResponseMessage(ctx, convID, t2, at2, 5, second, 1, 1, "end_turn")
	c.Error(err, "a second response at ordinal 5 must not be silently dropped")
	c.ErrorIs(err, ErrOrdinalOccupied, "err")
}

func TestRequestMessageReplayStaysLenient(t *testing.T) {
	// The other half of the rule: re-decomposing the same request is normal and
	// must stay silent, or every turn of a conversation errors.
	s, _ := newTestStore(t)
	ctx := context.Background()
	convID, _ := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if _, err := s.DecomposeRequest(ctx, convID, mustTurn(t, s, convID), time.Now(), body, "h"); err != nil {
		t.Fatalf("first decompose: %v", err)
	}
	_, err := s.DecomposeRequest(ctx, convID, mustTurn(t, s, convID), time.Now(), body, "h")
	assert.NewAborting(t).NoError(err, "replay decompose must stay lenient, got")
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

func TestConcurrentThreadsDoNotShareAnOrdinalSpace(t *testing.T) {
	ck := assert.NewCollecting(t)
	// The corruption this closes: two threads with equal message counts computed
	// identical ordinals, and ON CONFLICT DO NOTHING dropped the loser's reply.
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()

	root, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "")
	ck.Require().NoError(err, "ResolveThreadConversation root")
	branch, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "thread-b")
	ck.Require().NoError(err, "ResolveThreadConversation branch")
	ck.Require().NotEq(branch, root, "a non-root thread must get its own conversation row")

	// Both threads write a message at ordinal 0. Before the fix, the second was
	// silently dropped.
	body := []byte(`{"messages":[{"role":"user","content":"same shape"}]}`)
	if _, err := s.DecomposeRequest(ctx, root, mustTurn(t, s, root), time.Now(), body, "h"); err != nil {
		t.Fatalf("decompose root: %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, branch, mustTurn(t, s, branch), time.Now(), body, "h"); err != nil {
		t.Fatalf("decompose branch: %v", err)
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
	next, err := s.DecomposeRequest(ctx, root, tMain, atMain, []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`), "h")
	c.Require().NoError(err, "decompose main")
	c.Require().Eq(2, next, "root horizon")
	c.Require().NoError(s.AppendResponseMessage(ctx, root, tMain, atMain, next, []byte(`{"id":"msg_main","role":"assistant","content":[]}`), 1, 1, "end_turn"), "append main response")

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
	// the BRANCH at ordinal 1 even though the root row holds ordinals 0..2.
	nextF, err := s.DecomposeRequest(ctx, branch, tF, atF, []byte(`{"messages":[{"role":"user","content":"quick task"}]}`), "h")
	c.Require().NoError(err, "decompose founding")
	c.Require().NoError(s.AppendResponseMessage(ctx, branch, tF, atF, nextF, []byte(`{"id":"msg_sub1","role":"assistant","content":[]}`), 1, 1, "end_turn"), "append founding response")

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
