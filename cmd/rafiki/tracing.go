package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/timescale/savannah-common/go/tslogs"
)

var propagator = propagation.TraceContext{}

// setupTracing builds a TracerProvider from the standard OTLP env vars
// (OTEL_EXPORTER_OTLP_ENDPOINT etc.). Unset → no-op provider, zero overhead:
// tracing is opt-in for the --dev rig. The provider is injected (never
// installed globally) — the same contract embedded hosts get.
func setupTracing(ctx context.Context, logger *tslogs.Logger) (trace.TracerProvider, func(), error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return noop.NewTracerProvider(), func() {}, nil
	}
	exp, err := otlptracegrpc.New(ctx) // endpoint/headers/TLS from OTEL_* env
	if err != nil {
		return nil, nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("rafiki")))
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	logger.Info("otlp tracing enabled")
	return tp, func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutCtx)
	}, nil
}

// traceMiddleware honors an inbound W3C traceparent (sentinel and sc emit
// trace context) by extracting it onto the request context, where the proxy
// faces' spans pick it up; absent, spans start a new root.
func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
