// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.graveland.dev/rafiki/pkg/telemetry"
	"go.graveland.dev/rafiki/pkg/version"
)

var propagator = propagation.TraceContext{}

// setupTracing builds a TracerProvider from rafikid.toml's [telemetry] section
// and the standard OTLP env vars. Unset → no-op provider, zero overhead:
// tracing is opt-in. The provider is injected (never installed globally) — the
// same contract embedded hosts get.
func setupTracing(ctx context.Context, cfg telemetry.Config, daemonID string) (trace.TracerProvider, func(), error) {
	enabled, protocol, endpoint, err := resolveOTLP(os.Getenv, cfg)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return noop.NewTracerProvider(), func() {}, nil
	}

	var exp sdktrace.SpanExporter
	switch protocol {
	case "grpc":
		var opts []otlptracegrpc.Option
		if endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(endpoint))
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	case "http/protobuf":
		var opts []otlptracehttp.Option
		if endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
		}
		exp, err = otlptracehttp.New(ctx, opts...)
	default:
		return nil, nil, fmt.Errorf("otlp protocol %q: want \"http/protobuf\" or \"grpc\"", protocol)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("otlp exporter: %w", err)
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName("rafikid"),
		semconv.ServiceVersion(version.String()),
	}
	if daemonID != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(daemonID))
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	slog.Info("otlp tracing enabled", "protocol", protocol)
	return tp, func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutCtx)
	}, nil
}

// resolveOTLP decides whether tracing is on, and how to reach the collector.
//
// Precedence, first non-empty wins: for the protocol,
// OTEL_EXPORTER_OTLP_TRACES_PROTOCOL > OTEL_EXPORTER_OTLP_PROTOCOL >
// [telemetry] protocol > "http/protobuf". For the endpoint, either env var
// (TRACES first) beats the config file; when an env var names it, the endpoint
// is returned EMPTY so the exporter reads the environment itself.
func resolveOTLP(getenv func(string) string, cfg telemetry.Config) (enabled bool, protocol, endpoint string, err error) {
	envEndpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	enabled = envEndpoint || cfg.Endpoint != ""
	if !envEndpoint {
		endpoint = cfg.Endpoint
	}

	protocol = firstNonEmpty(
		getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"),
		getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
		cfg.Protocol,
		"http/protobuf",
	)
	switch protocol {
	case "http/protobuf", "grpc":
		return enabled, protocol, endpoint, nil
	default:
		return false, "", "", fmt.Errorf("otlp protocol %q: want \"http/protobuf\" or \"grpc\"", protocol)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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
