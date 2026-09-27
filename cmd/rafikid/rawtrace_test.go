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

	"github.com/multigres/testkit/assert"
)

// openTestPool opens a pool to RAFIKI_TEST_DSN or skips the test.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "open pool")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "migrate")
	return pool
}

// TestRawTrace_Insert verifies that RawTraceStore.Insert writes a row and the
// row can be queried back. Requires RAFIKI_TEST_DSN.
func TestRawTrace_Insert(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := openTestPool(t)

	// Clean any leftover rows from prior runs.
	if _, err := pool.Exec(t.Context(), `DELETE FROM conversations.raw_http_request`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	store := rawtrace.NewRawTraceStore(pool)
	c.Require().NotNil(store, "NewRawTraceStore returned nil")

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

	c.Require().NoError(store.Insert(ctx, r), "Insert")

	// Query back the row.
	var count int
	err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM conversations.raw_http_request
		 WHERE source='proxy' AND model='claude-sonnet-4-5'`).Scan(&count)
	c.Require().NoError(err, "query")
	c.Require().Eq(1, count, "expected 1 row, got")

	// Verify column values.
	var gotModel, gotSource, gotReqMethod string
	var gotLatency int
	err = pool.QueryRow(t.Context(),
		`SELECT model, source, req_method, latency_ms
		 FROM conversations.raw_http_request
		 WHERE source='proxy' LIMIT 1`).Scan(&gotModel, &gotSource, &gotReqMethod, &gotLatency)
	c.Require().NoError(err, "query columns")
	c.Eq("claude-sonnet-4-5", gotModel, "model: got")
	c.Eq("proxy", gotSource, "source: got")
	c.Eq("POST", gotReqMethod, "req_method: got")
	c.Eq(42, gotLatency, "latency_ms: got")
}

// TestRawTrace_InsertNilStore verifies that Insert on a nil store is a no-op.
func TestRawTrace_InsertNilStore(t *testing.T) {
	var nilStore *rawtrace.RawTraceStore

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	assert.NewAborting(t).NoError(nilStore.Insert(ctx, rawtrace.RawHTTPRequest{Source: "fundi"}), "nil store Insert")
}

// TestAgentRuntimeOptionsRawTraceAllOverridesPerSpawnFlag pins the actual bug:
// RAFIKI_RECORD_REQUESTS=1 (Controller.rawTraceAll) was documented as lifting
// capture for "every session... regardless of header", but agentRuntimeOptions
// only ever consulted req.RecordRequests — a native fundi child spawned
// without --record-requests recorded nothing even with the daemon-wide switch
// on. No DB is needed: rawTrace only needs to be a non-nil sentinel, since
// this test asserts on ro.RawTrace's nil-ness, never calls Insert.
func TestAgentRuntimeOptionsRawTraceAllOverridesPerSpawnFlag(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	c.rawTrace = &rawtrace.RawTraceStore{}
	c.rawTraceAll = true

	req := protocol.SpawnRequest{
		Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-sonnet-4-5",
		// RecordRequests deliberately left false: rawTraceAll must still win.
	}
	ro, err := c.agentRuntimeOptions(req, "c_rawtraceall", false, "", "")
	ck.Require().NoError(err, "agentRuntimeOptions")
	ck.NotNil(ro.RawTrace, "RawTrace = nil, want non-nil: RAFIKI_RECORD_REQUESTS=1 must capture every spawn, not just ones passing --record-requests")
}

// TestAgentRuntimeOptionsRawTraceOffByDefault guards the other side: with
// rawTraceAll false and no per-spawn opt-in, capture must stay off.
func TestAgentRuntimeOptionsRawTraceOffByDefault(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := newTestController(t)
	c.rawTrace = &rawtrace.RawTraceStore{}
	c.rawTraceAll = false

	req := protocol.SpawnRequest{
		Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-sonnet-4-5",
	}
	ro, err := c.agentRuntimeOptions(req, "c_rawtraceoff", false, "", "")
	ck.Require().NoError(err, "agentRuntimeOptions")
	ck.Nil(ro.RawTrace, "RawTrace != nil, want nil: neither --record-requests nor RAFIKI_RECORD_REQUESTS is set")
}
