// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"
)

// TestCaptureStore requires a real PostgreSQL/TimescaleDB instance. Set
// RAFIKI_TEST_DSN to a connection string to run it, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable" go test ./capture/...
//
// It is skipped by default so plain unit test runs (and CI without a
// TimescaleDB instance) stay green.
func TestCaptureStore(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

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
}

// newTestStore connects a CaptureStore to the RAFIKI_TEST_DSN database,
// following TestCaptureStore's setup above: skip without the DSN, migrate, and
// hand back both the store and the pool the tests read through.
func newTestStore(t *testing.T) (*CaptureStore, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is; the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return NewCaptureStore(pool), pool
}

func mustTurn(t *testing.T, s *CaptureStore, convID string) string {
	t.Helper()
	id, _, err := s.InsertTurnIntent(context.Background(), TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	return id
}

func testEnsureConversationByExternalRef(t *testing.T, ctx context.Context, cs *CaptureStore) {
	ref1 := "ext-ref-" + time.Now().Format(time.RFC3339Nano)
	id1a, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "client", ExternalRef: ref1,
	})
	if err != nil {
		t.Fatalf("EnsureConversationByExternalRef (first): %v", err)
	}
	id1b, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "client", ExternalRef: ref1,
	})
	if err != nil {
		t.Fatalf("EnsureConversationByExternalRef (repeat): %v", err)
	}
	if id1a != id1b {
		t.Fatalf("same external_ref must resolve to the same conversation id: %q != %q", id1a, id1b)
	}

	ref2 := "ext-ref-" + time.Now().Add(time.Second).Format(time.RFC3339Nano)
	id2, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "client", ExternalRef: ref2,
	})
	if err != nil {
		t.Fatalf("EnsureConversationByExternalRef (distinct ref): %v", err)
	}
	if id2 == id1a {
		t.Fatalf("distinct external_ref must yield a distinct conversation id, got %q for both", id2)
	}
}

