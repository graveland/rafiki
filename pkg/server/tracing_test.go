// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/multigres/testkit/assert"
)

// fakeSpanParent answers every ParentFor with a fixed span context.
type fakeSpanParent struct {
	sc      trace.SpanContext
	calls   int
	childID string
	prevMsg string
}

func (f *fakeSpanParent) ParentFor(childID, prevMessageID string) trace.SpanContext {
	f.calls++
	f.childID = childID
	f.prevMsg = prevMessageID
	return f.sc
}

func sampledSpanContext(t *testing.T) trace.SpanContext {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("span id: %v", err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
}

func newRecordingProvider() (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)), sr
}

func attrValue(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// sseUpstream serves a minimal well-formed Anthropic SSE turn.
func sseUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n"+
			"event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n")
	}))
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestProxyChatSpanNestsUnderResolver(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := sseUpstream(t)
	defer upstream.Close()

	tp, sr := newRecordingProvider()
	parent := sampledSpanContext(t)
	fp := &fakeSpanParent{sc: parent}

	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, testLogger())
	p.store = &fakeProxyStore{}
	p.SetTracing(tp, fp)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	req = req.WithContext(WithIdentity(req.Context(), &Identity{
		Via: ProvenanceChildToken, ChildID: "c_child", UserID: "u_owner",
	}))
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status =")
	spans := sr.Ended()
	c.Require().Len(spans, 1, "ended spans")
	span := spans[0]
	c.Eq("chat claude", span.Name(), "span name")
	c.Eq(trace.SpanKindClient, span.SpanKind(), "span kind")
	c.EqDeep(parent, span.Parent(), "span parent")
	c.Eq(1, fp.calls, "resolver asked once")
	c.Eq("c_child", fp.childID, "resolver was asked with the child id")
	c.Eq("", fp.prevMsg, "no previous_message_id in the body")
	agentID, ok := attrValue(span, "gen_ai.agent.id")
	c.Require().True(ok, "gen_ai.agent.id present")
	c.Eq("c_child", agentID, "gen_ai.agent.id")
	c.Eq("chat", mustAttr(t, span, "gen_ai.operation.name"), "gen_ai.operation.name")
	c.Eq("claude", mustAttr(t, span, "gen_ai.request.model"), "gen_ai.request.model")
	c.Eq("anthropic", mustAttr(t, span, "rafiki.protocol"), "rafiki.protocol")
	c.Eq("main", mustAttr(t, span, "rafiki.thread"), "rafiki.thread")
	c.Eq("anthropic", mustAttr(t, span, "gen_ai.provider.name"), "gen_ai.provider.name")
}

func TestProxyChatSpanSubagentThread(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := sseUpstream(t)
	defer upstream.Close()

	tp, sr := newRecordingProvider()
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, testLogger())
	p.store = &fakeProxyStore{}
	p.SetTracing(tp, nil)

	system := "You are Claude Code.\nx-anthropic-billing-header: cc_version=2.1.267.019; cc_entrypoint=sdk-cli; cch=6d579; cc_is_subagent=true; cc_prev_req=req_x; cc_prompt_id=p;"
	body := `{"model":"claude","stream":true,"system":[{"type":"text","text":` + strconv.Quote(system) + `}]}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusOK, rec.Code, "status =")
	spans := sr.Ended()
	c.Require().Len(spans, 1, "ended spans")
	c.Eq("subagent", mustAttr(t, spans[0], "rafiki.thread"), "rafiki.thread")
}

func TestProxyChatSpanUpstreamError(t *testing.T) {
	c := assert.NewCollecting(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"upstream exploded"}}`)
	}))
	defer upstream.Close()

	tp, sr := newRecordingProvider()
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, testLogger())
	p.store = &fakeProxyStore{}
	p.SetTracing(tp, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":true}`))
	p.ServeHTTP(rec, req)

	c.Require().Eq(http.StatusInternalServerError, rec.Code, "status =")
	spans := sr.Ended()
	c.Require().Len(spans, 1, "ended spans")
	span := spans[0]
	c.Eq(codes.Error, span.Status().Code, "span status")
	c.Require().True(len(span.Events()) > 0, "span carries an exception event")
	c.Eq("exception", span.Events()[0].Name, "event name")
}

func mustAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string) string {
	t.Helper()
	v, ok := attrValue(span, key)
	if !ok {
		t.Fatalf("attribute %q missing from span %q (attrs: %v)", key, span.Name(), span.Attributes())
	}
	return v
}
