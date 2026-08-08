// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// otlpPayload mirrors the OTLP JSON structure enough to extract spans.
type otlpPayload struct {
	ResourceSpans []struct {
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type otlpSpan struct {
	TraceID    string `json:"traceId"`
	SpanID     string `json:"spanId"`
	Name       string `json:"name"`
	StartUnix  string `json:"startTimeUnixNano"`
	EndUnix    string `json:"endTimeUnixNano"`
	Attributes []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string      `json:"stringValue"`
			IntValue    json.Number `json:"intValue,omitempty"`
			DoubleValue float64     `json:"doubleValue"`
		} `json:"value"`
	} `json:"attributes"`
}

// spanAttrs extracts known OTLP attributes from a span into a flat map.
func spanAttrs(sp otlpSpan) map[string]string {
	m := make(map[string]string)
	for _, a := range sp.Attributes {
		v := a.Value.StringValue
		if v == "" {
			v = a.Value.IntValue.String()
		}
		if v == "" && a.Value.DoubleValue != 0 {
			v = fmt.Sprintf("%f", a.Value.DoubleValue)
		}
		m[a.Key] = v
	}
	return m
}

// parseNanos parses an OTLP nanosecond-unix timestamp string, returning a
// zero time on failure (the span is still valid, just missing timing).
func parseNanos(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	var ns int64
	if _, err := fmt.Sscanf(s, "%d", &ns); err != nil {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// HandleOTLP is an HTTP handler that accepts OTLP JSON broadcast traces from
// OpenRouter, extracts per-generation metadata, and inserts them into
// openrouter.broadcast.
//
// It always returns 200 OK to the caller; parse or insert failures are logged
// but not surfaced, to avoid OpenRouter disabling the webhook on transient
// errors (the payload is fire-and-forget from OR's perspective).
func HandleOTLP(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	insertSQL := `INSERT INTO openrouter.broadcast
		(session_id, generation_id, trace_id, span_id, model,
		 input_tokens, output_tokens, cache_read_tokens, cost_usd,
		 latency_ms, provider, finish_reason, created_at, raw_payload)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20)) // 16 MiB
		if err != nil {
			logger.Warn("or_broadcast: read body failed", "error", err)
			return
		}
		if len(body) == 0 {
			logger.Warn("or_broadcast: empty body")
			return
		}

		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			logger.Warn("or_broadcast: parse failed", "error", err)
			return
		}

		ctx := context.Background()
		count := 0
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					if err := insertSpan(ctx, pool, insertSQL, sp, body); err != nil {
						logger.Warn("or_broadcast: insert span failed", "span_id", sp.SpanID, "error", err)
						continue
					}
					count++
				}
			}
		}
		if count > 0 {
			logger.Debug("or_broadcast: stored spans", "count", count)
		}
	}
}

func insertSpan(ctx context.Context, pool *pgxpool.Pool, sql string, sp otlpSpan, rawBody []byte) error {
	attrs := spanAttrs(sp)

	sessionID := attrs["session.id"]
	generationID := firstNonEmpty(
		attrs["gen_ai.generation.id"],
		attrs["generation.id"],
	)
	model := attrs["gen_ai.request.model"]
	provider := attrs["gen_ai.response.provider"]

	inputTokens := parseIntOr(attrs["gen_ai.usage.input_tokens"], 0)
	outputTokens := parseIntOr(attrs["gen_ai.usage.output_tokens"], 0)
	cacheReadTokens := parseIntOr(attrs["gen_ai.usage.cache_read_input_tokens"], 0)

	costUSD := parseFloatOr(attrs["gen_ai.usage.cost"], 0)

	createdAt := parseNanos(sp.StartUnix)
	latencyMS := 0
	if sp.StartUnix != "" && sp.EndUnix != "" {
		start := parseNanos(sp.StartUnix)
		end := parseNanos(sp.EndUnix)
		if !start.IsZero() && !end.IsZero() {
			latencyMS = int(end.Sub(start).Milliseconds())
		}
	}

	finishReason := firstNonEmpty(
		attrs["gen_ai.response.finish_reason"],
		attrs["gen_ai.response.stop_reason"],
	)

	_, err := pool.Exec(ctx, sql,
		nullStr(sessionID),
		nullStr(generationID),
		nullStr(sp.TraceID),
		nullStr(sp.SpanID),
		nullStr(model),
		nullInt(inputTokens),
		nullInt(outputTokens),
		nullInt(cacheReadTokens),
		nullFloat(costUSD),
		nullInt(int64(latencyMS)),
		nullStr(provider),
		nullStr(finishReason),
		createdAt,
		rawBody,
	)
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseIntOr(s string, def int64) int64 {
	if s == "" {
		return def
	}
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return def
	}
	return v
}

func parseFloatOr(s string, def float64) float64 {
	if s == "" {
		return def
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return def
	}
	return v
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func nullFloat(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}