func testInsertTurnIntentAndCompleteTurn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cs *CaptureStore) {
	convID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "server",
	})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	turnID, createdAt, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID,
		Ordinal:        1,
		Model:          "claude-test",
		Request:        []byte(`{"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}

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
	if err := cs.CompleteTurn(ctx, want); err != nil {
		t.Fatalf("CompleteTurn: %v", err)
	}

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
	if err != nil {
		t.Fatalf("read back completed turn: %v", err)
	}
	if gotStopReason != want.StopReason {
		t.Errorf("stop_reason = %q, want %q", gotStopReason, want.StopReason)
	}
	if gotUpstream != want.Upstream {
		t.Errorf("upstream = %q, want %q", gotUpstream, want.Upstream)
	}
	if gotModel != "claude-test-served" {
		t.Errorf("model = %q, want served model to override intent", gotModel)
	}
	if gotInputTokens != want.InputTokens {
		t.Errorf("input_tokens = %d, want %d", gotInputTokens, want.InputTokens)
	}
	if gotOutputTokens != want.OutputTokens {
		t.Errorf("output_tokens = %d, want %d", gotOutputTokens, want.OutputTokens)
	}
	if gotCacheReadTokens != want.CacheReadTokens {
		t.Errorf("cache_read_tokens = %d, want %d", gotCacheReadTokens, want.CacheReadTokens)
	}
	if gotCacheCreationTokens != want.CacheCreationTokens {
		t.Errorf("cache_creation_tokens = %d, want %d", gotCacheCreationTokens, want.CacheCreationTokens)
	}
	if gotLatencyMS != want.LatencyMS {
		t.Errorf("latency_ms = %d, want %d", gotLatencyMS, want.LatencyMS)
	}
}

// testConversationModelBackfill: a client-driven conversation is created with
// no model (session-header only); the first turn backfills it and a later
// different-model turn does NOT overwrite. A library-style conversation that
// pins its model at creation keeps it.
func testConversationModelBackfill(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cs *CaptureStore) {
	ref := "backfill-" + time.Now().Format(time.RFC3339Nano)
	convID, err := cs.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	model := func() any {
		var m any
		if err := pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, convID).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := model(); m != nil {
		t.Fatalf("pre-turn model = %v, want NULL", m)
	}
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
	if m := model(); m != "claude-haiku-4-5" {
		t.Fatalf("model after different-model turn = %v, want unchanged claude-haiku-4-5", m)
	}

	// Library-style: model pinned at creation survives untouched.
	pinnedID, err := cs.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "diagnose", DrivenBy: "server", Model: "claude-sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: pinnedID, Model: "claude-opus-4-8", Request: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	var pinned string
	if err := pool.QueryRow(ctx, `SELECT model FROM conversations.conversation WHERE id=$1::uuid`, pinnedID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != "claude-sonnet-5" {
		t.Fatalf("pinned model = %q, want creation-time claude-sonnet-5", pinned)
	}
}

func TestDecomposeRequest_MessagesAndPrefix(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req := []byte(`{"model":"claude","system":[{"type":"text","text":"S"}],
		"tools":[{"name":"T"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[{"type":"text","text":"yo"}]}
		]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req), Protocol: "anthropic"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}

	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req))
	if err != nil {
		t.Fatalf("DecomposeRequest: %v", err)
	}
	if next != 2 {
		t.Fatalf("next ordinal = %d, want 2 (two messages)", next)
	}

	// two message rows, content stored verbatim
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1`, convID).Scan(&cnt); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("message count = %d, want 2", cnt)
	}

	// content stored byte-for-byte (modulo JSON normalization) for message 0
	var gotContent string
	if err := pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=0`,
		convID).Scan(&gotContent); err != nil {
		t.Fatalf("read message 0 content: %v", err)
	}
	wantContent := `[{"type":"text","text":"hi"}]`
	var gotNorm, wantNorm any
	if err := json.Unmarshal([]byte(gotContent), &gotNorm); err != nil {
		t.Fatalf("unmarshal got content: %v", err)
	}
	if err := json.Unmarshal([]byte(wantContent), &wantNorm); err != nil {
		t.Fatalf("unmarshal want content: %v", err)
	}
	if !reflect.DeepEqual(gotNorm, wantNorm) {
		t.Fatalf("message 0 content = %s, want %s (verbatim)", gotContent, wantContent)
	}

	// prefix_content stored on this (first) turn
	var pc *string
	if err := pool.QueryRow(ctx,
		`SELECT prefix_content::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&pc); err != nil {
		t.Fatalf("read prefix_content: %v", err)
	}
	if pc == nil {
		t.Fatal("prefix_content is NULL, want the request envelope stored on the first turn")
	}
	if !strings.Contains(*pc, `"tools"`) {
		t.Fatalf("prefix_content = %q, want it to contain \"tools\"", *pc)
	}
}

func TestDecomposeRequest_PrefixUnchangedIsNull(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"}]}`)
	h := routing.PrefixHash(req)
	t1, c1, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: h})
	if err != nil {
		t.Fatalf("InsertTurnIntent (t1): %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, convID, t1, c1, req, h); err != nil {
		t.Fatalf("DecomposeRequest (t1): %v", err)
	}

	// second turn, same prefix hash → prefix_content must be NULL
	req2 := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	t2, c2, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req2, PrefixHash: h})
	if err != nil {
		t.Fatalf("InsertTurnIntent (t2): %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, convID, t2, c2, req2, h); err != nil {
		t.Fatalf("DecomposeRequest (t2): %v", err)
	}

	var pc *string
	if err := pool.QueryRow(ctx,
		`SELECT prefix_content::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`, t2, c2).Scan(&pc); err != nil {
		t.Fatalf("read prefix_content: %v", err)
	}
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
			if got := messageHasCacheControl(json.RawMessage(c.content)); got != c.want {
				t.Errorf("messageHasCacheControl(%s) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}

// TestJSONBSafe checks the U+0000 sanitizer directly (no DB): clean content is
// returned verbatim, NUL-bearing content is stripped and stays valid JSON with
// integers preserved, and invalid JSON is passed through unchanged.
func TestJSONBSafe(t *testing.T) {
	clean := `{"type":"text","text":"hello"}`
	if got := string(jsonbSafe([]byte(clean))); got != clean {
		t.Errorf("clean content changed: %s (want verbatim)", got)
	}

	withNUL := `[{"type":"text","text":"a\u0000b","n":123456789012345678}]`
	out := string(jsonbSafe([]byte(withNUL)))
	if strings.Contains(out, `\u0000`) || strings.IndexByte(out, 0) >= 0 {
		t.Errorf("output still carries NUL: %s", out)
	}
	if !strings.Contains(out, "123456789012345678") {
		t.Errorf("large integer not preserved (UseNumber): %s", out)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("output not valid json: %v", err)
	}
	if arr[0]["text"] != "ab" {
		t.Errorf("text = %v, want \"ab\" (NUL stripped)", arr[0]["text"])
	}

	bad := `not json \u0000`
	if got := string(jsonbSafe([]byte(bad))); got != bad {
		t.Errorf("invalid json changed: %s (want unchanged)", got)
	}
}

// TestDecomposeRequest_NullEscapeInContent verifies a message whose content
// carries a \u0000 escape (which a raw jsonb insert rejects with SQLSTATE 22P05)
// is captured with the NUL stripped rather than failing the decompose.
func TestDecomposeRequest_NullEscapeInContent(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"before\u0000after"}]}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req)})
	if err != nil {
		t.Fatalf("InsertTurnIntent with a \\u0000 in request: %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req)); err != nil {
		t.Fatalf("DecomposeRequest with a \\u0000 in content should succeed, got: %v", err)
	}

	var content string
	if err := pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=0`, convID).Scan(&content); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if strings.IndexByte(content, 0) >= 0 {
		t.Errorf("stored content still contains a NUL byte: %q", content)
	}
	if !strings.Contains(content, "beforeafter") {
		t.Errorf("content = %q, want the NUL stripped to \"beforeafter\"", content)
	}
}

// TestDecomposeRequest_PrefixChangeReStores verifies on-change prefix detection
// re-fires after an unchanged run: three turns hashed h1, h1, h2 must store
// prefix_content on turns 1 and 3 (NULL on 2).
func TestDecomposeRequest_PrefixChangeReStores(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	reqA := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"}]}`)
	reqB := []byte(`{"model":"claude","tools":[{"name":"T"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	reqC := []byte(`{"model":"claude","tools":[{"name":"T"},{"name":"U"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"},{"role":"assistant","content":"d"},{"role":"user","content":"e"}]}`)
	hA, hB, hC := routing.PrefixHash(reqA), routing.PrefixHash(reqB), routing.PrefixHash(reqC)
	if hA != hB {
		t.Fatalf("precondition: reqA and reqB must share a prefix hash (same envelope), got %q vs %q", hA, hB)
	}
	if hC == hA {
		t.Fatalf("precondition: reqC must differ (new tool), got same hash %q", hC)
	}

	decompose := func(req []byte, h string) (string, time.Time) {
		id, ca, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: h})
		if err != nil {
			t.Fatalf("InsertTurnIntent: %v", err)
		}
		if _, err := s.DecomposeRequest(ctx, convID, id, ca, req, h); err != nil {
			t.Fatalf("DecomposeRequest: %v", err)
		}
		return id, ca
	}
	prefixOf := func(id string, ca time.Time) *string {
		var pc *string
		if err := pool.QueryRow(ctx,
			`SELECT prefix_content::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`, id, ca).Scan(&pc); err != nil {
			t.Fatalf("read prefix_content: %v", err)
		}
		return pc
	}

	id1, c1 := decompose(reqA, hA)
	id2, c2 := decompose(reqB, hB)
	id3, c3 := decompose(reqC, hC)

	if prefixOf(id1, c1) == nil {
		t.Fatal("turn 1 prefix_content is NULL, want stored (first turn)")
	}
	if prefixOf(id2, c2) != nil {
		t.Fatal("turn 2 prefix_content stored, want NULL (unchanged from turn 1)")
	}
	pc3 := prefixOf(id3, c3)
	if pc3 == nil {
		t.Fatal("turn 3 prefix_content is NULL, want re-stored (envelope changed after an unchanged run)")
	}
	if !strings.Contains(*pc3, `"U"`) {
		t.Fatalf("turn 3 prefix_content = %q, want the new tool \"U\" in the envelope", *pc3)
	}
}

// TestDecomposeRequest_CrossTurnIdempotent verifies each resubmitted message
// persists exactly once at a contiguous ordinal (ON CONFLICT DO NOTHING,
// first-writer-wins) across turns of one conversation.
func TestDecomposeRequest_CrossTurnIdempotent(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"a"}]}`)
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	for _, req := range [][]byte{req1, req2} {
		h := routing.PrefixHash(req)
		id, ca, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: h})
		if err != nil {
			t.Fatalf("InsertTurnIntent: %v", err)
		}
		if _, err := s.DecomposeRequest(ctx, convID, id, ca, req, h); err != nil {
			t.Fatalf("DecomposeRequest: %v", err)
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT ordinal, role FROM conversations.conversation_message WHERE conversation_id=$1 ORDER BY ordinal`, convID)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	type msg struct {
		ord  int
		role string
	}
	var got []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.ord, &m.role); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []msg{{0, "user"}, {1, "assistant"}, {2, "user"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("conversation_message = %v, want %v (each resubmitted message once, contiguous ordinals)", got, want)
	}
}

// horizonTestEnv is the shared preamble for the horizon-rebase tests: the
// RAFIKI_TEST_DSN skip, connect, migrate, store. The pool is returned for
// direct assertion queries.
func horizonTestEnv(t *testing.T) (context.Context, *pgxpool.Pool, *CaptureStore) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return ctx, pool, NewCaptureStore(pool)
}

