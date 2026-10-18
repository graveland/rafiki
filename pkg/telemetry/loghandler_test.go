// SPDX-License-Identifier: Apache-2.0

package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/telemetry"
)

// fixedSpanContext is a valid SpanContext with stable identifiers, so the test
// can assert the exact strings the handler must emit.
func fixedSpanContext() trace.SpanContext {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		panic(err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		panic(err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
}

// decodeOne parses the single JSON record the wrapped handler wrote.
func decodeOne(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	return m
}

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	sc := fixedSpanContext()

	t.Run("with a span in the context", func(t *testing.T) {
		c := assert.NewCollecting(t)
		var buf bytes.Buffer
		logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))

		ctx := trace.ContextWithSpanContext(context.Background(), sc)
		logger.InfoContext(ctx, "hello")

		m := decodeOne(t, &buf)
		c.Eq(sc.TraceID().String(), m["trace_id"].(string))
		c.Eq(sc.SpanID().String(), m["span_id"].(string))
	})

	t.Run("without a span in the context", func(t *testing.T) {
		c := assert.NewCollecting(t)
		var buf bytes.Buffer
		logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)))

		logger.InfoContext(context.Background(), "hello")

		m := decodeOne(t, &buf)
		c.NotHasKey(m, "trace_id")
		c.NotHasKey(m, "span_id")
	})

	t.Run("through With", func(t *testing.T) {
		c := assert.NewCollecting(t)
		var buf bytes.Buffer
		logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil))).With("k", "v")

		ctx := trace.ContextWithSpanContext(context.Background(), sc)
		logger.InfoContext(ctx, "hello")

		m := decodeOne(t, &buf)
		c.Eq("v", m["k"].(string))
		c.Eq(sc.TraceID().String(), m["trace_id"].(string))
		c.Eq(sc.SpanID().String(), m["span_id"].(string))
	})

	t.Run("through WithGroup", func(t *testing.T) {
		c := assert.NewCollecting(t)
		var buf bytes.Buffer
		h := telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil)).WithGroup("g")
		logger := slog.New(h)

		ctx := trace.ContextWithSpanContext(context.Background(), sc)
		logger.InfoContext(ctx, "hello")

		// The identifiers are added to the record, so an enclosing group
		// carries them: that is what delegating to the inner handler means.
		m := decodeOne(t, &buf)
		group, ok := m["g"].(map[string]any)
		c.True(ok, "record must be grouped under g, got %T", m["g"])
		c.Eq(sc.TraceID().String(), group["trace_id"].(string))
		c.Eq(sc.SpanID().String(), group["span_id"].(string))
	})
}
