// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RawTraceStore writes debug HTTP request/response records to the
// conversations.raw_http_request table. A nil *RawTraceStore means
// disabled — callers check for nil before calling Insert.
type RawTraceStore struct{ pool *pgxpool.Pool }

// NewRawTraceStore returns a store backed by pool. Returns nil when pool is nil
// so callers can unconditionally call NewRawTraceStore(pool) and the result is
// already nil-safe.
func NewRawTraceStore(pool *pgxpool.Pool) *RawTraceStore {
	if pool == nil {
		return nil
	}
	return &RawTraceStore{pool: pool}
}

// RawHTTPRequest is one recorded LLM API call.
type RawHTTPRequest struct {
	ConversationID *string // nullable UUID
	TurnID         *string // nullable UUID
	Model          string
	Upstream       string // "anthropic" | "openrouter"
	Source         string // "proxy" | "fundi"
	ReqMethod      string
	ReqPath        string
	ReqHeaders     json.RawMessage // marshal-safe, nil-ok
	ReqBody        json.RawMessage // marshal-safe, nil-ok
	RespStatus     *int            // nil when no response arrived
	RespHeaders    json.RawMessage
	RespBody       json.RawMessage
	LatencyMS      int
	Error          string
}

// Insert writes one row. Best-effort: failures are logged, never returned.
// ctx is the caller's context; a short deadline is advised so a slow insert
// cannot block the hot path. Insert is a no-op when s is nil.
func (s *RawTraceStore) Insert(ctx context.Context, r RawHTTPRequest) error {
	if s == nil {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO conversations.raw_http_request
			(conversation_id, turn_id, model, upstream, source,
			 req_method, req_path, req_headers, req_body,
			 resp_status, resp_headers, resp_body,
			 latency_ms, error)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		nilUUID(r.ConversationID), nilUUID(r.TurnID),
		nullStr(r.Model), nullStr(r.Upstream), r.Source,
		r.ReqMethod, r.ReqPath,
		nilJSON(r.ReqHeaders), nilJSON(r.ReqBody),
		r.RespStatus,
		nilJSON(r.RespHeaders), nilJSON(r.RespBody),
		r.LatencyMS, nullStr(r.Error),
	)
	if err != nil {
		slog.Warn("raw trace: insert failed", "error", err)
	}
	return nil // best-effort
}

func nilUUID(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nilJSON prepares a raw byte payload for insertion into a JSONB column.
// Valid JSON (the common case: headers, and JSON-bodied requests/responses)
// passes through unchanged. Non-JSON payloads — an SSE stream body, an HTML
// error page from a load balancer — are wrapped as a JSON string so the
// INSERT can never fail with "invalid input syntax for type json" and the
// capture is still stored, queryable, and byte-for-byte recoverable. Empty
// input maps to SQL NULL.
func nilJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return b
	}
	wrapped, err := json.Marshal(string(b))
	if err != nil {
		// json.Marshal of a string only fails for invalid UTF-8, which string(b)
		// cannot produce (it replaces invalid sequences) — unreachable in practice.
		slog.Warn("raw trace: wrapping non-JSON payload failed", "error", err)
		return nil
	}
	return wrapped
}
