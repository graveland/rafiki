package routing

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/rafiki/store"
)

// TestCaptureStore requires a real PostgreSQL/TimescaleDB instance. Set
// RAFIKI_TEST_DSN to a connection string to run it, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable" go test ./routing/...
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
		ConversationID: convID, Request: req, PrefixHash: PrefixHash(req), Protocol: "anthropic"})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}

	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, PrefixHash(req))
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
	h := PrefixHash(req)
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
	turnID, createdAt, err := s.InsertTurnIntent(ctx, TurnIntent{ConversationID: convID, Request: req, PrefixHash: PrefixHash(req)})
	if err != nil {
		t.Fatalf("InsertTurnIntent with a \\u0000 in request: %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, PrefixHash(req)); err != nil {
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
	hA, hB, hC := PrefixHash(reqA), PrefixHash(reqB), PrefixHash(reqC)
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
		h := PrefixHash(req)
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
		ConversationID: convID, Request: req, PrefixHash: PrefixHash(req)})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	if _, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, PrefixHash(req)); err != nil {
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
		ConversationID: convID, Request: req, PrefixHash: PrefixHash(req)})
	if err != nil {
		t.Fatalf("InsertTurnIntent: %v", err)
	}
	next, err := s.DecomposeRequest(ctx, convID, turnID, createdAt, req, PrefixHash(req))
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