// runTurn mirrors the proxy's per-turn sequence: InsertTurnIntent, then
// DecomposeRequest, then CompleteTurn — completion BEFORE the next turn's
// decompose is what the boundary path's prior-turn input_tokens lookup reads.
// Returns DecomposeRequest's horizon-aware ordinal.
func runTurn(t *testing.T, ctx context.Context, s *CaptureStore, convID string, req []byte, inTok, outTok int64) int {
	t.Helper()
	h := routing.PrefixHash(req)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: h, Protocol: "anthropic"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, h)
	if err != nil {
		t.Fatalf("DecomposeRequest: %v", err)
	}
	if err := s.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Response: []byte(`{"content":[]}`),
		StopReason: "end_turn", Upstream: "anthropic",
		InputTokens: inTok, OutputTokens: outTok,
	}); err != nil {
		t.Fatalf("CompleteTurn: %v", err)
	}
	return next
}

// requireKindRow reads one message row's kind/role/content/input_tokens.
func requireKindRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, convID string, ordinal int) (kind, role *string, inTok *int64, content string) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT kind, role, input_tokens, content::text FROM conversations.conversation_message
		  WHERE conversation_id=$1 AND ordinal=$2`, convID, ordinal).
		Scan(&kind, &role, &inTok, &content); err != nil {
		t.Fatalf("read row ordinal %d: %v", ordinal, err)
	}
	return kind, role, inTok, content
}

func requireJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("unmarshal got %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("unmarshal want %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("content = %s, want %s (verbatim modulo JSON normalization)", got, want)
	}
}

// TestDecomposeRequest_FirstPostCompactRebases: the first request whose message
// 0 diverges from the stored anchor rebases to a new horizon, tags message 0
// kind='compaction_summary', and returns horizon+len(messages).
func TestDecomposeRequest_FirstPostCompactRebases(t *testing.T) {
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	// Pre-compact turns: accumulate rows 0..1, then 2.
	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	if next := runTurn(t, ctx, s, convID, req1, 1234, 50); next != 2 {
		t.Fatalf("turn 1 next ordinal = %d, want 2", next)
	}
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	if next := runTurn(t, ctx, s, convID, req2, 1400, 60); next != 3 {
		t.Fatalf("turn 2 next ordinal = %d, want 3", next)
	}

	// Post-compact: Claude Code sends a compaction summary as message 0.
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":"SUMMARY: earlier conversation"},{"role":"user","content":"next question"}]}`)
	next := runTurn(t, ctx, s, convID, req3, 90, 8)
	if next != 5 {
		t.Fatalf("turn 3 next ordinal = %d, want 5 (horizon 3 + two messages)", next)
	}

	// Horizon advanced to the boundary (H' = max stored ordinal + 1 = 3).
	var horizon int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon); err != nil {
		t.Fatalf("read resume_from_ordinal: %v", err)
	}
	if horizon != 3 {
		t.Fatalf("resume_from_ordinal = %d, want 3", horizon)
	}

	// Boundary row: kind, role user, prior turn's input_tokens as the
	// approximate replaced-context size, summary content verbatim.
	kind, role, inTok, content := requireKindRow(t, ctx, pool, convID, 3)
	if kind == nil || *kind != "compaction_summary" {
		t.Fatalf("ordinal 3 kind = %v, want compaction_summary", kind)
	}
	if role == nil || *role != "user" {
		t.Fatalf("ordinal 3 role = %v, want user", role)
	}
	if inTok == nil || *inTok != 1400 {
		t.Fatalf("ordinal 3 input_tokens = %v, want 1400 (the immediately prior turn's usage)", inTok)
	}
	requireJSONEqual(t, content, `"SUMMARY: earlier conversation"`)

	// The post-boundary ordinary row carries no kind.
	kind4, _, _, content4 := requireKindRow(t, ctx, pool, convID, 4)
	if kind4 != nil {
		t.Fatalf("ordinal 4 kind = %v, want NULL", kind4)
	}
	requireJSONEqual(t, content4, `"next question"`)
}

// TestDecomposeRequest_SecondPostCompactDedups: the next request carrying the
// SAME summary as message 0 dedups positionally (DO NOTHING) — horizon
// unchanged, no second marker row, the boundary row's kind and input_tokens
// survive untouched.
func TestDecomposeRequest_SecondPostCompactDedups(t *testing.T) {
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	if next := runTurn(t, ctx, s, convID, req1, 1234, 10); next != 2 {
		t.Fatalf("turn 1 next ordinal = %d, want 2", next)
	}
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"SUMMARY: earlier conversation"},{"role":"user","content":"next question"}]}`)
	if next := runTurn(t, ctx, s, convID, req2, 90, 8); next != 4 {
		t.Fatalf("turn 2 next ordinal = %d, want 4", next)
	}

	// Same summary again: stable prefix from the horizon, DO NOTHING.
	req3 := req2
	if next := runTurn(t, ctx, s, convID, req3, 91, 9); next != 4 {
		t.Fatalf("turn 3 next ordinal = %d, want 4 (dedup at horizon 2 + two messages)", next)
	}

	var horizon int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon); err != nil {
		t.Fatalf("read resume_from_ordinal: %v", err)
	}
	if horizon != 2 {
		t.Fatalf("resume_from_ordinal = %d, want 2 (unchanged)", horizon)
	}
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1`, convID).Scan(&cnt); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if cnt != 4 {
		t.Fatalf("message count = %d, want 4 (nothing appended, nothing duplicated)", cnt)
	}
	// The boundary row survived the DO NOTHING: kind and approximate
	// input_tokens are still the first boundary's.
	kind, _, inTok, _ := requireKindRow(t, ctx, pool, convID, 2)
	if kind == nil || *kind != "compaction_summary" {
		t.Fatalf("ordinal 2 kind = %v, want compaction_summary (untouched by the dedup insert)", kind)
	}
	if inTok == nil || *inTok != 1234 {
		t.Fatalf("ordinal 2 input_tokens = %v, want 1234 (DO NOTHING keeps first-seen)", inTok)
	}
}

