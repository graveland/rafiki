// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// otlpAttr is a convenience constructor for otlpSpan.Attributes elements
// (Go treats identical anonymous struct definitions as the same type, so
// this matches the field type inline in otlpSpan without duplicating it).
func otlpAttr(key, strVal string) struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string      `json:"stringValue"`
		IntValue    json.Number `json:"intValue,omitempty"`
		DoubleValue float64     `json:"doubleValue"`
	} `json:"value"`
} {
	return struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string      `json:"stringValue"`
			IntValue    json.Number `json:"intValue,omitempty"`
			DoubleValue float64     `json:"doubleValue"`
		} `json:"value"`
	}{
		Key: key,
		Value: struct {
			StringValue string      `json:"stringValue"`
			IntValue    json.Number `json:"intValue,omitempty"`
			DoubleValue float64     `json:"doubleValue"`
		}{StringValue: strVal},
	}
}

// otlpBody wraps spans into a single-resource, single-scope OTLP JSON body.
func otlpBody(t *testing.T, spans ...otlpSpan) []byte {
	t.Helper()
	payload := otlpPayload{
		ResourceSpans: []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		}{
			{
				ScopeSpans: []struct {
					Spans []otlpSpan `json:"spans"`
				}{
					{Spans: spans},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	assert.NewAborting(t).NoError(err, "marshal payload")
	return body
}

// setupBroadcastTable creates the openrouter.broadcast table directly
// (migration 0009 runs as part of the store chain, but a standalone unit
// test just needs the one table to exist) and registers cleanup.
func setupBroadcastTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		CREATE SCHEMA IF NOT EXISTS openrouter;
		CREATE TABLE IF NOT EXISTS openrouter.broadcast (
			id               BIGINT GENERATED ALWAYS AS IDENTITY,
			session_id       TEXT,
			generation_id    TEXT,
			trace_id         TEXT,
			span_id          TEXT,
			model            TEXT,
			input_tokens     BIGINT,
			output_tokens    BIGINT,
			cache_read_tokens   BIGINT,
			cost_usd         DOUBLE PRECISION,
			latency_ms       INT,
			provider         TEXT,
			finish_reason    TEXT,
			created_at       TIMESTAMPTZ NOT NULL,
			received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
			raw_payload      JSONB NOT NULL,
			total_tokens     BIGINT,
			reasoning_tokens BIGINT,
			input_cost_usd   DOUBLE PRECISION,
			output_cost_usd  DOUBLE PRECISION,
			PRIMARY KEY (id, created_at)
		)
	`)
	assert.NewAborting(t).NoError(err, "create broadcast table")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS openrouter.broadcast`)
	})
}

