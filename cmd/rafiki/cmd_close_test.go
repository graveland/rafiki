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

	"github.com/multigres/testkit/assert"
)

func TestCloseCommandKeepsForgetAsAnAlias(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newCloseCmd()
	c.Eq("close", cmd.Name(), "Name()")
	var hasForget, hasRM bool
	for _, a := range cmd.Aliases {
		switch a {
		case "forget":
			hasForget = true
		case "rm":
			hasRM = true
		}
	}
	c.True(hasForget, "`forget` must stay an alias: it is in muscle memory and in scripts")
	c.True(hasRM, "`rm` was an alias before the rename and must survive it")
}

func TestCloseCmd_HasStopTimeoutFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newCloseCmd()
	c.NotNil(cmd.Flags().Lookup("shutdown-timeout"), "--shutdown-timeout missing: close must be able to override the stop-first step's timeout")
	c.NotNil(cmd.Flags().Lookup("kill-timeout"), "--kill-timeout missing: close must be able to override the stop-first step's timeout")
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
			c := assert.NewCollecting(t)
			var buf bytes.Buffer
			c.Require().NoError(renderCloseAllExited(&buf, &rafikiv1.CloseAllExitedResponse{ChildIds: tc.ids}, outputTable), "renderCloseAllExited")
			c.Eq(tc.want, buf.String(), "output")
		})
	}
}

// JSON mode is the response's canonical protojson: the ids ride childIds (the
// framed payload's separate count is derivable from the list and the wire
// dropped it), and int64-free shape means no string-quoting surprises.
func TestCloseAllExitedJSONShape(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(renderCloseAllExited(&buf, &rafikiv1.CloseAllExitedResponse{ChildIds: []string{"a", "b"}}, outputJSON), "renderCloseAllExited")
	const want = `{
  "childIds": [
    "a",
    "b"
  ]
}
`
	c.Eq(want, buf.String(), "json output =\n")
}

func TestCloseAllExitedJSONLIds(t *testing.T) {
	c := assert.NewCollecting(t)
	var buf bytes.Buffer
	c.Require().NoError(renderCloseAllExited(&buf,
		&rafikiv1.CloseAllExitedResponse{ChildIds: []string{"c_a", "c_b", "c_c"}}, outputJSONL), "renderCloseAllExited")
	c.Eq("\"c_a\"\n\"c_b\"\n\"c_c\"\n", buf.String(), "jsonl output")

	// A zero-id response — zero lines.
	var empty bytes.Buffer
	c.Require().NoError(renderCloseAllExited(&empty, &rafikiv1.CloseAllExitedResponse{}, outputJSONL), "renderCloseAllExited zero")
	c.Eq(0, empty.Len(), "zero-count jsonl output = %q, want nothing", empty.String())
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
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	dir, err := os.MkdirTemp("", "rv")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	stub := &reviewStubControl{t: t}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
	return stub
}

// captureStderr swaps os.Stderr for a temp file and returns a reader for
// whatever was written to it. Restore happens at cleanup, before any later
// test runs.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	c := assert.NewAborting(t)
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	c.NoError(err, "CreateTemp")
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
		c.NoError(err, "read captured stderr")
		return string(b)
	}
}

// ─── the tests ──────────────────────────────────────────────────────────────

// Closing with neither flag never reviews — the stub fails the test if
// ConversationReview is reached.
func TestCloseReviewDefaultOff(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.forbidReview = true

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	c.Require().NoError(cmd.Execute(), "close")
	calls := stub.reviews()
	c.Empty(calls, "ConversationReview called %d time(s) with no --review flag", len(calls))
	// The close itself still went through.
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// --review sends one ConversationReview naming exactly the closed id.
func TestCloseReviewSendsRequestForClosedID(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newReviewHarness(t)
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	}}}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	c.NoError(cmd.Execute(), "close --review")

	calls := stub.reviews()
	c.Len(calls, 1, "got %d ConversationReview calls, want 1", len(calls))
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
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.forbidReview = true

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--no-review"})
	c.Require().NoError(cmd.Execute(), "close --no-review")
	c.Empty(stub.reviews(), "ConversationReview called under --no-review")
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// A failing review must never fail the close: runClose returns nil, the
// child was still closed, and the failure surfaces as the per-id stderr
// note with the exact format the brief pins.
func TestCloseReviewFailureNeverFailsTheClose(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.reviewErr = connect.NewError(connect.CodeFailedPrecondition, errors.New("unknown model"))
	stderr := captureStderr(t)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	c.Require().NoError(cmd.Execute(), "close must not fail because its review failed")

	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1] — the review failure swallowed the close", got)
	}
	out := stderr()
	if !strings.HasPrefix(out, "review c_1: ") {
		t.Errorf("stderr = %q, want a \"review c_1: <note>\" line", out)
	}
	c.StrContains(out, "unknown model", "stderr")
}

