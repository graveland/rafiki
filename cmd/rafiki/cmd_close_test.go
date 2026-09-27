// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
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
	"go.graveland.dev/rafiki/pkg/rpcreason"
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
		ids  []string
		want string
	}{
		{"three closed", []string{"a", "b", "c"}, "closed 3 exited children\n"},
		{"zero closed", nil, "no exited children to close\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderCloseAllExited(&buf, &rafikiv1.CloseAllExitedResponse{ChildIds: tc.ids}, outputTable); err != nil {
				t.Fatalf("renderCloseAllExited: %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("output = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// JSON mode is the response's canonical protojson: the ids ride childIds (the
// framed payload's separate count is derivable from the list and the wire
// dropped it), and int64-free shape means no string-quoting surprises.
func TestCloseAllExitedJSONShape(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCloseAllExited(&buf, &rafikiv1.CloseAllExitedResponse{ChildIds: []string{"a", "b"}}, outputJSON); err != nil {
		t.Fatalf("renderCloseAllExited: %v", err)
	}
	const want = `{
  "childIds": [
    "a",
    "b"
  ]
}
`
	if buf.String() != want {
		t.Errorf("json output =\n%s\nwant\n%s", buf.String(), want)
	}
}

func TestCloseAllExitedJSONLIds(t *testing.T) {
	var buf bytes.Buffer
	if err := renderCloseAllExited(&buf,
		&rafikiv1.CloseAllExitedResponse{ChildIds: []string{"c_a", "c_b", "c_c"}}, outputJSONL); err != nil {
		t.Fatalf("renderCloseAllExited: %v", err)
	}
	if buf.String() != "\"c_a\"\n\"c_b\"\n\"c_c\"\n" {
		t.Errorf("jsonl output = %q, want one id per line", buf.String())
	}

	// A zero-id response — zero lines.
	var empty bytes.Buffer
	if err := renderCloseAllExited(&empty, &rafikiv1.CloseAllExitedResponse{}, outputJSONL); err != nil {
		t.Fatalf("renderCloseAllExited zero: %v", err)
	}
	if empty.Len() != 0 {
		t.Errorf("zero-count jsonl output = %q, want nothing", empty.String())
	}
}

func intPtr(i int) *int { return &i }

// reviewStubControl serves the review verbs and the child operations the
// Connect close path needs (GetChild for the pre-check, Kill, Close,
// CloseAllExited) and records what arrived. forbidReview turns any
// ConversationReview call into a test failure — the "close with neither flag
// never reviews" assertion.
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

	// The close path's fakes. childStatus is what GetChild answers per id
	// (default exited — the shape `close <id>` usually meets); killErr and
	// closeErr are returned by Kill/Close when set; closeAllIds is
	// CloseAllExited's answer.
	childStatus  map[string]string
	killErr      error
	closeErr     error
	closeAllIds  []string
	killCalls    []string
	closeCalls   []string
	allExitedReq []*rafikiv1.CloseAllExitedRequest
}

func (s *reviewStubControl) GetChild(
	_ context.Context,
	req *connect.Request[rafikiv1.GetChildRequest],
) (*connect.Response[rafikiv1.GetChildResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.childStatus[req.Msg.GetChildId()]
	if !ok {
		status = "exited"
	}
	return connect.NewResponse(&rafikiv1.GetChildResponse{
		Child: &rafikiv1.ChildSummary{ChildId: req.Msg.GetChildId(), Status: status},
	}), nil
}

func (s *reviewStubControl) Kill(
	_ context.Context,
	req *connect.Request[rafikiv1.KillRequest],
) (*connect.Response[rafikiv1.KillResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killCalls = append(s.killCalls, req.Msg.GetChildId())
	if s.killErr != nil {
		return nil, s.killErr
	}
	code := int32(0)
	return connect.NewResponse(&rafikiv1.KillResponse{
		ChildId: req.Msg.GetChildId(), ExitCode: &code, DurationMs: 1,
	}), nil
}

func (s *reviewStubControl) Close(
	_ context.Context,
	req *connect.Request[rafikiv1.CloseRequest],
) (*connect.Response[rafikiv1.CloseResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls = append(s.closeCalls, req.Msg.GetChildId())
	if s.closeErr != nil {
		return nil, s.closeErr
	}
	return connect.NewResponse(&rafikiv1.CloseResponse{ChildId: req.Msg.GetChildId()}), nil
}

func (s *reviewStubControl) CloseAllExited(
	_ context.Context,
	req *connect.Request[rafikiv1.CloseAllExitedRequest],
) (*connect.Response[rafikiv1.CloseAllExitedResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allExitedReq = append(s.allExitedReq, req.Msg)
	return connect.NewResponse(&rafikiv1.CloseAllExitedResponse{ChildIds: s.closeAllIds}), nil
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

// newReviewHarness wires an isolated profile at a Connect stub served on the
// profile's own socket — the one path the client dials — and points the
// process at it.
func newReviewHarness(t *testing.T) *reviewStubControl {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	dir, err := os.MkdirTemp("", "rv")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	stub := &reviewStubControl{t: t}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
	return stub
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

// Closing with neither flag never reviews — the stub fails the test if
// ConversationReview is reached.
func TestCloseReviewDefaultOff(t *testing.T) {
	stub := newReviewHarness(t)
	stub.forbidReview = true

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if calls := stub.reviews(); len(calls) != 0 {
		t.Errorf("ConversationReview called %d time(s) with no --review flag", len(calls))
	}
	// The close itself still went through.
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// --review sends one ConversationReview naming exactly the closed id.
func TestCloseReviewSendsRequestForClosedID(t *testing.T) {
	stub := newReviewHarness(t)
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
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// --no-review is the explicit no-op spelling of the default.
func TestCloseNoReviewIsExplicitNoOp(t *testing.T) {
	stub := newReviewHarness(t)
	stub.forbidReview = true

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--no-review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close --no-review: %v", err)
	}
	if calls := stub.reviews(); len(calls) != 0 {
		t.Errorf("ConversationReview called under --no-review: %v", calls)
	}
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// A failing review must never fail the close: runClose returns nil, the
// child was still closed, and the failure surfaces as the per-id stderr
// note with the exact format the brief pins.
func TestCloseReviewFailureNeverFailsTheClose(t *testing.T) {
	stub := newReviewHarness(t)
	stub.reviewErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown model"))
	stderr := captureStderr(t)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close must not fail because its review failed: %v", err)
	}

	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1] — the review failure swallowed the close", got)
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
	stub := newReviewHarness(t)
	stub.closeAllIds = []string{"c_1", "c_2"}
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
	stub := newReviewHarness(t)
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

// ─── the Connect close path ──────────────────────────────────────────────────

// A child the daemon already reports exited skips the kill entirely: the
// pre-check sees status=exited and goes straight to Close.
func TestCloseExitedChildSkipsKill(t *testing.T) {
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "exited"}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := stub.killCalls; len(got) != 0 {
		t.Errorf("Kill called %v, want none — an exited child needs no stop", got)
	}
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// A live child is killed first, then closed.
func TestCloseLiveChildStopsFirst(t *testing.T) {
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "streaming"}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := stub.killCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Kill for %v, want [c_1]", got)
	}
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// THE RETIREMENT RULING: the "kill says the child already exited → proceed to
// close" branch keys on the rafiki REASON riding the Connect error, never on
// the bare FailedPrecondition code — which child_in_grace and
// child_shutting_down share. The stub answers a live child (so the kill runs)
// but its Kill fails with the reason attached; close must proceed anyway.
func TestCloseKillSaysAlreadyExitedProceedsToClose(t *testing.T) {
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "streaming"}
	stub.killErr = rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child has already exited")),
		protocol.ErrChildExited)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a child_exited kill must proceed to close, got: %v", err)
	}
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// The same code with a DIFFERENT reason is a failure, not a close: this is
// exactly what keying on the reason (instead of connect.CodeOf) buys.
func TestCloseKillInGraceIsAFailure(t *testing.T) {
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "streaming"}
	stub.killErr = rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child in grace")),
		protocol.ErrChildInGrace)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("a child_in_grace kill must fail the close, got nil")
	}
	if got := stub.closeCalls; len(got) != 0 {
		t.Errorf("Close for %v, want none — the child is not stopped yet", got)
	}
}

// A kill failure carrying no reason at all fails the close.
func TestCloseKillUnknownReasonFails(t *testing.T) {
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "streaming"}
	stub.killErr = connect.NewError(connect.CodeInternal, errors.New("boom"))

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("an unreasoned kill failure must fail the close, got nil")
	}
	if got := stub.closeCalls; len(got) != 0 {
		t.Errorf("Close for %v, want none", got)
	}
}
