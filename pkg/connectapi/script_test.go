// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// scriptChildScope is the per-child credential shape the three script verbs
// resolve against: ChildID is the caller's own id, and Authorize (unused by
// these verbs) stays the standard subtree refusal.
type scriptChildScope struct{ id string }

func (s scriptChildScope) ChildID() string { return s.id }

func (s scriptChildScope) Authorize(target string) error {
	return connect.NewError(connect.CodePermissionDenied, errors.New("not a descendant: "+target))
}

func (s scriptChildScope) Subtree([]string) []protocol.ChildSummary { return nil }

func (s scriptChildScope) ConversationInScope(string) bool { return false }

// recordedHub is the ScriptHub fake: it records every call and answers with
// scripted values.
type recordedHub struct {
	mu sync.Mutex

	reports   []recordedReport
	results   []recordedResult
	receives  []string
	reportErr error
	resultErr error
	recvErr   error
	stream    *scriptedStream
}

type recordedReport struct {
	callerID string
	kind     string
	dataJSON string
}

type recordedResult struct {
	callerID   string
	resultJSON string
}

func (h *recordedHub) Report(_ context.Context, callerID, kind, dataJSON string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reports = append(h.reports, recordedReport{callerID, kind, dataJSON})
	return h.reportErr
}

func (h *recordedHub) SetResult(_ context.Context, callerID, resultJSON string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, recordedResult{callerID, resultJSON})
	return h.resultErr
}

func (h *recordedHub) Receive(_ context.Context, callerID string) (connectapi.ScriptStream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.receives = append(h.receives, callerID)
	if h.recvErr != nil {
		return nil, h.recvErr
	}
	return h.stream, nil
}

func (h *recordedHub) gotReports() []recordedReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedReport(nil), h.reports...)
}

func (h *recordedHub) gotResults() []recordedResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedResult(nil), h.results...)
}

func (h *recordedHub) gotReceives() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.receives...)
}

// scriptedStream answers Recv with its messages in order, then io.EOF.
type scriptedStream struct {
	mu   sync.Mutex
	msgs []*rafikiv1.ScriptMessage
}

func (s *scriptedStream) Recv(context.Context) (*rafikiv1.ScriptMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return nil, io.EOF
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

// scriptServer wires a Server whose child scope always resolves to the given
// caller id (nil scope when empty) and whose hub is the recorded fake.
func scriptServer(t *testing.T, callerID string, hub *recordedHub) rafikiv1connect.ControlClient {
	t.Helper()
	s := connectapi.NewServer(nil)
	if callerID == "" {
		s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return nil })
	} else {
		s.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
			return scriptChildScope{id: callerID}
		})
	}
	if hub != nil {
		s.SetScriptHub(hub)
	}
	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
}

