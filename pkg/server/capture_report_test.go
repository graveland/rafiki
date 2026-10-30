// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
)

type recordingSink struct {
	mu      sync.Mutex
	reports []CaptureReport
}

func (s *recordingSink) CaptureReported(r CaptureReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
}

func (s *recordingSink) all() []CaptureReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CaptureReport(nil), s.reports...)
}

const (
	reportToolsBody = `{"model":"claude","stream":true,"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`
	reportPlainBody = `{"model":"claude","stream":true}`
)

const reportSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_resp1","role":"assistant","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

// reportProxy builds a proxy over fs and a recording sink whose upstream answers
// with status and, for a 2xx, reportSSE. calls counts upstream hits.
func reportProxy(t *testing.T, fs *fakeProxyStore, status int) (*MessagesProxy, *recordingSink, *int) {
	t.Helper()
	calls := new(int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		if status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reportSSE)
	}))
	t.Cleanup(upstream.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := NewMessagesProxy(nil, nil, "real-key", upstream.URL, "", nil, logger)
	p.store = fs
	sink := &recordingSink{}
	p.SetCaptureSink(sink)
	return p, sink, calls
}

func serveReport(p *MessagesProxy, body, session string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if session != "" {
		req.Header.Set("X-Rafiki-Session", session)
	}
	p.ServeHTTP(rec, req)
	return rec
}

func TestCaptureReportOnSuccess(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []*conv.Node{{Ref: conv.Ref{Ordinal: 0}}, {Ref: conv.Ref{Ordinal: 1}}}
	fs := &fakeProxyStore{sinceRows: rows}
	p, sink, _ := reportProxy(t, fs, http.StatusOK)

	rec := serveReport(p, reportToolsBody, "sess-1")
	c.Eq(http.StatusOK, rec.Code, "status")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.Eq("sess-1", got[0].Session)
	c.Eq("", got[0].ThreadID)
	c.Eq("conv-1", got[0].ConversationID)
	c.Eq("msg_resp1", got[0].ResponseMessageID)
	c.Eq("", got[0].Err)
	c.EqDeep(rows, got[0].Rows)
	c.Eq(1, fs.sinceCalls, "MessagesSince calls")
	c.True(fs.lastSince.Equal(time.Unix(0, 0)), "MessagesSince since = %v, want the turn intent's createdAt", fs.lastSince)
}

func TestCaptureReportOnUpstreamErrorCarriesPromptAndTurnErr(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []*conv.Node{{Ref: conv.Ref{Ordinal: 0}}}
	fs := &fakeProxyStore{sinceRows: rows}
	p, sink, _ := reportProxy(t, fs, http.StatusTooManyRequests)

	serveReport(p, reportToolsBody, "sess-1")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.Eq("", got[0].Err, "an upstream failure is not a persistence failure")
	c.NotEmpty(got[0].TurnErr, "TurnErr")
	c.StrContains(got[0].TurnErr, "429")
	c.Eq("", got[0].ResponseMessageID)
	c.EqDeep(rows, got[0].Rows)
}

func TestCaptureReportOnAppendFailure(t *testing.T) {
	c := assert.NewAborting(t)
	fs := &fakeProxyStore{appendErr: errors.New("ordinal occupied")}
	p, sink, _ := reportProxy(t, fs, http.StatusOK)

	serveReport(p, reportToolsBody, "sess-1")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.StrContains(got[0].Err, "append response failed")
	c.Eq("", got[0].ResponseMessageID)
}

func TestCaptureReportReadBackFailure(t *testing.T) {
	c := assert.NewAborting(t)
	fs := &fakeProxyStore{sinceErr: errors.New("db gone"), sinceRows: []*conv.Node{{Ref: conv.Ref{Ordinal: 0}}}}
	p, sink, _ := reportProxy(t, fs, http.StatusOK)

	serveReport(p, reportToolsBody, "sess-1")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.StrContains(got[0].Err, "read back failed")
	c.Eq("", got[0].ResponseMessageID)
	c.Len(got[0].Rows, 0, "rows")
}

