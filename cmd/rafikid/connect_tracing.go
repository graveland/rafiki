// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel/trace"
)

// newConnectTracing builds the interceptor that traces Connect RPCs on the
// Control service. It returns nil, nil for a nil provider, which is how the
// daemon runs with tracing off: connectControlRoute skips a nil interceptor.
//
// Only UNARY procedures are instrumented. StreamEvents and the other streaming
// RPCs have lifetimes owned by their handlers (a parked stream can last the
// session), so a span wrapped around one would end when the client goes away,
// not when the work does — and the daemon already traces what those streams
// carry from the event pipeline. The filter excludes everything whose spec
// says it is not a unary call.
//
// WithoutMetrics: the daemon's own *server.Metrics is the single Prometheus
// surface, so otelconnect must not register a second family of rpc.* metrics.
func newConnectTracing(tp trace.TracerProvider) (connect.Interceptor, error) {
	if tp == nil {
		return nil, nil
	}
	return otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tp),
		otelconnect.WithoutMetrics(),
		otelconnect.WithFilter(func(_ context.Context, spec connect.Spec) bool {
			return spec.StreamType == connect.StreamTypeUnary
		}),
	)
}