// TestDecomposeRequest_SecondDifferentSummaryRebasesAgain: a second, different
// compaction summary diverges again and rebases forward; the first boundary's
// marker row is untouched (append-only — nothing deleted or renumbered).
func TestDecomposeRequest_SecondDifferentSummaryRebasesAgain(t *testing.T) {
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	if next := runTurn(t, ctx, s, convID, req1, 1234, 10); next != 2 {
		t.Fatalf("turn 1 next ordinal = %d, want 2", next)
	}
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"SUMMARY-A"},{"role":"user","content":"q1"}]}`)
	if next := runTurn(t, ctx, s, convID, req2, 90, 8); next != 4 {
		t.Fatalf("turn 2 next ordinal = %d, want 4", next)
	}

	// A different summary: rebase again.
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":"SUMMARY-B"},{"role":"user","content":"q2"}]}`)
	next := runTurn(t, ctx, s, convID, req3, 60, 6)
	if next != 6 {
		t.Fatalf("turn 3 next ordinal = %d, want 6 (horizon 4 + two messages)", next)
	}

	var horizon int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon); err != nil {
		t.Fatalf("read resume_from_ordinal: %v", err)
	}
	if horizon != 4 {
		t.Fatalf("resume_from_ordinal = %d, want 4", horizon)
	}

	// The FIRST boundary's marker row is untouched.
	kind, role, inTok, content := requireKindRow(t, ctx, pool, convID, 2)
	if kind == nil || *kind != "compaction_summary" || role == nil || *role != "user" {
		t.Fatalf("ordinal 2 kind/role = %v/%v, want compaction_summary/user (first boundary preserved)", kind, role)
	}
	requireJSONEqual(t, content, `"SUMMARY-A"`)
	if inTok == nil || *inTok != 1234 {
		t.Fatalf("ordinal 2 input_tokens = %v, want 1234 (first boundary preserved)", inTok)
	}

	// The SECOND boundary row sits at the new horizon.
	kind, _, inTok, content = requireKindRow(t, ctx, pool, convID, 4)
	if kind == nil || *kind != "compaction_summary" {
		t.Fatalf("ordinal 4 kind = %v, want compaction_summary", kind)
	}
	requireJSONEqual(t, content, `"SUMMARY-B"`)
	// Prior turn by created_at is turn 2 (turn 1 is older), whose usage was 90.
	if inTok == nil || *inTok != 90 {
		t.Fatalf("ordinal 4 input_tokens = %v, want 90 (the immediately prior turn's usage)", inTok)
	}
}

// TestDecomposeRequest_ReAnchorRewind: a request whose messages 0 and 1 match
// two consecutive EARLIER stored ordinals (a rewind/resume from an older
// session) re-anchors the horizon to that match — appending positionally from
// it — WITHOUT recording a new kind='compaction_summary' boundary row.
func TestDecomposeRequest_ReAnchorRewind(t *testing.T) {
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	if next := runTurn(t, ctx, s, convID, req1, 100, 10); next != 2 {
		t.Fatalf("turn 1 next ordinal = %d, want 2", next)
	}
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	if next := runTurn(t, ctx, s, convID, req2, 110, 11); next != 3 {
		t.Fatalf("turn 2 next ordinal = %d, want 3", next)
	}
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":"SUMMARY-A"},{"role":"user","content":"q2"}]}`)
	if next := runTurn(t, ctx, s, convID, req3, 120, 12); next != 5 {
		t.Fatalf("turn 3 next ordinal = %d, want 5 (boundary at 3 + two messages)", next)
	}

	var boundaryCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind='compaction_summary'`, convID).Scan(&boundaryCount); err != nil {
		t.Fatalf("count boundary rows: %v", err)
	}
	if boundaryCount != 1 {
		t.Fatalf("boundary rows = %d, want 1 (the turn-3 marker)", boundaryCount)
	}

	// Rewind: the client resumes from the older, pre-compact session.
	req4 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"},{"role":"assistant","content":"r1"}]}`)
	next := runTurn(t, ctx, s, convID, req4, 130, 13)
	if next != 4 {
		t.Fatalf("turn 4 next ordinal = %d, want 4 (re-anchored to 0 + four messages)", next)
	}

	// No NEW boundary was recorded: still exactly one marker row, still the
	// turn-3 one (the rewound request's insert at its ordinal DO NOTHING'd).
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind='compaction_summary'`, convID).Scan(&boundaryCount); err != nil {
		t.Fatalf("count boundary rows: %v", err)
	}
	if boundaryCount != 1 {
		t.Fatalf("boundary rows = %d, want 1 (re-anchor records no marker)", boundaryCount)
	}
	_, _, _, content := requireKindRow(t, ctx, pool, convID, 3)
	requireJSONEqual(t, content, `"SUMMARY-A"`)

	// Re-anchor is returned-only in this implementation: the stored horizon is
	// untouched, so the next request re-resolves from it.
	var horizon int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(resume_from_ordinal,0) FROM conversations.conversation WHERE id=$1`, convID).Scan(&horizon); err != nil {
		t.Fatalf("read resume_from_ordinal: %v", err)
	}
	if horizon != 3 {
		t.Fatalf("resume_from_ordinal = %d, want 3 (re-anchor does not persist a new horizon)", horizon)
	}
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
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req1 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	if next := runTurn(t, ctx, s, convID, req1, 100, 10); next != 2 {
		t.Fatalf("turn 1 next ordinal = %d, want 2", next)
	}
	req2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"}]}`)
	if next := runTurn(t, ctx, s, convID, req2, 110, 11); next != 3 {
		t.Fatalf("turn 2 next ordinal = %d, want 3", next)
	}
	req3 := []byte(`{"model":"claude","messages":[{"role":"user","content":"SUMMARY-A"},{"role":"user","content":"q2"}]}`)
	if next := runTurn(t, ctx, s, convID, req3, 120, 12); next != 5 {
		t.Fatalf("turn 3 next ordinal = %d, want 5 (boundary at 3 + two messages)", next)
	}

	// Rewind turn, run manually (not via runTurn) so the response can be
	// appended against THIS turn's row, the way the proxy does post-stream.
	req4 := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"q1"},{"role":"assistant","content":"r1"}]}`)
	h := routing.PrefixHash(req4)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req4, PrefixHash: h, Protocol: "anthropic"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	// Re-anchors to 0: messages 0..2 match rows 0..2 positionally, and "r1" at
	// ordinal 3 DO NOTHINGs against the stored "SUMMARY-A" boundary row. The
	// returned ordinal 4 is ALREADY occupied by the "q2" row from turn 3.
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req4, h)
	if err != nil {
		t.Fatalf("DecomposeRequest: %v", err)
	}
	if next != 4 {
		t.Fatalf("rewind turn next ordinal = %d, want 4 (re-anchored to 0 + four messages)", next)
	}
	if err := s.CompleteTurn(ctx, TurnResult{
		TurnID: turnID, CreatedAt: createdAt, Response: []byte(`{"content":[]}`),
		StopReason: "end_turn", Upstream: "anthropic",
		InputTokens: 130, OutputTokens: 13,
	}); err != nil {
		t.Fatalf("CompleteTurn: %v", err)
	}

	// The colliding response must surface, not vanish: a response landing on an
	// occupied ordinal is always a lost reply, never a benign replay.
	canonical := []byte(`{"content":[{"type":"text","text":"rewound assistant reply"}]}`)
	err = s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 14, 2, "end_turn")
	if err == nil {
		t.Fatal("a rewind response at an occupied ordinal must not be silently dropped")
	}
	if !errors.Is(err, ErrOrdinalOccupied) {
		t.Fatalf("err = %v, want ErrOrdinalOccupied", err)
	}

	// No assistant row exists at that ordinal: the insert is refused, not
	// relocated.
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=$2 AND role='assistant')`,
		convID, next).Scan(&present); err != nil {
		t.Fatalf("read response row: %v", err)
	}
	if present {
		t.Fatalf("assistant row present at ordinal %d, want absent (the collision must write no row)", next)
	}
	// The pre-existing occupant's first-seen content wins, untouched.
	_, role, _, content := requireKindRow(t, ctx, pool, convID, next)
	if role == nil || *role != "user" {
		t.Fatalf("ordinal %d role = %v, want user (occupant untouched by the collision)", next, role)
	}
	requireJSONEqual(t, content, `"q2"`)

	// A refused append must not stamp the turn row: previously both turns kept
	// response_ordinal pointing at a user row.
	var respOrd *int
	if err := pool.QueryRow(ctx,
		`SELECT response_ordinal FROM conversations.conversation_turn WHERE id=$1::uuid`, turnID).Scan(&respOrd); err != nil {
		t.Fatalf("read response_ordinal: %v", err)
	}
	if respOrd != nil {
		t.Fatalf("response_ordinal = %d, want NULL (the failed append must not stamp the turn)", *respOrd)
	}
}