// --all-exited --review reviews EVERY id the close reported, in one batched
// request — not one request per id.
func TestCloseAllExitedReviewBatchesEveryClosedID(t *testing.T) {
	c := assert.NewAborting(t)
	stub := newReviewHarness(t)
	stub.closeAllIds = []string{"c_1", "c_2"}
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_ENQUEUED,
	}}}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"--all-exited", "--review"})
	c.NoError(cmd.Execute(), "close --all-exited --review")

	calls := stub.reviews()
	c.Len(calls, 1, "got %d ConversationReview calls, want one batched request", len(calls))
	if ids := calls[0].GetConversationIds(); len(ids) != 2 || ids[0] != "c_1" || ids[1] != "c_2" {
		t.Errorf("ConversationIds = %v, want [c_1 c_2]", ids)
	}
}

// A per-id non-enqueued status is the same stderr note shape as a
// whole-request failure: "review <id>: <status text>".
func TestCloseReviewNotesNonEnqueuedStatuses(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.reviewResp = &rafikiv1.ConversationReviewResponse{Accepted: []*rafikiv1.ConversationReviewAccept{{
		ConversationId: "c_1", Status: rafikiv1.ReviewAcceptStatus_REVIEW_ACCEPT_STATUS_QUEUE_FULL,
	}}}
	stderr := captureStderr(t)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1", "--review"})
	c.Require().NoError(cmd.Execute(), "close --review")
	out := stderr()
	c.Eq("review c_1: queue full\n", out, "stderr = %q, want \"review c_1: queue full\\n\"", out)
}

// ─── the Connect close path ──────────────────────────────────────────────────

// A child the daemon already reports exited skips the kill entirely: the
// pre-check sees status=exited and goes straight to Close.
func TestCloseExitedChildSkipsKill(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "exited"}

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	c.Require().NoError(cmd.Execute(), "close")
	c.Empty(stub.killCalls, "Kill called")
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
	assert.NewAborting(t).NoError(cmd.Execute(), "close")
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
	assert.NewAborting(t).NoError(cmd.Execute(), "a child_exited kill must proceed to close, got")
	if got := stub.closeCalls; len(got) != 1 || got[0] != "c_1" {
		t.Errorf("Close for %v, want [c_1]", got)
	}
}

// The same code with a DIFFERENT reason is a failure, not a close: this is
// exactly what keying on the reason (instead of connect.CodeOf) buys.
func TestCloseKillInGraceIsAFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "streaming"}
	stub.killErr = rpcreason.Attach(
		connect.NewError(connect.CodeFailedPrecondition, errors.New("child in grace")),
		protocol.ErrChildInGrace)

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	c.Require().Error(cmd.Execute(), "a child_in_grace kill must fail the close, got nil")
	c.Empty(stub.closeCalls, "Close for")
}

// A kill failure carrying no reason at all fails the close.
func TestCloseKillUnknownReasonFails(t *testing.T) {
	c := assert.NewCollecting(t)
	stub := newReviewHarness(t)
	stub.childStatus = map[string]string{"c_1": "streaming"}
	stub.killErr = connect.NewError(connect.CodeInternal, errors.New("boom"))

	cmd := newCloseCmd()
	cmd.SetArgs([]string{"c_1"})
	c.Require().Error(cmd.Execute(), "an unreasoned kill failure must fail the close, got nil")
	c.Empty(stub.closeCalls, "Close for")
}
