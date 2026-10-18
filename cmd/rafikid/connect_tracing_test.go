// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/telemetry"
	"go.graveland.dev/rafiki/pkg/turntrace"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// tracingMount mounts the Control service through connectControlRoute exactly
// as proxy.go does — behind the proxy face's auth middleware — with tel in the
// tracing position. It returns a client pointed at the mounted stack.
func tracingMount(t *testing.T, tel connect.Interceptor) rafikiv1connect.ControlClient {
	t.Helper()

	auth := server.NewUserTokenAuth(
		fakeUserStore{token: proxyUserToken, id: users.Identity{UserID: "u1", Username: "brent"}},
		proxyBootToken,
		server.DefaultAuthCacheTTL,
	)
	auth.SetChildTokenLookup(func(token string) (childID, ownerUserID string, ok bool) {
		if token == proxyChildToken {
			return "c_child", "u1", true
		}
		return "", "", false
	})
	auth.SetChildOwnerLookup(func(childID string) (string, bool) {
		if childID == "c_child" {
			return "u1", true
		}
		return "", false
	})

	ctrl := &Controller{st: childstore.New(), cm: newChildManager()}
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		StartedAt: time.Now(),
	})
	srv := connectapi.NewServer(nil)
	srv.SetChildScopeSource(ctrl.childScopeFor)

	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv, newStreamRegistry(), tel)

	mux := http.NewServeMux()
	h.Mount(mux, auth.Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return rafikiv1connect.NewControlClient(epochClient(ts.Client()), ts.URL)
}

// countServerSpans counts the spans the interceptor produced as a server.
func countServerSpans(spans []sdktrace.ReadOnlySpan) int {
	n := 0
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindServer {
			n++
		}
	}
	return n
}

// traceAttrString returns the span attribute's string value, or "" when absent.
func traceAttrString(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

// TestConnectTracingUnaryTraced: a unary Control call through
// connectControlRoute yields exactly one server span.
func TestConnectTracingUnaryTraced(t *testing.T) {
	c := assert.NewAborting(t)
	tp, rec := newTraceObserver(t)
	tel, err := newConnectTracing(tp)
	c.NoError(err, "newConnectTracing")
	c.True(tel != nil, "a non-nil provider must produce an interceptor")

	client := tracingMount(t, tel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&rafikiv1.ListPresetsRequest{})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)
	// The preset manager is unwired in this fixture; CodeUnavailable is the
	// handler running, which is all this test needs.
	_, _ = client.ListPresets(ctx, req)

	c.Eq(1, countServerSpans(rec.Ended()), "server spans for a unary call")
}

// TestConnectTracingStreamsExcluded: a server-streaming Control RPC produces no
// span. A stream's lifetime belongs to its handler, so a span around it would
// measure the client's patience, not the work — the filter excludes it.
func TestConnectTracingStreamsExcluded(t *testing.T) {
	c := assert.NewAborting(t)
	tp, rec := newTraceObserver(t)
	tel, err := newConnectTracing(tp)
	c.NoError(err, "newConnectTracing")

	client := tracingMount(t, tel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&rafikiv1.StreamEventsRequest{})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)
	stream, err := client.StreamEvents(ctx, req)
	c.NoError(err, "open StreamEvents")
	if err == nil {
		for stream.Receive() {
		}
		_ = stream.Close()
	}

	c.Eq(0, countServerSpans(rec.Ended()), "server spans for a streaming call")
}

// TestConnectTracingRefusalTraced: a call the policy gate refuses is still
// traced, with an error status. That proves the tracer sits BEFORE the gate —
// placed after it, the one call an operator most wants to see would be the one
// recorded nowhere.
func TestConnectTracingRefusalTraced(t *testing.T) {
	c := assert.NewAborting(t)
	tp, rec := newTraceObserver(t)
	tel, err := newConnectTracing(tp)
	c.NoError(err, "newConnectTracing")

	client := tracingMount(t, tel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&rafikiv1.DeleteSkillRequest{Namespace: "user", Name: "s"})
	req.Header().Set("Authorization", "Bearer "+proxyChildToken)
	_, err = client.DeleteSkill(ctx, req)
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "DeleteSkill with a child credential = %v, want", err)

	// otelconnect applies the OTel RPC semantic convention to the span STATUS:
	// a client error like PERMISSION_DENIED is deliberately NOT codes.Error
	// (that would mark the server unhealthy). The refusal is recorded in the
	// span's status ATTRIBUTES instead, which is what proves the tracer ran
	// before the gate: the interceptor saw the gate's error and stamped it on a
	// span that a post-gate tracer would never have opened at all.
	var found bool
	for _, s := range rec.Ended() {
		if s.SpanKind() != trace.SpanKindServer {
			continue
		}
		found = true
		c.Eq("PERMISSION_DENIED", traceAttrString(s, "rpc.response.status_code"), "refused call rpc.response.status_code")
		c.Eq("PERMISSION_DENIED", traceAttrString(s, "error.type"), "refused call error.type")
		c.Eq(codes.Unset, s.Status().Code, "a client-error refusal is not an OTel server error")
	}
	c.True(found, "the refusal must produce a server span")
}

// TestConnectTracingNilIsOff: a nil interceptor leaves the routes working and
// traces nothing. This is the tracing-off path, where newConnectTracing
// returns nil, nil for a nil provider.
func TestConnectTracingNilIsOff(t *testing.T) {
	c := assert.NewAborting(t)

	tel, err := newConnectTracing(nil)
	c.NoError(err, "newConnectTracing(nil)")
	c.True(tel == nil, "a nil provider must mean no interceptor")

	_, rec := newTraceObserver(t)
	client := tracingMount(t, tel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&rafikiv1.ListPresetsRequest{})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)
	_, err = client.ListPresets(ctx, req)
	// The route must still work: the unwired manager answers Unavailable, not
	// the Unimplemented a missing route gives.
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "ListPresets with tracing off = %v, want", err)
	c.Eq(0, countServerSpans(rec.Ended()), "spans with tracing off")
}

// TestProxyTracingOffPassesNilResolver pins the typed-nil guard: the proxy
// options carry a *turntrace.Observer, and passing a nil one straight into the
// SpanParentResolver interface would make the seam non-nil. The adapter must
// yield a nil INTERFACE for a nil observer.
func TestProxyTracingOffPassesNilResolver(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(spanParentResolver(nil) == nil, "a nil observer must become a nil resolver")

	tp, _ := newTraceObserver(t)
	obs := turntrace.New(tp, telemetry.Config{}.Policy(), nil, nil)
	c.True(spanParentResolver(obs) != nil, "a real observer must become a real resolver")
}