// TestDecomposeRequest_BootstrapNoBoundary: a brand-new conversation's first
// request has no anchor row at the horizon — it proceeds unchanged, records no
// boundary, and returns the plain message count.
func TestDecomposeRequest_BootstrapNoBoundary(t *testing.T) {
	ctx, pool, s := horizonTestEnv(t)
	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req := []byte(`{"model":"claude","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]}`)
	if next := runTurn(t, ctx, s, convID, req, 50, 5); next != 3 {
		t.Fatalf("next ordinal = %d, want 3", next)
	}

	// resume_from_ordinal still NULL (never bumped) — coalesce reads 0.
	var resume *int
	if err := pool.QueryRow(ctx,
		`SELECT resume_from_ordinal FROM conversations.conversation WHERE id=$1`, convID).Scan(&resume); err != nil {
		t.Fatalf("read resume_from_ordinal: %v", err)
	}
	if resume != nil {
		t.Fatalf("resume_from_ordinal = %d, want NULL (bootstrap recorded no boundary)", *resume)
	}
	var kindCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1 AND kind IS NOT NULL`, convID).Scan(&kindCount); err != nil {
		t.Fatalf("count kind rows: %v", err)
	}
	if kindCount != 0 {
		t.Fatalf("kind-tagged rows = %d, want 0 (bootstrap records no boundary)", kindCount)
	}
}

// TestDecomposeRequest_CacheBreakpoints verifies breakpoint detection is
// structure-aware: only messages with an actual cache_control field on a
// content block are recorded, not messages whose text merely contains the
// literal substring "cache_control".
func TestDecomposeRequest_CacheBreakpoints(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req := []byte(`{"model":"claude","tools":[{"name":"T"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"plain, no marker"}]},
			{"role":"assistant","content":[{"type":"text","text":"cached block","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"this text literally says cache_control but has no such field"}]}
		]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req)})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req)); err != nil {
		t.Fatalf("DecomposeRequest: %v", err)
	}

	var bp *string
	if err := pool.QueryRow(ctx,
		`SELECT cache_breakpoints::text FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&bp); err != nil {
		t.Fatalf("read cache_breakpoints: %v", err)
	}
	if bp == nil {
		t.Fatal("cache_breakpoints is NULL, want [1]")
	}
	var got []int
	if err := json.Unmarshal([]byte(*bp), &got); err != nil {
		t.Fatalf("unmarshal cache_breakpoints %q: %v", *bp, err)
	}
	want := []int{1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cache_breakpoints = %v, want %v (only the message with a real cache_control field, not the text-only mention)", got, want)
	}
}

// TestAppendResponseMessage verifies the canonical assistant response is
// appended as a conversation_message at the caller-supplied ordinal (verbatim
// content, token usage, stop_reason) and that the turn's response_ordinal is
// set to that same ordinal.
func TestAppendResponseMessage(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := NewCaptureStore(pool)

	convID, err := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

	req := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{
		ConversationID: convID, Request: req, PrefixHash: routing.PrefixHash(req)})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, routing.PrefixHash(req))
	if err != nil {
		t.Fatalf("DecomposeRequest: %v", err)
	}
	if next != 1 {
		t.Fatalf("next ordinal = %d, want 1 (one request message)", next)
	}

	canonical := []byte(`{"role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	if err := s.AppendResponseMessage(ctx, convID, turnID, createdAt, next, canonical, 10, 5, "end_turn"); err != nil {
		t.Fatalf("AppendResponseMessage: %v", err)
	}

	var role, stop string
	var out int64
	if err := pool.QueryRow(ctx,
		`SELECT role, coalesce(stop_reason,''), coalesce(output_tokens,0) FROM conversations.conversation_message
		  WHERE conversation_id=$1 AND ordinal=$2`, convID, next).Scan(&role, &stop, &out); err != nil {
		t.Fatalf("read conversation_message: %v", err)
	}
	if role != "assistant" {
		t.Fatalf("role = %q, want assistant", role)
	}
	if stop != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", stop)
	}
	if out != 5 {
		t.Fatalf("output_tokens = %d, want 5", out)
	}

	var gotContent string
	if err := pool.QueryRow(ctx,
		`SELECT content::text FROM conversations.conversation_message WHERE conversation_id=$1 AND ordinal=$2`,
		convID, next).Scan(&gotContent); err != nil {
		t.Fatalf("read content: %v", err)
	}
	wantContent := `[{"type":"text","text":"hello"}]`
	var gotNorm, wantNorm any
	if err := json.Unmarshal([]byte(gotContent), &gotNorm); err != nil {
		t.Fatalf("unmarshal got content: %v", err)
	}
	if err := json.Unmarshal([]byte(wantContent), &wantNorm); err != nil {
		t.Fatalf("unmarshal want content: %v", err)
	}
	if !reflect.DeepEqual(gotNorm, wantNorm) {
		t.Fatalf("content = %s, want %s (verbatim canonical.content)", gotContent, wantContent)
	}

	var respOrd int
	if err := pool.QueryRow(ctx,
		`SELECT response_ordinal FROM conversations.conversation_turn WHERE id=$1 AND created_at=$2`,
		turnID, createdAt).Scan(&respOrd); err != nil {
		t.Fatalf("read response_ordinal: %v", err)
	}
	if respOrd != next {
		t.Fatalf("response_ordinal = %d, want %d", respOrd, next)
	}
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
			if got != tt.retry {
				t.Errorf("isRetryableDB(%v) = %v, want %v", tt.err, got, tt.retry)
			}
		})
	}
}

