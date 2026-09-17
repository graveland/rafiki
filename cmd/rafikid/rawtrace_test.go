package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/store"
)

// openTestPool opens a pool to RAFIKI_TEST_DSN or skips the test.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestRawTrace_Insert verifies that RawTraceStore.Insert writes a row and the
// row can be queried back. Requires RAFIKI_TEST_DSN.
func TestRawTrace_Insert(t *testing.T) {
	pool := openTestPool(t)

	// Clean any leftover rows from prior runs.
	if _, err := pool.Exec(t.Context(), `DELETE FROM conversations.raw_http_request`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	store := rawtrace.NewRawTraceStore(pool)
	if store == nil {
		t.Fatal("NewRawTraceStore returned nil")
	}

	convID := "00000000-0000-0000-0000-000000000001"
	turnID := "00000000-0000-0000-0000-000000000002"
	status := 200
	r := rawtrace.RawHTTPRequest{
		Source:         "proxy",
		Model:          "claude-sonnet-4-5",
		Upstream:       "anthropic",
		ReqMethod:      "POST",
		ReqPath:        "/v1/messages",
		ReqHeaders:     json.RawMessage(`{"Content-Type":"application/json"}`),
		ReqBody:        json.RawMessage(`{"model":"claude-sonnet-4-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`),
		RespStatus:     &status,
		RespHeaders:    json.RawMessage(`{}`),
		RespBody:       json.RawMessage(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"hello"}]}`),
		LatencyMS:      42,
		ConversationID: &convID,
		TurnID:         &turnID,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := store.Insert(ctx, r); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Query back the row.
	var count int
	err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM conversations.raw_http_request
		 WHERE source='proxy' AND model='claude-sonnet-4-5'`).Scan(&count)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}

	// Verify column values.
	var gotModel, gotSource, gotReqMethod string
	var gotLatency int
	err = pool.QueryRow(t.Context(),
		`SELECT model, source, req_method, latency_ms
		 FROM conversations.raw_http_request
		 WHERE source='proxy' LIMIT 1`).Scan(&gotModel, &gotSource, &gotReqMethod, &gotLatency)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	if gotModel != "claude-sonnet-4-5" {
		t.Errorf("model: got %q, want %q", gotModel, "claude-sonnet-4-5")
	}
	if gotSource != "proxy" {
		t.Errorf("source: got %q, want %q", gotSource, "proxy")
	}
	if gotReqMethod != "POST" {
		t.Errorf("req_method: got %q, want %q", gotReqMethod, "POST")
	}
	if gotLatency != 42 {
		t.Errorf("latency_ms: got %d, want 42", gotLatency)
	}
}

// TestRawTrace_InsertNilStore verifies that Insert on a nil store is a no-op.
func TestRawTrace_InsertNilStore(t *testing.T) {
	var nilStore *rawtrace.RawTraceStore

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := nilStore.Insert(ctx, rawtrace.RawHTTPRequest{Source: "fundi"}); err != nil {
		t.Fatalf("nil store Insert: %v", err)
	}
}

// TestAgentRuntimeOptionsRawTraceAllOverridesPerSpawnFlag pins the actual bug:
// RAFIKI_RECORD_REQUESTS=1 (Controller.rawTraceAll) was documented as lifting
// capture for "every session... regardless of header", but agentRuntimeOptions
// only ever consulted req.RecordRequests — a native fundi child spawned
// without --record-requests recorded nothing even with the daemon-wide switch
// on. No DB is needed: rawTrace only needs to be a non-nil sentinel, since
// this test asserts on ro.RawTrace's nil-ness, never calls Insert.
func TestAgentRuntimeOptionsRawTraceAllOverridesPerSpawnFlag(t *testing.T) {
	c := newTestController(t)
	c.rawTrace = &rawtrace.RawTraceStore{}
	c.rawTraceAll = true

	req := protocol.SpawnRequest{
		Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-sonnet-4-5",
		// RecordRequests deliberately left false: rawTraceAll must still win.
	}
	ro, err := c.agentRuntimeOptions(req, "c_rawtraceall", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.RawTrace == nil {
		t.Error("RawTrace = nil, want non-nil: RAFIKI_RECORD_REQUESTS=1 must capture every spawn, not just ones passing --record-requests")
	}
}

// TestAgentRuntimeOptionsRawTraceOffByDefault guards the other side: with
// rawTraceAll false and no per-spawn opt-in, capture must stay off.
func TestAgentRuntimeOptionsRawTraceOffByDefault(t *testing.T) {
	c := newTestController(t)
	c.rawTrace = &rawtrace.RawTraceStore{}
	c.rawTraceAll = false

	req := protocol.SpawnRequest{
		Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-sonnet-4-5",
	}
	ro, err := c.agentRuntimeOptions(req, "c_rawtraceoff", false, "", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.RawTrace != nil {
		t.Error("RawTrace != nil, want nil: neither --record-requests nor RAFIKI_RECORD_REQUESTS is set")
	}
}
