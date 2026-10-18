// SPDX-License-Identifier: Apache-2.0

package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// tracingSender is the minimal Sender the span tests need: a canned response
// or error, no streaming.
type tracingSender struct {
	resp *anthropic.Message
	err  error
}

func (s tracingSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	return s.resp, s.err
}

func tracingMessage(model, stop string, in, out, cacheRead, cacheCreate int64) *anthropic.Message {
	var m anthropic.Message
	raw := `{"id":"msg_t","type":"message","role":"assistant","model":"` + model + `",
		"content":[{"type":"text","text":"hi"}],"stop_reason":"` + stop + `",
		"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) +
		`,"cache_read_input_tokens":` + itoa(cacheRead) + `,"cache_creation_input_tokens":` + itoa(cacheCreate) + `}}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return &m
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// tracingClient builds a client over one injected anthropic sender and a
// recorder-backed tracer provider.
func tracingClient(t *testing.T, sender *tracingSender, opts ...llm.ClientOption) (*llm.Client, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	set := &providers.Set{
		DefaultProvider: "anthropic",
		Providers: map[string]providers.Provider{
			"anthropic": {Name: "anthropic", Kind: providers.KindAnthropic},
		},
	}
	base := []llm.ClientOption{
		llm.WithProviders(set),
		llm.WithProviderSender("anthropic", sender),
		llm.WithTracerProvider(tp),
	}
	c, err := llm.NewClient(append(base, opts...)...)
	assert.NewAborting(t).NoError(err, "NewClient")
	return c, rec
}

func chatParams() anthropic.MessageNewParams {
	return anthropic.MessageNewParams{Model: anthropic.Model("m"), MaxTokens: 8}
}

func endedSpan(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not recorded; recorded: %v", name, spanNames(rec))
	return nil
}

func spanNames(rec *tracetest.SpanRecorder) []string {
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	return names
}

func attrValue(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestChatSpanSemconv(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := &tracingSender{resp: tracingMessage("claude-haiku-4-5", "end_turn", 10, 5, 3, 2)}
	client, rec := tracingClient(t, sender)

	if _, err := client.SendParams(context.Background(), llm.SendMeta{}, chatParams()); err != nil {
		t.Fatalf("SendParams: %v", err)
	}

	span := endedSpan(t, rec, "chat m")
	c.Eq("chat m", span.Name(), "span name")

	for key, want := range map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.request.model":  "m",
		"gen_ai.provider.name":  "anthropic",
		"gen_ai.response.model": "claude-haiku-4-5",
	} {
		v, ok := attrValue(span, key)
		c.True(ok, "attribute %q missing", key)
		if ok {
			c.Eq(want, v.AsString(), "attribute %q", key)
		}
	}
	for key, want := range map[string]int64{
		"gen_ai.usage.input_tokens":                10,
		"gen_ai.usage.output_tokens":               5,
		"gen_ai.usage.cache_read.input_tokens":     3,
		"gen_ai.usage.cache_creation.input_tokens": 2,
	} {
		v, ok := attrValue(span, key)
		c.True(ok, "attribute %q missing", key)
		if ok {
			c.Eq(want, v.AsInt64(), "attribute %q", key)
		}
	}
	v, ok := attrValue(span, "gen_ai.response.finish_reasons")
	c.True(ok, "finish_reasons missing")
	if ok {
		c.EqDeep([]string{"end_turn"}, v.AsStringSlice(), "finish_reasons")
	}
}

func TestChatSpanErrorFullText(t *testing.T) {
	c := assert.NewCollecting(t)
	long := strings.Repeat("e", 70*1024) + "TAIL"
	client, rec := tracingClient(t, &tracingSender{err: errors.New(long)})

	if _, err := client.SendParams(context.Background(), llm.SendMeta{}, chatParams()); err == nil {
		t.Fatal("SendParams succeeded, want the sender's error")
	}

	span := endedSpan(t, rec, "chat m")
	c.Eq(codes.Error, span.Status().Code, "span status")

	var msg string
	found := false
	for _, ev := range span.Events() {
		if ev.Name != "exception" {
			continue
		}
		for _, kv := range ev.Attributes {
			if string(kv.Key) == "exception.message" {
				msg = kv.Value.AsString()
				found = true
			}
		}
	}
	c.True(found, "exception event with exception.message missing")
	c.StrContains(msg, "TAIL", "exception message lost the tail")
	c.StrContains(msg, "bytes elided", "exception message was not capped by ErrorText")
}

func fixedSpanContext(traceByte, spanByte byte) trace.SpanContext {
	var traceID trace.TraceID
	var spanID trace.SpanID
	for i := range traceID {
		traceID[i] = traceByte
	}
	for i := range spanID {
		spanID[i] = spanByte
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
}

func TestSpanParentUsedWithoutCtxSpan(t *testing.T) {
	c := assert.NewCollecting(t)
	want := fixedSpanContext(1, 2)
	client, rec := tracingClient(t, &tracingSender{resp: tracingMessage("m", "end_turn", 1, 1, 0, 0)},
		llm.WithSpanParent(func() trace.SpanContext { return want }))

	if _, err := client.SendParams(context.Background(), llm.SendMeta{}, chatParams()); err != nil {
		t.Fatalf("SendParams: %v", err)
	}

	span := endedSpan(t, rec, "chat m")
	c.Eq(want.TraceID().String(), span.Parent().TraceID().String(), "parent trace id")
	c.Eq(want.SpanID().String(), span.Parent().SpanID().String(), "parent span id")
}

func TestChatSpanEntrypoint(t *testing.T) {
	c := assert.NewCollecting(t)
	client, rec := tracingClient(t, &tracingSender{resp: tracingMessage("m", "end_turn", 1, 1, 0, 0)})

	if _, err := client.SendParams(context.Background(),
		llm.SendMeta{OriginEntrypoint: "recall-summary"}, chatParams()); err != nil {
		t.Fatalf("SendParams: %v", err)
	}

	span := endedSpan(t, rec, "chat m")
	c.False(span.Parent().IsValid(), "daemon job span should be a root")
	v, ok := attrValue(span, "rafiki.entrypoint")
	c.True(ok, "rafiki.entrypoint missing")
	if ok {
		c.Eq("recall-summary", v.AsString(), "rafiki.entrypoint")
	}
}

func TestSpanParentIgnoredWhenCtxHasSpan(t *testing.T) {
	c := assert.NewCollecting(t)
	ignored := fixedSpanContext(3, 4)
	client, rec := tracingClient(t, &tracingSender{resp: tracingMessage("m", "end_turn", 1, 1, 0, 0)},
		llm.WithSpanParent(func() trace.SpanContext { return ignored }))

	callerTP := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = callerTP.Shutdown(context.Background()) })
	ctx, caller := callerTP.Tracer("test").Start(context.Background(), "caller")
	defer caller.End()
	if _, err := client.SendParams(ctx, llm.SendMeta{}, chatParams()); err != nil {
		t.Fatalf("SendParams: %v", err)
	}

	span := endedSpan(t, rec, "chat m")
	c.Eq(caller.SpanContext().TraceID().String(), span.Parent().TraceID().String(), "parent trace id")
	c.Eq(caller.SpanContext().SpanID().String(), span.Parent().SpanID().String(), "parent span id")
}
