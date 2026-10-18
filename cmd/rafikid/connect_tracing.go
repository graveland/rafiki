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
//
// WithPropagator: otelconnect otherwise extracts with the GLOBAL propagator,
// which this daemon never installs (the provider is injected, never global).
// Passing the daemon's own propagator (the one traceMiddleware uses) is what
// makes an inbound W3C traceparent on a Control call visible at all.
//
// WithTrustRemote: otelconnect's default is to treat an inbound server context
// as untrusted — a new root span plus a link. The documented contract is that a
// Connect server span is the CHILD of the caller's traceparent (the operator
// link then points at this span), so the inbound context is trusted. The callers
// this reaches are authenticated on both mounts; a caller choosing its own trace
// id is the ordinary W3C propagation semantic, not a privilege.
func newConnectTracing(tp trace.TracerProvider) (connect.Interceptor, error) {
	if tp == nil {
		return nil, nil
	}
	return otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tp),
		otelconnect.WithPropagator(propagator),
		otelconnect.WithTrustRemote(),
		otelconnect.WithoutMetrics(),
		otelconnect.WithFilter(func(_ context.Context, spec connect.Spec) bool {
			return spec.StreamType == connect.StreamTypeUnary
		}),
	)
}
