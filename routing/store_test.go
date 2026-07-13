package routing

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timescale/rafiki/store"
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
		gotInputTokens         int64
		gotOutputTokens        int64
		gotCacheReadTokens     int64
		gotCacheCreationTokens int64
		gotLatencyMS           int
	)
	err = pool.QueryRow(ctx, `
		SELECT stop_reason, upstream, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, latency_ms
		  FROM conversations.conversation_turn
		 WHERE id=$1::uuid AND created_at=$2`,
		turnID, createdAt).Scan(
		&gotStopReason, &gotUpstream, &gotInputTokens, &gotOutputTokens,
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
