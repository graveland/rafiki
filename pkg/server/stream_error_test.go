// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/multigres/testkit/assert"
)

const streamErrorSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_partial","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
	"event: error\n" +
	`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"

func streamProxy(t *testing.T, sse string) (*MessagesProxy, *recordingSink, *stubRateLimitObserver) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(upstream.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = &fakeProxyStore{}
	sink := &recordingSink{}
	p.SetCaptureSink(sink)
	obs := newStubRateLimitObserver()
	p.SetRateLimitObserver(obs)
	return p, sink, obs
}

func TestInBandStreamErrorReportsAFailedTurnAndDoesNotClearTheRateLimitWatch(t *testing.T) {
	c := assert.NewCollecting(t)

	p, sink, obs := streamProxy(t, streamErrorSSE)
	serveReport(p, reportToolsBody, "sess-1")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.Eq("", got[0].Err, "an in-band error is not a persistence failure")
	c.StrContains(got[0].TurnErr, "stream error", "TurnErr names the stream error")
	c.StrContains(got[0].TurnErr, "Overloaded", "TurnErr carries the provider's message")
	c.Eq("", got[0].ResponseMessageID, "a failed turn names no persisted response")
	c.Eq(0, len(obs.oks), "an errored stream is not a clean completion for the rate-limit watch")

	control, controlSink, controlObs := streamProxy(t, reportSSE)
	serveReport(control, reportToolsBody, "sess-1")
	cgot := controlSink.all()
	c.Require().Len(cgot, 1, "control reports")
	c.Eq("", cgot[0].TurnErr, "a clean stream carries no TurnErr")
	c.EqDeep([]string{"sess-1"}, controlObs.oks)
}
