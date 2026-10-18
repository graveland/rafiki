// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"
)

// pgSpanKey marks a context in which this tracer delegated TraceQueryStart, so
// TraceQueryEnd only finishes spans this tracer actually started.
type pgSpanKey struct{}

// parentOnlyQueryTracer starts a query span only inside an existing span, so
// background work (the indexer, sweeps) never produces root traces. Query
// parameters are never recorded.
type parentOnlyQueryTracer struct{ inner pgx.QueryTracer }

// newPGTracer wraps otelpgx in the parent-only gate. otelpgx records query
// parameters only when WithIncludeQueryParameters is passed, and it never is
// here: prompt and tool-argument text must not leave the daemon on a postgres
// span.
func newPGTracer(tp trace.TracerProvider) pgx.QueryTracer {
	return &parentOnlyQueryTracer{inner: otelpgx.NewTracer(otelpgx.WithTracerProvider(tp))}
}

// TraceQueryStart delegates only when the context already carries a valid span.
func (t *parentOnlyQueryTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	return context.WithValue(t.inner.TraceQueryStart(ctx, conn, data), pgSpanKey{}, struct{}{})
}

// TraceQueryEnd delegates only for a context TraceQueryStart marked.
func (t *parentOnlyQueryTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if _, ok := ctx.Value(pgSpanKey{}).(struct{}); !ok {
		return
	}
	t.inner.TraceQueryEnd(ctx, conn, data)
}
