// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestCloseCommandKeepsForgetAsAnAlias(t *testing.T) {
	cmd := newCloseCmd()
	if cmd.Name() != "close" {
		t.Errorf("Name() = %q, want close", cmd.Name())
	}
	var hasForget, hasRM bool
	for _, a := range cmd.Aliases {
		switch a {
		case "forget":
			hasForget = true
		case "rm":
			hasRM = true
		}
	}
	if !hasForget {
		t.Error("`forget` must stay an alias: it is in muscle memory and in scripts")
	}
	if !hasRM {
		t.Error("`rm` was an alias before the rename and must survive it")
	}
}

func TestCloseCmd_HasStopTimeoutFlags(t *testing.T) {
	cmd := newCloseCmd()
	if cmd.Flags().Lookup("shutdown-timeout") == nil {
		t.Error("--shutdown-timeout missing: close must be able to override the stop-first step's timeout")
	}
	if cmd.Flags().Lookup("kill-timeout") == nil {
		t.Error("--kill-timeout missing: close must be able to override the stop-first step's timeout")
	}
}

func TestCloseAllExitedTextCount(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"three closed", `{"count":3,"children":["a","b","c"]}`, "closed 3 exited children\n"},
		{"zero closed", `{"count":0}`, "no exited children to close\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderCloseAllExited(&buf, json.RawMessage(tc.raw), outputTable); err != nil {
				t.Fatalf("renderCloseAllExited: %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("output = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// JSON mode stays the raw payload passthrough it has always been — the client
// reshapes nothing, so whatever the daemon sent (including fields this client
// does not know about) reaches the consumer.
func TestCloseAllExitedJSONRawPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"count":2,"children":["a","b"],"extra":"field"}`)
	var buf bytes.Buffer
	if err := renderCloseAllExited(&buf, raw, outputJSON); err != nil {
		t.Fatalf("renderCloseAllExited: %v", err)
	}
	var want bytes.Buffer
	enc := json.NewEncoder(&want)
	enc.SetIndent("", "  ")
	if err := enc.Encode(raw); err != nil {
		t.Fatalf("encode reference: %v", err)
	}
	if buf.String() != want.String() {
		t.Errorf("json output changed:\nold: %s\nnew: %s", want.String(), buf.String())
	}
}

func TestCloseAllExitedJSONLIds(t *testing.T) {
	var buf bytes.Buffer
	raw := json.RawMessage(`{"count":3,"children":["c_a","c_b","c_c"]}`)
	if err := renderCloseAllExited(&buf, raw, outputJSONL); err != nil {
		t.Fatalf("renderCloseAllExited: %v", err)
	}
	if buf.String() != "\"c_a\"\n\"c_b\"\n\"c_c\"\n" {
		t.Errorf("jsonl output = %q, want one id per line", buf.String())
	}

	// A zero-count response carries no children — zero lines.
	var empty bytes.Buffer
	if err := renderCloseAllExited(&empty, json.RawMessage(`{"count":0}`), outputJSONL); err != nil {
		t.Fatalf("renderCloseAllExited zero: %v", err)
	}
	if empty.Len() != 0 {
		t.Errorf("zero-count jsonl output = %q, want nothing", empty.String())
	}
}

// ─── review-close harness: a framed fake daemon plus a Connect stub ──────────
//
// runClose reaches the daemon on TWO planes: the close itself rides the
// framed protocol (mustDial) and the post-close review rides Connect
// (newConnectEndpoint). The harness below fakes both in the layout the real
// client derives: the profile's Socket names controller.sock, and
// connectSocketFor treats connect.sock as its SIBLING — so the framed fake
// listens on controller.sock and the Connect stub on connect.sock, in a
// short directory (unix socket paths cap at ~104 bytes).

// fakeFramedDaemon answers every ctrl_* request with success and records
// what arrived, so a test can assert the close itself actually happened.
type fakeFramedDaemon struct {
	mu        sync.Mutex
	list      protocol.ListResponseData
	allExited protocol.ForgetAllExitedResponseData
	kills     []string
	forgets   []string
}

func (f *fakeFramedDaemon) forgotten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgets...)
}

func (f *fakeFramedDaemon) handleConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewScanner(conn)
	w := bufio.NewWriter(conn)
	for r.Scan() {
		var hdr struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(r.Bytes(), &hdr) != nil {
			continue
		}
		f.mu.Lock()
		var data any
		switch hdr.Type {
		case protocol.TypeCtrlList:
			data = f.list
		case protocol.TypeCtrlKill:
			f.kills = append(f.kills, requestField(r.Bytes(), "childId"))
			data = protocol.KillResponseData{ExitCode: intPtr(0), DurationMs: 1}
		case protocol.TypeCtrlForget:
			f.forgets = append(f.forgets, requestField(r.Bytes(), "childId"))
			data = struct{}{}
		case protocol.TypeCtrlForgetAllExited:
			data = f.allExited
		default:
			data = struct{}{}
		}
		f.mu.Unlock()
		payload, err := json.Marshal(data)
		if err != nil {
			continue
		}
		resp, err := json.Marshal(protocol.Response{
			Type: protocol.TypeCtrlResponse, Command: hdr.Type, ID: hdr.ID, Success: true, Data: payload,
		})
		if err != nil {
			continue
		}
		if _, err := w.Write(resp); err != nil {
			return
		}
		if err := w.WriteByte('\n'); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// requestField pulls one top-level string field out of a framed request.
func requestField(raw []byte, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}

func serveFramedDaemon(t *testing.T, sockPath string, f *fakeFramedDaemon) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handleConn(conn)
		}
	}()
}

// reviewStubControl serves the two review verbs and records what arrived.
// forbidReview turns any ConversationReview call into a test failure — the
// "close with neither flag never dials Connect at all" assertion.
type reviewStubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	t            *testing.T
	forbidReview bool

	mu            sync.Mutex
	reviewCalls   []*rafikiv1.ConversationReviewRequest
	findingsCalls []*rafikiv1.ConversationFindingsRequest
	reviewResp    *rafikiv1.ConversationReviewResponse
	reviewErr     error
	findingsResp  *rafikiv1.ConversationFindingsResponse
	findingsErr   error
}

func (s *reviewStubControl) ConversationReview(
	_ context.Context,
	req *connect.Request[rafikiv1.ConversationReviewRequest],
) (*connect.Response[rafikiv1.ConversationReviewResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forbidReview && s.t != nil {
		s.t.Errorf("ConversationReview called unexpectedly: %v", req.Msg)
	}
	s.reviewCalls = append(s.reviewCalls, req.Msg)
	if s.reviewErr != nil {
		return nil, s.reviewErr
	}
	return connect.NewResponse(s.reviewResp), nil
}

func (s *reviewStubControl) ConversationFindings(
	_ context.Context,
	req *connect.Request[rafikiv1.ConversationFindingsRequest],
) (*connect.Response[rafikiv1.ConversationFindingsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findingsCalls = append(s.findingsCalls, req.Msg)
	if s.findingsErr != nil {
		return nil, s.findingsErr
	}
	return connect.NewResponse(s.findingsResp), nil
}

func (s *reviewStubControl) reviews() []*rafikiv1.ConversationReviewRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ConversationReviewRequest(nil), s.reviewCalls...)
}

func (s *reviewStubControl) findings() []*rafikiv1.ConversationFindingsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.ConversationFindingsRequest(nil), s.findingsCalls...)
}

// newReviewHarness wires an isolated profile at a framed fake on
// controller.sock with a Connect stub on the sibling connect.sock, and points
// the process at it.
func newReviewHarness(t *testing.T) (*fakeFramedDaemon, *reviewStubControl) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	dir, err := os.MkdirTemp("", "rv")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")

	daemon := &fakeFramedDaemon{}
	serveFramedDaemon(t, controlSock, daemon)
	stub := &reviewStubControl{t: t}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, filepath.Join(dir, "connect.sock"), routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: controlSock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
	return daemon, stub
}

// captureStderr swaps os.Stderr for a temp file and returns a reader for
// whatever was written to it. Restore happens at cleanup, before any later
// test runs.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	old := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		f.Close()
	})
	return func() string {
		// os.Stderr writes go straight through write(2), so the bytes are
		// already visible to ReadFile via the page cache; Sync is only a
		// durability flush and its failure cannot change what the test reads.
		_ = f.Sync()
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("read captured stderr: %v", err)
		}
		return string(b)
	}
}

// ─── the tests ──────────────────────────────────────────────────────────────

// Closing with neither flag never dials Connect at all — the fake fails the
// test if ConversationReview is reached.
func TestCloseReviewDefaultOff(t *testing.T) {
	daemon, stub := newReviewHarness(t)
	stub.forbidReview = true

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if calls := stub.reviews(); len(calls) != 0 {
		t.Errorf("ConversationReview called %d time(s) with no --review flag", len(calls))
	}
	// The close itself still went through both framed steps.
	if got := daemon.forgotten(); len(got) != 1 || got[0] != "c_1" {
		t.Errorf("ctrl_forget for %v, want [c_1]", got)
	}
}

// --review sends one ConversationReview naming exactly the closed id.
func TestCloseReviewSendsRequestForClosedID(t *testing.T) {
	daemon, stub := newReviewHarness(t)
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	}}}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close --review: %v", err)
	}

	calls := stub.reviews()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationReview calls, want 1", len(calls))
	}
	if ids := calls[0].GetConversationIds(); len(ids) != 1 || ids[0] != "c_1" {
		t.Errorf("ConversationIds = %v, want [c_1]", ids)
	}
	// No review config in the isolated config dir and no env override, so the
	// request must stay all-optional: the daemon decides everything.
	if calls[0].Model != nil || calls[0].Profile != nil || calls[0].BudgetUsd != nil || calls[0].MinTurns != nil {
		t.Errorf("all-optional request carries config fields: %+v", calls[0])
	}
	if got := daemon.forgotten(); len(got) != 1 || got[0] != "c_1" {
		t.Errorf("ctrl_forget for %v, want [c_1]", got)
	}
}

// --no-review is the explicit no-op spelling of the default.
func TestCloseNoReviewIsExplicitNoOp(t *testing.T) {
	daemon, stub := newReviewHarness(t)
	stub.forbidReview = true

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--no-review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close --no-review: %v", err)
	}
	if calls := stub.reviews(); len(calls) != 0 {
		t.Errorf("ConversationReview called under --no-review: %v", calls)
	}
	if got := daemon.forgotten(); len(got) != 1 || got[0] != "c_1" {
		t.Errorf("ctrl_forget for %v, want [c_1]", got)
	}
}

// A failing review must never fail the close: runClose returns nil, the
// child was still closed, and the failure surfaces as the per-id stderr
// note with the exact format the brief pins.
func TestCloseReviewFailureNeverFailsTheClose(t *testing.T) {
	daemon, stub := newReviewHarness(t)
	stub.reviewErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown model"))
	stderr := captureStderr(t)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close must not fail because its review failed: %v", err)
	}

	if got := daemon.forgotten(); len(got) != 1 || got[0] != "c_1" {
		t.Errorf("ctrl_forget for %v, want [c_1] — the review failure swallowed the close", got)
	}
	out := stderr()
	if !strings.HasPrefix(out, "review c_1: ") {
		t.Errorf("stderr = %q, want a \"review c_1: <note>\" line", out)
	}
	if !strings.Contains(out, "unknown model") {
		t.Errorf("stderr %q does not carry the daemon's error text", out)
	}
}

// --all-exited --review reviews EVERY id the close reported, in one batched
// request — not one request per id.
func TestCloseAllExitedReviewBatchesEveryClosedID(t *testing.T) {
	daemon, stub := newReviewHarness(t)
	daemon.allExited = protocol.ForgetAllExitedResponseData{Count: 2, Children: []string{"c_1", "c_2"}}
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	}}}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"--all-exited", "--review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close --all-exited --review: %v", err)
	}

	calls := stub.reviews()
	if len(calls) != 1 {
		t.Fatalf("got %d ConversationReview calls, want one batched request", len(calls))
	}
	if ids := calls[0].GetConversationIds(); len(ids) != 2 || ids[0] != "c_1" || ids[1] != "c_2" {
		t.Errorf("ConversationIds = %v, want [c_1 c_2]", ids)
	}
}

// A per-id non-enqueued status is the same stderr note shape as a
// whole-request failure: "review <id>: <status text>".
func TestCloseReviewNotesNonEnqueuedStatuses(t *testing.T) {
	_, stub := newReviewHarness(t)
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL,
	}}}
	stderr := captureStderr(t)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close --review: %v", err)
	}
	if out := stderr(); out != "review c_1: queue full\n" {
		t.Errorf("stderr = %q, want \"review c_1: queue full\\n\"", out)
	}
}