func TestRetryDB_NonTransientNoRetry(t *testing.T) {
	calls := 0
	err := retryDB(context.Background(), "test", func(ctx context.Context) error {
		calls++
		return errors.New("permanent")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetryDB_TransientRecovers(t *testing.T) {
	withShortDBRetryDelays(t)
	calls := 0
	err := retryDB(context.Background(), "test", func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return &net.OpError{Op: "read", Err: errors.New("connection reset")}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected recovery, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestRetryDB_ExhaustedRetries(t *testing.T) {
	withShortDBRetryDelays(t)
	calls := 0
	err := retryDB(context.Background(), "test", func(ctx context.Context) error {
		calls++
		return context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if calls != 4 { // initial + 3 retries
		t.Fatalf("expected 4 calls, got %d", calls)
	}
}

func TestRetryDB_ParentContextCanceled(t *testing.T) {
	withShortDBRetryDelays(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryDB(ctx, "test", func(ctx context.Context) error {
		calls++
		return context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("expected error from cancelled parent")
	}
	// With a cancelled parent, the select at the end of the retry loop
	// immediately returns ctx.Err(). Should not retry.
	if calls > 1 {
		t.Fatalf("expected at most 1 call with cancelled parent, got %d", calls)
	}
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
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	cs := NewCaptureStore(pool)

	convID, err := cs.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "test", DrivenBy: "server"})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

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
		if err != nil {
			t.Fatalf("InsertTurnIntent: %v", err)
		}
		if tc.fail {
			if err := cs.FailTurn(ctx, turnID, createdAt, "deliberate"); err != nil {
				t.Fatalf("FailTurn: %v", err)
			}
			continue
		}
		if err := cs.CompleteTurn(ctx, TurnResult{
			TurnID: turnID, CreatedAt: createdAt, Model: tc.model, Upstream: "test",
			InputTokens: tc.in, OutputTokens: tc.out, CacheReadTokens: tc.cr, CacheCreationTokens: tc.cw,
		}); err != nil {
			t.Fatalf("CompleteTurn: %v", err)
		}
	}

	got, err := cs.ConversationTokens(ctx, convID)
	if err != nil {
		t.Fatalf("ConversationTokens: %v", err)
	}
	byModel := map[string]ModelTokens{}
	for _, m := range got {
		byModel[m.Model] = m
	}
	if len(byModel) != 2 {
		t.Fatalf("got %d model groups (%v), want 2 — the errored turn must be excluded", len(byModel), byModel)
	}
	if s := byModel["claude-sonnet-5"]; s.InputTokens != 300 || s.OutputTokens != 30 || s.CacheReadTokens != 3000 || s.CacheCreationTokens != 10 {
		t.Errorf("claude-sonnet-5 = %+v, want in=300 out=30 cr=3000 cw=10", s)
	}
	if s := byModel["deepseek/deepseek-v4-pro"]; s.InputTokens != 300 || s.CacheReadTokens != 3000 {
		t.Errorf("deepseek = %+v, want in=300 cr=3000", s)
	}
}

func TestRecordThreadLinksConsecutiveTurns(t *testing.T) {
	s, pool := newTestStore(t) // follow the file's existing helper
	ctx := context.Background()
	convID, err := s.EnsureConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client",
	})
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	// No session header, so the predecessor lookup scopes to the turn's own
	// conversation: the pre-session shape of the same rule.
	const session = ""

	threadOf := func(turnID string) string {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
			turnID).Scan(&got); err != nil {
			t.Fatalf("read thread_id: %v", err)
		}
		return got
	}

	// Turn 1: the main thread's first turn. thread_id NULL is the convention
	// that keeps the session's root conversation on the bare external_ref.
	t1, t1At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	if err := s.RecordThread(ctx, session, convID, t1, t1At, "", "msg_one", false); err != nil {
		t.Fatalf("RecordThread turn 1: %v", err)
	}

	// Turn 2: chains to turn 1. The predecessor IS the main thread (its
	// thread_id is NULL), so turn 2 keeps NULL too.
	t2, t2At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	if err := s.RecordThread(ctx, session, convID, t2, t2At, "msg_one", "msg_two", false); err != nil {
		t.Fatalf("RecordThread turn 2: %v", err)
	}

	// Turn 3: a SECOND root, as a concurrent subagent's first turn is. The
	// billing header marks it cc_is_subagent, so it founds its own thread.
	t3, t3At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	if err := s.RecordThread(ctx, session, convID, t3, t3At, "", "msg_three", true); err != nil {
		t.Fatalf("RecordThread turn 3: %v", err)
	}

	// Turn 4: the subagent's second turn, chaining to its own first.
	t4, t4At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	if err := s.RecordThread(ctx, session, convID, t4, t4At, "msg_three", "msg_four", true); err != nil {
		t.Fatalf("RecordThread turn 4: %v", err)
	}

	if threadOf(t1) != "" {
		t.Errorf("turn 1 (main thread root) thread_id = %s, want NULL", threadOf(t1))
	}
	if threadOf(t2) != "" {
		t.Errorf("turn 2 (main thread, chained) thread_id = %s, want NULL: a NULL predecessor thread stays the main thread", threadOf(t2))
	}
	if threadOf(t3) == "" {
		t.Error("turn 3 (subagent first turn) thread_id is NULL, want its own id: a subagent with no predecessor founds a new thread")
	}
	if threadOf(t3) != t3 {
		t.Errorf("turn 3 thread = %s, want its own id %s", threadOf(t3), t3)
	}
	if threadOf(t4) != t3 {
		t.Errorf("turn 4 thread = %s, want turn 3's id %s: the subagent thread continues", threadOf(t4), t3)
	}
}

func TestOrdinalCollisionOnAResponseIsLoud(t *testing.T) {
	// A replayed request prefix legitimately re-inserts identical content and
	// stays lenient. A SECOND assistant reply at one ordinal is always a lost
	// response, so it must surface rather than vanish.
	s, _ := newTestStore(t)
	ctx := context.Background()
	convID, _ := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})

	t1, at1, _ := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	first := []byte(`{"id":"msg_a","content":[{"type":"text","text":"first"}]}`)
	if err := s.AppendResponseMessage(ctx, convID, t1, at1, 5, first, 1, 1, "end_turn"); err != nil {
		t.Fatalf("first append: %v", err)
	}

	t2, at2, _ := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	second := []byte(`{"id":"msg_b","content":[{"type":"text","text":"second"}]}`)
	err := s.AppendResponseMessage(ctx, convID, t2, at2, 5, second, 1, 1, "end_turn")
	if err == nil {
		t.Fatal("a second response at ordinal 5 must not be silently dropped")
	}
	if !errors.Is(err, ErrOrdinalOccupied) {
		t.Fatalf("err = %v, want ErrOrdinalOccupied", err)
	}
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
	if _, err := s.DecomposeRequest(ctx, convID, mustTurn(t, s, convID), time.Now(), body, "h"); err != nil {
		t.Fatalf("replay decompose must stay lenient, got: %v", err)
	}
}

func TestRecordThreadUnresolvablePredecessorStaysMainThreadUnlessSubagent(t *testing.T) {
	// A predecessor from before this column existed, or from a conversation the
	// proxy did not capture, must not silently attach the turn to an unrelated
	// thread. For the main thread that means staying the main thread (thread_id
	// NULL, on the conversation the routing already chose); for a subagent it
	// means founding a new thread with its own id.
	s, pool := newTestStore(t)
	ctx := context.Background()
	convID, _ := s.EnsureConversation(ctx, ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client"})

	id, at, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent main: %v", err)
	}
	if err := s.RecordThread(ctx, "", convID, id, at, "msg_never_seen", "msg_mine", false); err != nil {
		t.Fatalf("RecordThread main: %v", err)
	}

	subID, subAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent subagent: %v", err)
	}
	if err := s.RecordThread(ctx, "", convID, subID, subAt, "msg_also_never_seen", "msg_sub_mine", true); err != nil {
		t.Fatalf("RecordThread subagent: %v", err)
	}

	threadOf := func(turnID string) string {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
			turnID).Scan(&got); err != nil {
			t.Fatalf("read thread_id: %v", err)
		}
		return got
	}
	if got := threadOf(id); got != "" {
		t.Errorf("main thread with an unresolvable predecessor: thread_id = %s, want NULL (not the unrelated thread a guess would attach)", got)
	}
	if got := threadOf(subID); got != subID {
		t.Errorf("subagent with an unresolvable predecessor: thread_id = %s, want its own id %s (a new thread root)", got, subID)
	}
}

