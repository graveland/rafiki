// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogHandler wraps h so every record logged inside a valid span carries the
// span's trace_id and span_id, which is what makes a log line joinable to the
// trace it belongs to.
func NewLogHandler(h slog.Handler) slog.Handler {
	return &logHandler{inner: h}
}

// logHandler adds the trace identifiers and otherwise delegates unchanged.
// WithAttrs and WithGroup re-wrap, so the identifiers are added for a
// with-attrs or grouped handler too.
type logHandler struct {
	inner slog.Handler
}

// Enabled delegates to the wrapped handler.
func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle stamps the record with the context's trace identifiers, if any.
func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs delegates, keeping the wrapper.
func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup delegates, keeping the wrapper.
func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{inner: h.inner.WithGroup(name)}
}
