// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

// SpanParentResolver finds where a child's proxied LLM call nests: its open
// turn, or the Task tool span of the Claude sub-agent thread the request
// continues. Satisfied by *turntrace.Observer.
type SpanParentResolver interface {
	ParentFor(childID, prevMessageID string) trace.SpanContext
}

// noopTracer is the tracer both proxies carry until SetTracing is called.
func noopTracer() trace.Tracer {
	return noop.NewTracerProvider().Tracer("rafiki.server")
}

// startChatSpan opens the "chat <model>" client span for one proxied LLM
// call. When the request already carries a valid span it nests under it; only
// otherwise — and only when a resolver names a valid parent for this child —
// does it adopt the resolver's parent. A child id with no resolver, or a
// resolver with no known parent, leaves the span a root.
//
// provider may be empty when the upstream is not yet resolved;
// gen_ai.provider.name is then set later on the returned span.
func startChatSpan(ctx context.Context, tracer trace.Tracer, r SpanParentResolver, childID, prevMessageID, model, provider, protocol string) (context.Context, trace.Span) {
	if tracer == nil {
		tracer = noopTracer()
	}
	if !trace.SpanContextFromContext(ctx).IsValid() && r != nil && childID != "" {
		if parent := r.ParentFor(childID, prevMessageID); parent.IsValid() {
			ctx = trace.ContextWithSpanContext(ctx, parent)
		}
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", model),
		attribute.String("gen_ai.provider.name", provider),
		attribute.String("rafiki.protocol", protocol),
	}
	if childID != "" {
		attrs = append(attrs, attribute.String("gen_ai.agent.id", childID))
	}
	return tracer.Start(ctx, "chat "+model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))
}

// recordSpanError marks the request's span errored, capping the text the same
// way every exported error is capped.
func recordSpanError(ctx context.Context, text string) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(errors.New(telemetry.ErrorText(text)))
	span.SetStatus(codes.Error, "")
}

// setUsageAttributes records the token usage of a completed turn on its span.
func setUsageAttributes(span trace.Span, usage routing.CapturedUsage) {
	span.SetAttributes(
		attribute.Int64("gen_ai.usage.input_tokens", usage.InputTokens),
		attribute.Int64("gen_ai.usage.output_tokens", usage.OutputTokens),
		attribute.Int64("gen_ai.usage.cache_read.input_tokens", usage.CacheReadTokens),
		attribute.Int64("gen_ai.usage.cache_creation.input_tokens", usage.CacheCreationTokens),
	)
}