func TestConcurrentThreadsDoNotShareAnOrdinalSpace(t *testing.T) {
	// The corruption this closes: two threads with equal message counts computed
	// identical ordinals, and ON CONFLICT DO NOTHING dropped the loser's reply.
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()

	root, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "")
	if err != nil {
		t.Fatalf("ResolveThreadConversation root: %v", err)
	}
	branch, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "thread-b")
	if err != nil {
		t.Fatalf("ResolveThreadConversation branch: %v", err)
	}
	if root == branch {
		t.Fatal("a non-root thread must get its own conversation row")
	}

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
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id=$1::uuid`,
			c.id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.name, err)
		}
		if n != 1 {
			t.Errorf("%s conversation has %d messages, want 1", c.name, n)
		}
	}

	// The branch must be discoverable from the session for cost rollup.
	var ref string
	if err := pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, branch).Scan(&ref); err != nil {
		t.Fatalf("read external_ref: %v", err)
	}
	if want := session + ":thread-b"; ref != want {
		t.Errorf("branch external_ref = %q, want %q", ref, want)
	}
}

func TestRootThreadKeepsTheBareSessionExternalRef(t *testing.T) {
	// Every existing conversation, every cost rollup keyed on external_ref and
	// every `rafiki logs <child>` depends on this staying unchanged.
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()
	id, err := s.ResolveThreadConversation(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session,
	}, "")
	if err != nil {
		t.Fatalf("ResolveThreadConversation: %v", err)
	}
	var ref string
	if err := pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, id).Scan(&ref); err != nil {
		t.Fatalf("read external_ref: %v", err)
	}
	if ref != session {
		t.Errorf("root external_ref = %q, want the bare session %q", ref, session)
	}
}

// TestChainedMainThreadRequestsStayOnTheBareSessionRow pins the routing rule
// end to end against the real database: the main thread's second and later
// requests DO carry a resolvable predecessor, and that predecessor's thread_id
// is NULL, so every one of them keeps landing on the bare-ref conversation and
// the bare ref is never suffixed. Before the NULL-thread_id convention, the
// resolver forked a fresh conversation per request and cost rollups keyed on
// external_ref counted only the first one.
func TestChainedMainThreadRequestsStayOnTheBareSessionRow(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	root, err := s.ResolveThreadConversation(ctx, ref, "")
	if err != nil {
		t.Fatalf("ResolveThreadConversation root: %v", err)
	}

	// Request 1: no predecessor (the main thread's first turn).
	t1, t1At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent 1: %v", err)
	}
	if err := s.RecordThread(ctx, session, root, t1, t1At, "", "msg_"+t1, false); err != nil {
		t.Fatalf("RecordThread 1: %v", err)
	}

	// Request 2: chains to request 1's response, exactly as Claude Code does.
	tid1, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t1)
	if err != nil {
		t.Fatalf("ThreadOfPredecessorInSession 1: %v", err)
	}
	if tid1 != "" {
		t.Fatalf("turn 1 is the main thread: lookup = %q, want the empty thread (root row)", tid1)
	}
	conv2, err := s.ResolveThreadConversation(ctx, ref, tid1)
	if err != nil {
		t.Fatalf("ResolveThreadConversation 2: %v", err)
	}
	if conv2 != root {
		t.Fatalf("request 2 landed on %s, want the root row %s", conv2, root)
	}
	t2, t2At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: conv2, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent 2: %v", err)
	}
	if err := s.RecordThread(ctx, session, conv2, t2, t2At, "msg_"+t1, "msg_"+t2, false); err != nil {
		t.Fatalf("RecordThread 2: %v", err)
	}

	// Request 3: same again, proving the chain stays main-thread past turn 2.
	tid2, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t2)
	if err != nil {
		t.Fatalf("ThreadOfPredecessorInSession 2: %v", err)
	}
	if tid2 != "" {
		t.Fatalf("turn 2 chains to the main thread: lookup = %q, want the empty thread", tid2)
	}
	conv3, err := s.ResolveThreadConversation(ctx, ref, tid2)
	if err != nil {
		t.Fatalf("ResolveThreadConversation 3: %v", err)
	}
	if conv3 != root {
		t.Fatalf("request 3 landed on %s, want the root row %s", conv3, root)
	}

	for i, turnID := range []string{t1, t2} {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
			turnID).Scan(&got); err != nil {
			t.Fatalf("read thread_id %d: %v", i+1, err)
		}
		if got != "" {
			t.Errorf("main-thread turn %d thread_id = %s, want NULL", i+1, got)
		}
	}

	// The bare ref itself, and that no branch row was ever created for this
	// session: the invariant every external_ref-keyed consumer relies on.
	var refVal string
	if err := pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, root).Scan(&refVal); err != nil {
		t.Fatalf("read external_ref: %v", err)
	}
	if refVal != session {
		t.Errorf("root external_ref = %q, want the unsuffixed %q", refVal, session)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation WHERE external_ref = $1 OR external_ref LIKE $1 || ':%'`,
		session).Scan(&n); err != nil {
		t.Fatalf("count family: %v", err)
	}
	if n != 1 {
		t.Errorf("session family has %d conversations, want 1: chained main-thread requests must never fork a branch", n)
	}
}