// TestScriptVerbsRequireAChildCredential proves the self-position verbs have
// no operator path: a caller that resolves to no child scope — a user
// credential, or the unix socket's anonymous local trust — has no position in
// the agent tree for the verbs to act on.
func TestScriptVerbsRequireAChildCredential(t *testing.T) {
	client := scriptServer(t, "", &recordedHub{stream: &scriptedStream{}})

	if _, err := client.Report(context.Background(), connect.NewRequest(&rafikiv1.ReportRequest{
		Kind: "progress", DataJson: `{"step":1}`,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Report as a user = %v, want %v", err, connect.CodePermissionDenied)
	}
	if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{"ok":true}`,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("SetResult as a user = %v, want %v", err, connect.CodePermissionDenied)
	}
	userStream, err := client.Receive(context.Background(), connect.NewRequest(&rafikiv1.ReceiveRequest{}))
	if err != nil {
		t.Fatalf("Receive as a user: %v", err)
	}
	if userStream.Receive() {
		t.Fatalf("Receive as a user delivered %+v", userStream.Msg())
	}
	if connect.CodeOf(userStream.Err()) != connect.CodePermissionDenied {
		t.Fatalf("Receive as a user = %v, want %v", userStream.Err(), connect.CodePermissionDenied)
	}
}

// TestScriptVerbsWithAnEmptyChildIDRefused proves the empty-ChildID scope —
// the credential that names the provenance but NO child — is refused like a
// user credential, not served by the operator path.
func TestScriptVerbsWithAnEmptyChildIDRefused(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetChildScopeSource(func(context.Context) connectapi.ChildScope { return emptyChildScope{} })
	s.SetScriptHub(&recordedHub{stream: &scriptedStream{}})
	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)

	if _, err := client.Report(context.Background(), connect.NewRequest(&rafikiv1.ReportRequest{
		Kind: "progress", DataJson: `{}`,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Report with an empty-ChildID scope = %v, want %v", err, connect.CodePermissionDenied)
	}
	if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{}`,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("SetResult with an empty-ChildID scope = %v, want %v", err, connect.CodePermissionDenied)
	}
	emptyStream, err := client.Receive(context.Background(), connect.NewRequest(&rafikiv1.ReceiveRequest{}))
	if err != nil {
		t.Fatalf("Receive with an empty-ChildID scope: %v", err)
	}
	if emptyStream.Receive() || connect.CodeOf(emptyStream.Err()) != connect.CodePermissionDenied {
		t.Fatalf("Receive with an empty-ChildID scope = %v, want %v", emptyStream.Err(), connect.CodePermissionDenied)
	}
}

// TestScriptVerbsWithoutAHubFailClosed proves an unwired hub degrades to
// CodeUnavailable — the setter is post-construction, and a request served
// before main.go finishes wiring must never report success for work that
// reached nothing.
func TestScriptVerbsWithoutAHubFailClosed(t *testing.T) {
	client := scriptServer(t, "c_script", nil)

	if _, err := client.Report(context.Background(), connect.NewRequest(&rafikiv1.ReportRequest{
		Kind: "progress", DataJson: `{}`,
	})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("Report without a hub = %v, want %v", err, connect.CodeUnavailable)
	}
	if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{}`,
	})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SetResult without a hub = %v, want %v", err, connect.CodeUnavailable)
	}
	userStream, err := client.Receive(context.Background(), connect.NewRequest(&rafikiv1.ReceiveRequest{}))
	if err != nil {
		t.Fatalf("Receive without a hub: %v", err)
	}
	if userStream.Receive() || connect.CodeOf(userStream.Err()) != connect.CodeUnavailable {
		t.Fatalf("Receive without a hub = %v, want %v", userStream.Err(), connect.CodeUnavailable)
	}
}

// TestReportForwardsToTheCallerOwnParent is the happy path: the caller's own
// id — from the credential, never from the request — is what reaches the hub.
func TestReportForwardsToTheCallerOwnParent(t *testing.T) {
	hub := &recordedHub{stream: &scriptedStream{}}
	client := scriptServer(t, "c_script", hub)

	if _, err := client.Report(context.Background(), connect.NewRequest(&rafikiv1.ReportRequest{
		Kind: "progress", DataJson: `{"step":1}`,
	})); err != nil {
		t.Fatalf("Report: %v", err)
	}
	reports := hub.gotReports()
	if len(reports) != 1 {
		t.Fatalf("want 1 report, got %d", len(reports))
	}
	if reports[0].callerID != "c_script" || reports[0].kind != "progress" ||
		reports[0].dataJSON != `{"step":1}` {
		t.Fatalf("report = %+v", reports[0])
	}
}

// TestReportValidatesKindAndData is the size/validation half: required kind,
// 64-byte kind cap, control characters, JSON parse, 4 KiB cap — refused with
// CodeInvalidArgument, never truncated.
func TestReportValidatesKindAndData(t *testing.T) {
	cases := []struct {
		name string
		req  rafikiv1.ReportRequest
	}{
		{"empty kind", rafikiv1.ReportRequest{DataJson: `{}`}},
		{"oversized kind", rafikiv1.ReportRequest{Kind: strings.Repeat("k", 65), DataJson: `{}`}},
		{"control character in kind", rafikiv1.ReportRequest{Kind: "progr\ness", DataJson: `{}`}},
		{"malformed data", rafikiv1.ReportRequest{Kind: "progress", DataJson: `{"step":`}},
		{"oversized data", rafikiv1.ReportRequest{Kind: "progress", DataJson: `{"pad":"` + strings.Repeat("x", 4096) + `"}`}},
	}
	hub := &recordedHub{stream: &scriptedStream{}}
	client := scriptServer(t, "c_script", hub)
	for i := range cases {
		tc := &cases[i]
		_, err := client.Report(context.Background(), connect.NewRequest(&tc.req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: Report = %v, want %v", tc.name, err, connect.CodeInvalidArgument)
		}
	}
	if got := hub.gotReports(); len(got) != 0 {
		t.Fatalf("refused reports reached the hub: %+v", got)
	}
	// At the cap exactly: accepted.
	_, err := client.Report(context.Background(), connect.NewRequest(&rafikiv1.ReportRequest{
		Kind:     "progress",
		DataJson: `{"pad":"` + strings.Repeat("x", 4086) + `"}`,
	}))
	if err != nil {
		t.Fatalf("4 KiB report refused: %v", err)
	}
}

// TestReceiveIsSelfOnly proves the identity check: child_id empty (own inbox)
// and child_id == own id are admitted; a sibling's id is refused with
// PermissionDenied — and the sibling's id reaching the hub would be a
// fail-open bug, so the refusal is checked before Receive is called.
func TestReceiveIsSelfOnly(t *testing.T) {
	hub := &recordedHub{stream: &scriptedStream{msgs: []*rafikiv1.ScriptMessage{
		{Body: &rafikiv1.ScriptMessage_Text_{Text: &rafikiv1.ScriptMessage_Text{
			Text: "do work", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, MessageIds: []string{"m1"},
		}}},
	}}}
	client := scriptServer(t, "c_script", hub)

	// Own id, explicit: allowed.
	stream, err := client.Receive(context.Background(), connect.NewRequest(&rafikiv1.ReceiveRequest{ChildId: "c_script"}))
	if err != nil {
		t.Fatalf("Receive(own id): %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("first Receive failed: %v", stream.Err())
	}
	msg := stream.Msg()
	if msg.GetText().GetText() != "do work" || msg.GetText().GetMessageIds()[0] != "m1" {
		t.Fatalf("text message = %+v", msg)
	}
	if stream.Receive() {
		t.Fatalf("second Receive = %+v, want stream end", stream.Msg())
	}
	if err := stream.Err(); err != nil && !strings.Contains(err.Error(), "EOF") && !errors.Is(err, io.EOF) {
		// The stream ends cleanly after the last message; any error other
		// than a clean end is a failure.
		t.Fatalf("stream ended with %v, want a clean end", err)
	}
	if got := hub.gotReceives(); len(got) != 1 || got[0] != "c_script" {
		t.Fatalf("hub received %+v", got)
	}

	// Sibling id: refused, hub never called. A server-stream refusal surfaces
	// on the first Receive, not on the initial call.
	before := len(hub.gotReceives())
	sibStream, err := client.Receive(context.Background(), connect.NewRequest(&rafikiv1.ReceiveRequest{ChildId: "c_sibling"}))
	if err != nil {
		t.Fatalf("Receive(sibling) call: %v", err)
	}
	if sibStream.Receive() {
		t.Fatalf("Receive(sibling) delivered %+v", sibStream.Msg())
	}
	if connect.CodeOf(sibStream.Err()) != connect.CodePermissionDenied {
		t.Fatalf("Receive(sibling) = %v, want %v", sibStream.Err(), connect.CodePermissionDenied)
	}
	if after := len(hub.gotReceives()); after != before {
		t.Fatalf("refused Receive reached the hub: %+v", hub.gotReceives())
	}
}

// TestSetResultIsSelfOnlyAndValidated: the result is stored against the
// caller's own id, and malformed or oversized JSON is refused before the hub
// is reached.
func TestSetResultIsSelfOnlyAndValidated(t *testing.T) {
	hub := &recordedHub{stream: &scriptedStream{}}
	client := scriptServer(t, "c_script", hub)

	if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{"answer":42}`,
	})); err != nil {
		t.Fatalf("SetResult: %v", err)
	}
	results := hub.gotResults()
	if len(results) != 1 || results[0].callerID != "c_script" || results[0].resultJSON != `{"answer":42}` {
		t.Fatalf("result = %+v", results)
	}

	for name, res := range map[string]string{
		"malformed": `{"answer":`,
		"empty":     ``,
		"oversized": `{"pad":"` + strings.Repeat("x", 4097) + `"}`,
	} {
		if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{ResultJson: res})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s result: SetResult = %v, want %v", name, err, connect.CodeInvalidArgument)
		}
	}
	if got := len(hub.gotResults()); got != 1 {
		t.Fatalf("refused results reached the hub: %d", got)
	}

	// At the cap exactly: accepted (and stored trimmed — F3's rule).
	if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{"pad":"` + strings.Repeat("x", 4086) + `"}`,
	})); err != nil {
		t.Fatalf("4 KiB result refused: %v", err)
	}
	results = hub.gotResults()
	if len(results) != 2 || results[1].resultJSON != `{"pad":"`+strings.Repeat("x", 4086)+`"}` {
		t.Fatalf("at-cap result = %+v", results)
	}

	// Trailing whitespace is trimmed, not stored: json.Valid would accept
	// padding newlines and they would land verbatim in the parent's frame.
	if _, err := client.SetResult(context.Background(), connect.NewRequest(&rafikiv1.SetResultRequest{
		ResultJson: `{"answer":7}` + "\n\n",
	})); err != nil {
		t.Fatalf("whitespace-padded result refused: %v", err)
	}
	results = hub.gotResults()
	if len(results) != 3 || results[2].resultJSON != `{"answer":7}` {
		t.Fatalf("trimmed result = %+v", results)
	}
}