// TestHandleOTLP_RoundTrip sends an OTLP payload with known attributes and
// verifies the row lands in openrouter.broadcast correctly.
func TestHandleOTLP_RoundTrip(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testBroadcastPool(t)
	if pool == nil {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	setupBroadcastTable(t, pool)

	handler := HandleOTLP(pool, testBroadcastLogger())

	// Build a payload with known values using the named types.
	payload := otlpPayload{
		ResourceSpans: []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		}{
			{
				ScopeSpans: []struct {
					Spans []otlpSpan `json:"spans"`
				}{
					{
						Spans: []otlpSpan{
							{
								TraceID:   "abc123trace",
								SpanID:    "def456span",
								Name:      "chat",
								StartUnix: "1705312800000000000",
								EndUnix:   "1705312801500000000",
								Attributes: []struct {
									Key   string `json:"key"`
									Value struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									} `json:"value"`
								}{
									{Key: "session.id", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{StringValue: "conv-uuid-123"}},
									{Key: "gen_ai.response.id", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{StringValue: "gen-test-999"}},
									{Key: "gen_ai.request.model", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{StringValue: "deepseek/deepseek-v4-pro"}},
									{Key: "gen_ai.response.provider", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{StringValue: "DeepInfra"}},
									{Key: "gen_ai.usage.input_tokens", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{IntValue: "150"}},
									{Key: "gen_ai.usage.output_tokens", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{IntValue: "50"}},
									{Key: "gen_ai.usage.total_cost", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{DoubleValue: 0.00042}},
									{Key: "gen_ai.usage.input_cost", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{DoubleValue: 0.00036}},
									{Key: "gen_ai.usage.output_cost", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{DoubleValue: 0.00006}},
									{Key: "gen_ai.usage.total_tokens", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{IntValue: "200"}},
									{Key: "gen_ai.usage.output_tokens.reasoning", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{IntValue: "30"}},
									{Key: "gen_ai.usage.input_tokens.cached", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{IntValue: "100"}},
									{Key: "gen_ai.response.finish_reason", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{StringValue: "stop"}},
								},
							},
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	c.Require().NoError(err, "marshal payload")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	c.Eq(http.StatusOK, rec.Code, "status")

	// Verify the row was inserted correctly.
	var (
		sessionID       string
		generationID    string
		traceID         string
		spanID          string
		model           string
		provider        string
		inputTokens     int64
		outputTokens    int64
		cacheReadTokens int64
		costUSD         float64
		latency         int
		finishReason    string
		totalTokens     int64
		reasoningTokens int64
		inputCostUSD    float64
		outputCostUSD   float64
	)
	err = pool.QueryRow(context.Background(),
		`SELECT session_id, generation_id, trace_id, span_id, model, provider,
		        input_tokens, output_tokens, cache_read_tokens, cost_usd, latency_ms, finish_reason,
		        total_tokens, reasoning_tokens, input_cost_usd, output_cost_usd
		 FROM openrouter.broadcast
		 WHERE span_id = 'def456span'`).Scan(
		&sessionID, &generationID, &traceID, &spanID, &model, &provider,
		&inputTokens, &outputTokens, &cacheReadTokens, &costUSD, &latency, &finishReason,
		&totalTokens, &reasoningTokens, &inputCostUSD, &outputCostUSD)
	c.Require().NoError(err, "query inserted row")

	c.Eq("conv-uuid-123", sessionID, "session_id")
	c.Eq("gen-test-999", generationID, "generation_id")
	c.Eq("abc123trace", traceID, "trace_id")
	c.Eq("def456span", spanID, "span_id")
	c.Eq("deepseek/deepseek-v4-pro", model, "model")
	c.Eq("DeepInfra", provider, "provider")
	c.Eq(150, inputTokens, "input_tokens")
	c.Eq(50, outputTokens, "output_tokens")
	c.Eq(100, cacheReadTokens, "cache_read_tokens")
	c.Eq(0.00042, costUSD, "cost_usd")
	c.Eq(200, totalTokens, "total_tokens")
	c.Eq(30, reasoningTokens, "reasoning_tokens")
	c.Eq(0.00036, inputCostUSD, "input_cost_usd")
	c.Eq(0.00006, outputCostUSD, "output_cost_usd")
	// 1.5 seconds in nanoseconds = 1500ms
	c.Eq(1500, latency, "latency_ms")
	c.Eq("stop", finishReason, "finish_reason")
}

// TestHandleOTLP_MissingTimestampFallsBackToNow verifies that a span missing
// startTimeUnixNano gets created_at set to roughly "now" rather than the zero
// time.Time (0001-01-01), which would poison the hypertable's time
// partitioning and retention policy.
func TestHandleOTLP_MissingTimestampFallsBackToNow(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testBroadcastPool(t)
	if pool == nil {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	setupBroadcastTable(t, pool)

	handler := HandleOTLP(pool, testBroadcastLogger())

	sp := otlpSpan{
		TraceID: "ts-fallback-trace",
		SpanID:  "ts-fallback-span",
		Name:    "chat",
		// StartUnix and EndUnix deliberately omitted.
		Attributes: []struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string      `json:"stringValue"`
				IntValue    json.Number `json:"intValue,omitempty"`
				DoubleValue float64     `json:"doubleValue"`
			} `json:"value"`
		}{otlpAttr("session.id", "ts-fallback-conv")},
	}

	before := time.Now().Add(-5 * time.Second)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(otlpBody(t, sp)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	after := time.Now().Add(5 * time.Second)

	c.Require().Eq(http.StatusOK, rec.Code, "status")

	var createdAt time.Time
	c.Require().NoError(pool.QueryRow(context.Background(),
		`SELECT created_at FROM openrouter.broadcast WHERE span_id = 'ts-fallback-span'`,
	).Scan(&createdAt), "query inserted row")

	c.False(createdAt.Before(before) || createdAt.After(after), "created_at = %v, want between %v and %v (fallback to now, not zero time)", createdAt, before, after)
	c.GreaterOrEqual(2000, createdAt.Year(), "created_at = %v, looks like the zero time.Time, not a fallback to now", createdAt)
}

// TestHandleOTLP_RawPayloadIsPerSpanNotWholeBatch verifies that raw_payload
// stores only the individual span's own JSON, not the entire request body —
// a batch of N spans must not store N duplicate copies of the same
// multi-span blob.
func TestHandleOTLP_RawPayloadIsPerSpanNotWholeBatch(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testBroadcastPool(t)
	if pool == nil {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	setupBroadcastTable(t, pool)

	handler := HandleOTLP(pool, testBroadcastLogger())

	attrsFor := func(sessionID string) []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string      `json:"stringValue"`
			IntValue    json.Number `json:"intValue,omitempty"`
			DoubleValue float64     `json:"doubleValue"`
		} `json:"value"`
	} {
		return []struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string      `json:"stringValue"`
				IntValue    json.Number `json:"intValue,omitempty"`
				DoubleValue float64     `json:"doubleValue"`
			} `json:"value"`
		}{otlpAttr("session.id", sessionID)}
	}

	spanA := otlpSpan{TraceID: "batch-trace", SpanID: "batch-span-a", Name: "chat-a", Attributes: attrsFor("batch-conv-a")}
	spanB := otlpSpan{TraceID: "batch-trace", SpanID: "batch-span-b", Name: "chat-b", Attributes: attrsFor("batch-conv-b")}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(otlpBody(t, spanA, spanB)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status")

	checkRow := func(spanID, wantSessionID, otherSpanID string) {
		t.Helper()
		var rawPayload string
		c.Require().NoError(pool.QueryRow(context.Background(),
			`SELECT raw_payload::text FROM openrouter.broadcast WHERE span_id = $1`, spanID,
		).Scan(&rawPayload), "query raw_payload for %s", spanID)

		var decoded otlpSpan
		err := json.Unmarshal([]byte(rawPayload), &decoded)
		c.Require().NoError(err, "raw_payload for %s is not a single otlpSpan: %v (payload: %s)", spanID, err, rawPayload)
		c.Eq(spanID, decoded.SpanID, "raw_payload spanId")
		c.NotStrContains(rawPayload, otherSpanID, "raw_payload for %s contains %s — storing the whole batch body, not the individual span", spanID, otherSpanID)
	}

	checkRow("batch-span-a", "batch-conv-a", "batch-span-b")
	checkRow("batch-span-b", "batch-conv-b", "batch-span-a")
}

// TestHandleOTLP_EmptyBody verifies the handler does not panic on empty body.
func TestHandleOTLP_EmptyBody(t *testing.T) {
	handler := HandleOTLP(nil, testBroadcastLogger())
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte{}))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	// Must not panic, must return 200.
	handler.ServeHTTP(rec, req)
	assert.NewCollecting(t).Eq(http.StatusOK, rec.Code, "status")
}

// TestHandleOTLP_NoSpans verifies no-op on valid OTLP with zero spans.
func TestHandleOTLP_NoSpans(t *testing.T) {
	handler := HandleOTLP(nil, testBroadcastLogger())
	body := []byte(`{"resourceSpans":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.NewCollecting(t).Eq(http.StatusOK, rec.Code, "status")
}

// testBroadcastPool returns a pgxpool from RAFIKI_TEST_DSN, or nil if not set.
func testBroadcastPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	assert.NewAborting(t).NoError(err, "pgxpool")
	t.Cleanup(pool.Close)
	return pool
}

// testBroadcastLogger returns a discarded logger for test handlers.
func testBroadcastLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSpanLogCounterCoalescesPerWindow pins the coalescing contract: spans
// recorded inside one window emit nothing; the first record past the window
// flushes exactly the count stored INSIDE that window, and belongs to the
// window it opens; a zero-count record never opens a window but still
// flushes an expired one. No database — the counter is pure, and its clock
// is caller-supplied so the test never sleeps.
func TestSpanLogCounterCoalescesPerWindow(t *testing.T) {
	ck := assert.NewAborting(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	base := time.Unix(1_700_000_000, 0)
	c := &spanLogCounter{}

	c.record(3, base, logger)
	c.record(4, base.Add(30*time.Second), logger)
	ck.Eq(0, buf.Len(), "records inside one window logged %q; want silence", buf.String())

	c.record(5, base.Add(broadcastLogWindow+time.Second), logger)
	if out := buf.String(); !strings.Contains(out, "or_broadcast: stored spans") || !strings.Contains(out, "count=7") {
		t.Fatalf("flush line = %q; want stored spans with count=7 (3+4, not the triggering 5)", out)
	}

	// The triggering record opened a fresh window carrying its own 5; the
	// next record joins it, and both flush together when that window closes.
	buf.Reset()
	c.record(1, base.Add(broadcastLogWindow+2*time.Second), logger)
	ck.Eq(0, buf.Len(), "record into the reopened window logged %q; want silence", buf.String())
	c.record(1, base.Add(2*broadcastLogWindow+2*time.Second), logger)
	ck.StrContains(buf.String(), "count=6", "second flush")

	// A zero-count record must not open a window (a no-op request would
	// otherwise start the clock for spans it never stored), but must still
	// flush an expired one.
	buf.Reset()
	c2 := &spanLogCounter{}
	c2.record(0, base, logger)
	c2.record(2, base.Add(90*time.Second), logger)
	ck.Eq(0, buf.Len(), "records after a zero-count opener logged %q; want silence", buf.String())
	c2.record(0, base.Add(150*time.Second), logger)
	ck.StrContains(buf.String(), "count=2", "zero-record flush")
}