// TestSubagentTurn2RoutesToItsOwnBranch pins the other half: a subagent whose
// first turn sat on the root row (no predecessor to resolve) is routed to its
// own branch the moment its second turn chains back, and its thread id is what
// names the branch.
func TestSubagentTurn2RoutesToItsOwnBranch(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	session := "c_" + t.Name()
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	root, err := s.ResolveThreadConversation(ctx, ref, "")
	if err != nil {
		t.Fatalf("ResolveThreadConversation root: %v", err)
	}

	// Subagent turn 1: no predecessor, so it lands on the root row for this
	// one request and is stamped a new thread root with its own id.
	t1, t1At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent 1: %v", err)
	}
	if err := s.RecordThread(ctx, session, root, t1, t1At, "", "msg_"+t1, true); err != nil {
		t.Fatalf("RecordThread 1: %v", err)
	}
	var t1Thread string
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
		t1).Scan(&t1Thread); err != nil {
		t.Fatalf("read thread_id 1: %v", err)
	}
	if t1Thread != t1 {
		t.Fatalf("subagent turn 1 thread_id = %s, want its own id %s", t1Thread, t1)
	}

	// Subagent turn 2: predecessor = turn 1's response, whose thread_id is
	// turn 1's own id. The lookup must answer exactly that id and route to the
	// branch named after it, while turn 1's messages stay on the root row.
	tid, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t1)
	if err != nil {
		t.Fatalf("ThreadOfPredecessorInSession: %v", err)
	}
	if tid != t1 {
		t.Fatalf("lookup = %q, want the predecessor's thread id %q", tid, t1)
	}
	branch, err := s.ResolveThreadConversation(ctx, ref, tid)
	if err != nil {
		t.Fatalf("ResolveThreadConversation branch: %v", err)
	}
	if branch == root {
		t.Fatal("subagent turn 2 must leave the root row: its thread got a branch")
	}
	var branchRef string
	if err := pool.QueryRow(ctx,
		`SELECT external_ref FROM conversations.conversation WHERE id=$1::uuid`, branch).Scan(&branchRef); err != nil {
		t.Fatalf("read branch external_ref: %v", err)
	}
	if want := session + ":" + t1; branchRef != want {
		t.Errorf("branch external_ref = %q, want %q", branchRef, want)
	}

	// The branch turn records its membership through the family-scoped lookup:
	// its predecessor's turn lives on the ROOT conversation, so a lookup scoped
	// to the turn's own row would miss and fork yet another thread.
	t2, t2At, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: branch, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent 2: %v", err)
	}
	if err := s.RecordThread(ctx, session, branch, t2, t2At, "msg_"+t1, "msg_"+t2, true); err != nil {
		t.Fatalf("RecordThread 2: %v", err)
	}
	var t2Thread string
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(thread_id::text, '') FROM conversations.conversation_turn WHERE id=$1::uuid`,
		t2).Scan(&t2Thread); err != nil {
		t.Fatalf("read thread_id 2: %v", err)
	}
	if t2Thread != t1 {
		t.Errorf("subagent turn 2 thread_id = %s, want the predecessor's %s: the chain must survive the conversation boundary", t2Thread, t1)
	}
	tid3, err := s.ThreadOfPredecessorInSession(ctx, session, "msg_"+t2)
	if err != nil {
		t.Fatalf("ThreadOfPredecessorInSession 3: %v", err)
	}
	if tid3 != t1 {
		t.Fatalf("turn 3 lookup = %q, want %q: the subagent thread stays on its branch", tid3, t1)
	}
	branch3, err := s.ResolveThreadConversation(ctx, ref, tid3)
	if err != nil {
		t.Fatalf("ResolveThreadConversation 3: %v", err)
	}
	if branch3 != branch {
		t.Errorf("subagent turn 3 landed on %s, want the same branch %s", branch3, branch)
	}
}

// TestThreadOfPredecessorInSessionEscapesTheSessionWildcard calls the lookup
// against the real database with a session carrying both LIKE wildcards. The
// escape literal in the SQL is a raw-string backslash: get it wrong twice and
// PostgreSQL rejects every call with SQLSTATE 22025, get the replacer wrong
// and a decoy conversation of another session over-matches. Either regression
// fails here.
func TestThreadOfPredecessorInSessionEscapesTheSessionWildcard(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	// % inside and _ at the end, so the UNESCAPED pattern is all wildcards.
	session := "c_esc%" + t.Name() + "_"
	ref := ConversationRef{OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: session}

	root, err := s.ResolveThreadConversation(ctx, ref, "")
	if err != nil {
		t.Fatalf("ResolveThreadConversation root: %v", err)
	}

	// The session's own family: a main-thread turn on the root, a subagent
	// root beside it, and the subagent's second turn on its branch.
	tr, atr, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent main: %v", err)
	}
	if err := s.RecordThread(ctx, session, root, tr, atr, "", "msg_"+tr, false); err != nil {
		t.Fatalf("RecordThread main: %v", err)
	}
	ts, ats, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: root, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent subagent: %v", err)
	}
	if err := s.RecordThread(ctx, session, root, ts, ats, "", "msg_"+ts, true); err != nil {
		t.Fatalf("RecordThread subagent: %v", err)
	}
	branch, err := s.ResolveThreadConversation(ctx, ref, ts)
	if err != nil {
		t.Fatalf("ResolveThreadConversation branch: %v", err)
	}
	tb, atb, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: branch, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent branch: %v", err)
	}
	if err := s.RecordThread(ctx, session, branch, tb, atb, "msg_"+ts, "msg_"+tb, true); err != nil {
		t.Fatalf("RecordThread branch: %v", err)
	}

	// A decoy conversation of ANOTHER session that the unescaped pattern
	// matches: c_esc [any] <name> [one] : [any]. Only the escaped pattern may
	// keep it out.
	decoyRef := "c_escX" + t.Name() + "Z:decoy"
	decoy, err := s.EnsureConversationByExternalRef(ctx, ConversationRef{
		OriginEntrypoint: "claude", DrivenBy: "client", ExternalRef: decoyRef,
	})
	if err != nil {
		t.Fatalf("EnsureConversationByExternalRef decoy: %v", err)
	}
	td, atd, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: decoy, Model: "m"})
	if err != nil {
		t.Fatalf("InsertTurnIntent decoy: %v", err)
	}
	if err := s.RecordThread(ctx, decoyRef, decoy, td, atd, "", "msg_"+td, true); err != nil {
		t.Fatalf("RecordThread decoy: %v", err)
	}

	lookup := func(prevMessageID, want string) {
		t.Helper()
		got, err := s.ThreadOfPredecessorInSession(ctx, session, prevMessageID)
		if err != nil {
			t.Fatalf("ThreadOfPredecessorInSession(%q): %v", prevMessageID, err)
		}
		if got != want {
			t.Errorf("ThreadOfPredecessorInSession(%q) = %q, want %q", prevMessageID, got, want)
		}
	}

	lookup("msg_"+tr, "")        // found on the root: the main thread
	lookup("msg_"+ts, ts)        // found on the root: a new thread root's id, verbatim
	lookup("msg_"+tb, ts)        // found on the branch through the LIKE arm
	lookup("msg_"+td, "")        // the decoy is another session's family; it must not match
	lookup("msg_never_seen", "") // a miss, never a most-recent-turn guess
}
