// SPDX-License-Identifier: Apache-2.0

package integration_test

// An OTLP/HTTP trace receiver for the telemetry integration tests. It is a
// real HTTP server rather than an in-memory exporter so the test proves the
// daemon's exporter configuration, protobuf encoding and wire path end to
// end — the same bytes a collector would see.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otlpFixture accepts OTLP/HTTP trace exports and exposes what arrived.
//
// Two signals: every accepted POST is one value on exports (buffered, never
// blocking the exporter), and every decoded span is one value on spans. The
// request counter stays exact even if a channel would have overflowed, so the
// negative test can assert "nothing was ever posted" without relying on a
// channel read.
type otlpFixture struct {
	srv      *httptest.Server
	spans    chan *tracepb.Span
	exports  chan struct{}
	requests atomic.Int64
}

func newOTLPFixture(t *testing.T) *otlpFixture {
	t.Helper()
	f := &otlpFixture{
		spans:   make(chan *tracepb.Span, 1024),
		exports: make(chan struct{}, 256),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			http.NotFound(w, r)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-protobuf" {
			http.Error(w, "unexpected content type: "+ct, http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req collectortracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
			return
		}
		f.requests.Add(1)
		select {
		case f.exports <- struct{}{}:
		default:
		}
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					select {
					case f.spans <- sp:
					default:
					}
				}
			}
		}
		// An empty ExportTraceServiceResponse is the "no partial success"
		// reply; the exporter unmarshals the body.
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// spanStringAttr returns the string-valued attribute key on sp, or the empty
// string and false when it is absent (or not a string).
func spanStringAttr(sp *tracepb.Span, key string) (string, bool) {
	for _, kv := range sp.GetAttributes() {
		if kv.GetKey() != key {
			continue
		}
		return kv.GetValue().GetStringValue(), true
	}
	return "", false
}
