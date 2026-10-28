// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multigres/testkit/assert"
)

func newPassthroughProxy(upstreamURL string) *MessagesProxy {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstreamURL, "", nil, logger)
	p.store = &fakeProxyStore{}
	return p
}

func passthroughRequest(session string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude","stream":false}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	if session != "" {
		req.Header.Set("X-Rafiki-Session", session)
	}
	return req.WithContext(WithPassthroughCredential(context.Background(), "Bearer sk-ant-oat01-client"))
}

// A connection that dies before any response byte is a transport error the
// passthrough path replays, with the caller's credential, on a fresh dial.
func TestPassthroughRetriesTransportError(t *testing.T) {
	c := assert.NewCollecting(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		c.Eq("Bearer sk-ant-oat01-client", r.Header.Get("Authorization"), "credential on the retry")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","stop_reason":"end_turn","usage":{"output_tokens":1}}`)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	newPassthroughProxy(upstream.URL).ServeHTTP(rec, passthroughRequest(""))

	c.Require().Eq(http.StatusOK, rec.Code, "status")
	c.Eq(int32(2), calls.Load(), "upstream calls")
}

// A status the upstream actually returned is the client's to judge: replaying
// it would double-spend a request Anthropic already answered.
func TestPassthroughDoesNotRetryAnUpstreamStatus(t *testing.T) {
	c := assert.NewCollecting(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	newPassthroughProxy(upstream.URL).ServeHTTP(rec, passthroughRequest(""))

	c.Eq(http.StatusInternalServerError, rec.Code, "status")
	c.Eq(int32(1), calls.Load(), "upstream calls")
}
