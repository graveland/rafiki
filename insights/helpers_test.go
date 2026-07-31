// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/rafiki/store"
)

// Integration tests need a real TimescaleDB (>= 2.22, PostgreSQL 18 for
// uuidv7()). Set RAFIKI_TEST_DSN to run them, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres@localhost:5432/postgres?sslmode=disable" go test ./insights/...
//
// Each call to newTestPool runs in its own scratch database, migrated to head.

var scratchSeq atomic.Uint64

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_insights_%d_%d", time.Now().UnixNano(), scratchSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate scratch db: %v", err)
	}
	return pool
}

// insertConversation creates a conversation row and returns its id. owner is
// stored NULL when empty.
func insertConversation(t *testing.T, pool *pgxpool.Pool, drivenBy, owner string) string {
	t.Helper()
	var ownerArg any
	if owner != "" {
		ownerArg = owner
	}
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (owner, persona, model, origin_entrypoint, driven_by)
		 VALUES ($1, 'team-platform', 'claude-fable-5', 'test', $2) RETURNING id::text`,
		ownerArg, drivenBy).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

// seedTurn is a fully-specified conversation_turn for tests.
type seedTurn struct {
	ordinal         int
	status          string // defaults to "complete"
	model           string
	source          string
	upstream        string
	inTok           int64
	outTok          int64
	cacheRead       int64
	cacheCreate     int64
	latencyMS       int
	prefixHash      string
	prefixContent   string // JSON; stored NULL when empty
	responseOrdinal *int
	createdAt       time.Time // defaults to now() when zero
}

func insertTurn(t *testing.T, pool *pgxpool.Pool, convID string, st seedTurn) string {
	t.Helper()
	status := st.status
	if status == "" {
		status = "complete"
	}
	createdAt := st.createdAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	nullStr := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	nullJSON := func(s string) any {
		if s == "" {
			return nil
		}
		return []byte(s)
	}
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation_turn
		   (conversation_id, ordinal, status, model, request, response, stop_reason,
		    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		    upstream, latency_ms, source, prefix_hash, response_ordinal, prefix_content, created_at)
		 VALUES ($1, $2, $3, $4, '{}'::jsonb, NULL, 'end_turn',
		         $5, $6, $7, $8,
		         $9, $10, $11, $12, $13, $14, $15)
		 RETURNING id::text`,
		convID, st.ordinal, status, nullStr(st.model),
		st.inTok, st.outTok, st.cacheRead, st.cacheCreate,
		nullStr(st.upstream), st.latencyMS, nullStr(st.source), nullStr(st.prefixHash),
		st.responseOrdinal, nullJSON(st.prefixContent), createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	return id
}

func insertMessage(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int, role, contentJSON string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
		 VALUES ($1, $2, $3, $4)`,
		convID, ordinal, role, []byte(contentJSON))
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

// seedConversation creates a conversation with two complete turns and a
// user/assistant message pair, returning its id. Used by Search tests.
func seedConversation(t *testing.T, pool *pgxpool.Pool, drivenBy, owner string) string {
	t.Helper()
	convID := insertConversation(t, pool, drivenBy, owner)
	base := time.Now().Add(-time.Hour)
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 0, model: "claude-fable-5", source: "claude", upstream: "anthropic",
		inTok: 100, outTok: 50, cacheRead: 0, latencyMS: 1200,
		prefixHash: "hash-a", prefixContent: `{"system":"you are helpful"}`,
		createdAt: base,
	})
	one := 1
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 1, model: "claude-fable-5", source: "claude", upstream: "anthropic",
		inTok: 120, outTok: 60, cacheRead: 80, latencyMS: 900,
		prefixHash: "hash-a", responseOrdinal: &one,
		createdAt: base.Add(time.Minute),
	})
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"hello there"}]`)
	insertMessage(t, pool, convID, 1, "assistant", `[{"type":"text","text":"hi"}]`)
	return convID
}