// A session request that declares no client tools is not the agent's own
// conversation — Claude Code's titler, its quota probe and the per-tool model
// helpers all ride the session header — so none of its rows may publish. It
// MUST still report, because the response id in that report is what releases the
// child's message gate: a request that armed the gate and reported nothing left
// the child's event stream (TurnEnd included) silent until the child exited.
func TestCaptureReportReleasesGateForRequestWithoutClientTools(t *testing.T) {
	c := assert.NewAborting(t)
	fs := &fakeProxyStore{sinceRows: []*conv.Node{{Ref: conv.Ref{Ordinal: 0}}, {Ref: conv.Ref{Ordinal: 1}}}}
	p, sink, _ := reportProxy(t, fs, http.StatusOK)

	serveReport(p, reportPlainBody, "sess-1")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.True(got[0].ReleaseOnly, "ReleaseOnly")
	c.Eq("msg_resp1", got[0].ResponseMessageID, "the response id is what releases the gate")
	c.Len(got[0].Rows, 0, "rows")
	c.Eq(0, fs.sinceCalls, "MessagesSince calls: a release-only report reads nothing back")
}

// The client-tool declaration decides whether rows publish, never whether the
// request reports: a refused capture on a no-client-tools request must reach
// the sink too, or the gate its frame armed is never released.
func TestCaptureReportReleasesGateForRefusedRequestWithoutClientTools(t *testing.T) {
	c := assert.NewAborting(t)
	p, sink, _ := reportProxy(t, &fakeProxyStore{intentErr: errors.New("db down")}, http.StatusOK)

	serveReport(p, reportPlainBody, "sess-1")

	got := sink.all()
	c.Require().Len(got, 1, "reports")
	c.True(got[0].ReleaseOnly, "ReleaseOnly")
	c.StrContains(got[0].Err, "insert turn intent", "Err")
	c.Eq("", got[0].ResponseMessageID, "a refused capture stores no response")
}

func TestCaptureReportSkipsSessionlessRequest(t *testing.T) {
	c := assert.NewAborting(t)
	p, sink, _ := reportProxy(t, &fakeProxyStore{}, http.StatusOK)

	serveReport(p, reportToolsBody, "")

	c.Len(sink.all(), 0, "reports")
}

func TestProxyRefusesSessionRequestWhenCaptureCannotBegin(t *testing.T) {
	cases := []struct {
		name   string
		store  *fakeProxyStore
		prefix string
	}{
		{"ensure conversation", &fakeProxyStore{resolveErr: errors.New("db down")}, "ensure conversation:"},
		{"insert turn intent", &fakeProxyStore{intentErr: errors.New("db down")}, "insert turn intent:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			p, sink, calls := reportProxy(t, tc.store, http.StatusOK)

			rec := serveReport(p, reportToolsBody, "sess-1")

			c.Eq(http.StatusServiceUnavailable, rec.Code, "status")
			c.Eq(0, *calls, "upstream calls")
			got := sink.all()
			c.Require().Len(got, 1, "reports")
			c.True(strings.HasPrefix(got[0].Err, tc.prefix), "Err = %q, want prefix %q", got[0].Err, tc.prefix)
			c.Eq("sess-1", got[0].Session)
		})
	}
}

func TestProxyForwardsSessionlessRequestWhenCaptureCannotBegin(t *testing.T) {
	c := assert.NewAborting(t)
	p, sink, calls := reportProxy(t, &fakeProxyStore{resolveErr: errors.New("db down")}, http.StatusOK)

	rec := serveReport(p, reportToolsBody, "")

	c.Eq(http.StatusOK, rec.Code, "status")
	c.Eq(1, *calls, "upstream calls")
	c.Len(sink.all(), 0, "reports")
}

func TestSetCaptureSinkNilIsRefused(t *testing.T) {
	c := assert.NewAborting(t)
	p := &MessagesProxy{}
	p.SetCaptureSink(nil)
	c.True(p.captureSink == nil, "nil sink must leave the field unset")
	p.SetCaptureSink(&recordingSink{})
	c.True(p.captureSink != nil, "a real sink is stored")
}
