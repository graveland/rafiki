// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/multigres/testkit/assert"
)

func TestParentOnlyQueryTracer(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(recorder)
	tracer := newPGTracer(tp)

	data := pgx.TraceQueryStartData{SQL: "SELECT 1", Args: []any{"secret-argument"}}
	end := pgx.TraceQueryEndData{}

	t.Run("no parent starts no span", func(t *testing.T) {
		ctx := tracer.TraceQueryStart(context.Background(), nil, data)
		tracer.TraceQueryEnd(ctx, nil, end)
		assert.NewCollecting(t).Eq(0, len(recorder.Started()), "spans started without a parent")
	})

	t.Run("inside a parent starts a child", func(t *testing.T) {
		ctx, parent := tp.Tracer("test").Start(context.Background(), "parent")
		ctx = tracer.TraceQueryStart(ctx, nil, data)
		tracer.TraceQueryEnd(ctx, nil, end)
		parent.End()

		var child sdktrace.ReadOnlySpan
		for _, s := range recorder.Ended() {
			if s.Name() != "parent" {
				child = s
			}
		}
		if child == nil {
			t.Fatalf("no query span recorded; ended: %d", len(recorder.Ended()))
		}
		c := assert.NewCollecting(t)
		c.Eq(parent.SpanContext().TraceID().String(), child.Parent().TraceID().String(), "query span parent trace id")
		c.Eq(parent.SpanContext().SpanID().String(), child.Parent().SpanID().String(), "query span parent span id")

		for _, attr := range child.Attributes() {
			k := strings.ToLower(string(attr.Key))
			if strings.Contains(k, "args") || strings.Contains(k, "parameters") {
				t.Errorf("query span records parameters in attribute %q", attr.Key)
			}
		}
	})
}
