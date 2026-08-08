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
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// setupBroadcastTable creates the openrouter.broadcast table directly
// (migration 0009 runs as part of the store chain, but a standalone unit
// test just needs the one table to exist) and registers cleanup.
func setupBroadcastTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		CREATE SCHEMA IF NOT EXISTS openrouter;
		CREATE TABLE IF NOT EXISTS openrouter.broadcast (
			id              BIGINT GENERATED ALWAYS AS IDENTITY,
			session_id      TEXT,
			generation_id   TEXT,
			trace_id        TEXT,
			span_id         TEXT,
			model           TEXT,
			input_tokens    BIGINT,
			output_tokens   BIGINT,
			cache_read_tokens  BIGINT,
			cost_usd        DOUBLE PRECISION,
			latency_ms      INT,
			provider        TEXT,
			finish_reason   TEXT,
			created_at      TIMESTAMPTZ NOT NULL,
			received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
			raw_payload     JSONB NOT NULL,
			PRIMARY KEY (id, created_at)
		)
	`); err != nil {
		t.Fatalf("create broadcast table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS openrouter.broadcast`)
	})
}

// TestHandleOTLP_RoundTrip sends an OTLP payload with known attributes and
// verifies the row lands in openrouter.broadcast correctly.
func TestHandleOTLP_RoundTrip(t *testing.T) {
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
									{Key: "gen_ai.generation.id", Value: struct {
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
									{Key: "gen_ai.usage.cost", Value: struct {
										StringValue string      `json:"stringValue"`
										IntValue    json.Number `json:"intValue,omitempty"`
										DoubleValue float64     `json:"doubleValue"`
									}{DoubleValue: 0.00042}},
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
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	// Verify the row was inserted correctly.
	var (
		sessionID    string
		generationID string
		traceID      string
		spanID       string
		model        string
		provider     string
		inputTokens  int64
		outputTokens int64
		costUSD      float64
		latency      int
		finishReason string
	)
	err = pool.QueryRow(context.Background(),
		`SELECT session_id, generation_id, trace_id, span_id, model, provider,
		        input_tokens, output_tokens, cost_usd, latency_ms, finish_reason
		 FROM openrouter.broadcast
		 WHERE span_id = 'def456span'`).Scan(
		&sessionID, &generationID, &traceID, &spanID, &model, &provider,
		&inputTokens, &outputTokens, &costUSD, &latency, &finishReason)
	if err != nil {
		t.Fatalf("query inserted row: %v", err)
	}

	if sessionID != "conv-uuid-123" {
		t.Errorf("session_id = %q, want conv-uuid-123", sessionID)
	}
	if generationID != "gen-test-999" {
		t.Errorf("generation_id = %q, want gen-test-999", generationID)
	}
	if traceID != "abc123trace" {
		t.Errorf("trace_id = %q, want abc123trace", traceID)
	}
	if spanID != "def456span" {
		t.Errorf("span_id = %q, want def456span", spanID)
	}
	if model != "deepseek/deepseek-v4-pro" {
		t.Errorf("model = %q, want deepseek/deepseek-v4-pro", model)
	}
	if provider != "DeepInfra" {
		t.Errorf("provider = %q, want DeepInfra", provider)
	}
	if inputTokens != 150 {
		t.Errorf("input_tokens = %d, want 150", inputTokens)
	}
	if outputTokens != 50 {
		t.Errorf("output_tokens = %d, want 50", outputTokens)
	}
	if costUSD != 0.00042 {
		t.Errorf("cost_usd = %f, want 0.00042", costUSD)
	}
	// 1.5 seconds in nanoseconds = 1500ms
	if latency != 1500 {
		t.Errorf("latency_ms = %d, want 1500", latency)
	}
	if finishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", finishReason)
	}
}

// TestHandleOTLP_MissingTimestampFallsBackToNow verifies that a span missing
// startTimeUnixNano gets created_at set to roughly "now" rather than the zero
// time.Time (0001-01-01), which would poison the hypertable's time
// partitioning and retention policy.
func TestHandleOTLP_MissingTimestampFallsBackToNow(t *testing.T) {
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

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var createdAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT created_at FROM openrouter.broadcast WHERE span_id = 'ts-fallback-span'`,
	).Scan(&createdAt); err != nil {
		t.Fatalf("query inserted row: %v", err)
	}

	if createdAt.Before(before) || createdAt.After(after) {
		t.Errorf("created_at = %v, want between %v and %v (fallback to now, not zero time)", createdAt, before, after)
	}
	if createdAt.Year() < 2000 {
		t.Errorf("created_at = %v, looks like the zero time.Time, not a fallback to now", createdAt)
	}
}

// TestHandleOTLP_RawPayloadIsPerSpanNotWholeBatch verifies that raw_payload
// stores only the individual span's own JSON, not the entire request body —
// a batch of N spans must not store N duplicate copies of the same
// multi-span blob.
func TestHandleOTLP_RawPayloadIsPerSpanNotWholeBatch(t *testing.T) {
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

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	checkRow := func(spanID, wantSessionID, otherSpanID string) {
		t.Helper()
		var rawPayload string
		if err := pool.QueryRow(context.Background(),
			`SELECT raw_payload::text FROM openrouter.broadcast WHERE span_id = $1`, spanID,
		).Scan(&rawPayload); err != nil {
			t.Fatalf("query raw_payload for %s: %v", spanID, err)
		}

		var decoded otlpSpan
		if err := json.Unmarshal([]byte(rawPayload), &decoded); err != nil {
			t.Fatalf("raw_payload for %s is not a single otlpSpan: %v (payload: %s)", spanID, err, rawPayload)
		}
		if decoded.SpanID != spanID {
			t.Errorf("raw_payload spanId = %q, want %q", decoded.SpanID, spanID)
		}
		if strings.Contains(rawPayload, otherSpanID) {
			t.Errorf("raw_payload for %s contains %s — storing the whole batch body, not the individual span", spanID, otherSpanID)
		}
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
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestHandleOTLP_NoSpans verifies no-op on valid OTLP with zero spans.
func TestHandleOTLP_NoSpans(t *testing.T) {
	handler := HandleOTLP(nil, testBroadcastLogger())
	body := []byte(`{"resourceSpans":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
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
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testBroadcastLogger returns a discarded logger for test handlers.
func testBroadcastLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
