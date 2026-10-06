// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeSendStepRunner records what the Send handler asked it to run and what
// it answers with. render/outcome/err are the canned reply; caller/target/
// steps capture the call so the tests can pin the handler's conversion and
// scope plumbing.
type fakeSendStepRunner struct {
	called  bool
	caller  string
	target  string
	steps   []protocol.SendStep
	render  string
	outcome []protocol.StepSummary
	err     error
}

func (f *fakeSendStepRunner) RunSendSteps(
	_ context.Context, callerID, targetID string, steps []protocol.SendStep,
) (string, []protocol.StepSummary, error) {
	f.called = true
	f.caller, f.target, f.steps = callerID, targetID, steps
	if f.err != nil {
		return "", nil, f.err
	}
	return f.render, f.outcome, nil
}

// sendStepsChildScope is the per-child credential shape for the caller-id
// test: a fixed ChildID that authorizes every target the tests send to.
type sendStepsChildScope struct{ id string }

func (s sendStepsChildScope) ChildID() string { return s.id }

func (s sendStepsChildScope) Authorize(string) error { return nil }

func (s sendStepsChildScope) Subtree([]string) []protocol.ChildSummary { return nil }

func (s sendStepsChildScope) ConversationInScope(string) bool { return false }

// readStep, bashStep and pymoduleStep are one step of each kind with every
// field set, so the conversion test proves nothing is dropped.
func readStep(where rafikiv1.StepSite) *rafikiv1.SendStep {
	return &rafikiv1.SendStep{
		Where: where,
		Echo:  true,
		Kind:  &rafikiv1.SendStep_Read{Read: &rafikiv1.ReadStep{Path: "docs/x.md", Start: 3, End: 9}},
	}
}

func bashStep(where rafikiv1.StepSite) *rafikiv1.SendStep {
	return &rafikiv1.SendStep{
		Where: where,
		Kind:  &rafikiv1.SendStep_Bash{Bash: &rafikiv1.BashStep{Command: "go test ./...", Timeout: durationpb.New(2500 * time.Millisecond)}},
	}
}

func pymoduleStep(where rafikiv1.StepSite) *rafikiv1.SendStep {
	return &rafikiv1.SendStep{
		Where: where,
		Kind: &rafikiv1.SendStep_PymoduleRun{PymoduleRun: &rafikiv1.PymoduleRunStep{
			Repo:    "local",
			Script:  "analyze",
			Modules: []string{"m1", "m2"},
			Args:    []string{"--json"},
			Cwd:     "/tmp/w",
		}},
	}
}

// TestSendStepsWireConversion round-trips every field of all three step
// kinds through Send into the runner, and refuses a missing site and a
// missing kind with the exact per-step messages.
func TestSendStepsWireConversion(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)
	runner := &fakeSendStepRunner{render: "R"}
	s.SetSendStepRunner(runner)

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("msg"),
		Steps: []*rafikiv1.SendStep{
			readStep(rafikiv1.StepSite_STEP_SITE_CHILD),
			bashStep(rafikiv1.StepSite_STEP_SITE_SENDER),
			pymoduleStep(rafikiv1.StepSite_STEP_SITE_SENDER),
		},
	}))
	c.Require().NoError(err, "Send with all three step kinds")
	c.Require().Eq(3, len(runner.steps), "runner saw %d steps", len(runner.steps))
	c.EqDeep([]protocol.SendStep{
		{
			Where: protocol.StepSiteChild,
			Echo:  true,
			Read:  &protocol.PrefillRead{Path: "docs/x.md", Start: 3, End: 9},
		},
		{
			Where: protocol.StepSiteSender,
			Bash:  &protocol.BashStep{Command: "go test ./...", TimeoutMs: 2500},
		},
		{
			Where: protocol.StepSiteSender,
			PymoduleRun: &protocol.PymoduleRunStep{
				Repo: "local", Script: "analyze", Modules: []string{"m1", "m2"},
				Args: []string{"--json"}, Cwd: "/tmp/w",
			},
		},
	}, runner.steps, "steps did not round-trip")

	// UNSPECIFIED is refused, never defaulted.
	_, err = s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Steps:   []*rafikiv1.SendStep{bashStep(rafikiv1.StepSite_STEP_SITE_UNSPECIFIED)},
	}))
	c.EqualError(err, "invalid_argument: step 1: where is required (child or sender)",
		"unspecified site")

	// A step with no kind is refused too.
	_, err = s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Steps:   []*rafikiv1.SendStep{{Where: rafikiv1.StepSite_STEP_SITE_CHILD}},
	}))
	c.EqualError(err, "invalid_argument: step 1: exactly one of read, bash, pymodule_run is required",
		"kindless step")

	// The step index in the message is 1-based in the caller's terms.
	_, err = s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Steps: []*rafikiv1.SendStep{
			bashStep(rafikiv1.StepSite_STEP_SITE_SENDER),
			bashStep(rafikiv1.StepSite_STEP_SITE_UNSPECIFIED),
		},
	}))
	c.EqualError(err, "invalid_argument: step 2: where is required (child or sender)",
		"second step's index")
}

// TestSendStepsRefusesBadBashTimeout pins the proto-boundary guard: a bash
// step whose Duration exceeds the proto range, or is negative, is refused
// InvalidArgument before the runner sees it — the old int32 timeout_ms range
// refusal, now expressed as a Duration.
func TestSendStepsRefusesBadBashTimeout(t *testing.T) {
	c := assert.NewCollecting(t)
	for name, timeout := range map[string]*durationpb.Duration{
		"negative":     durationpb.New(-time.Second),
		"out of range": {Seconds: 315576000001},
	} {
		t.Run(name, func(t *testing.T) {
			acc := &fakeAccepter{}
			s := connectapi.NewServer(nil)
			s.SetInbox(acc)
			runner := &fakeSendStepRunner{}
			s.SetSendStepRunner(runner)
			_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
				ChildId: "c_1",
				Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
				Steps: []*rafikiv1.SendStep{{
					Where: rafikiv1.StepSite_STEP_SITE_SENDER,
					Kind:  &rafikiv1.SendStep_Bash{Bash: &rafikiv1.BashStep{Command: "x", Timeout: timeout}},
				}},
			}))
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "bad bash timeout")
			c.Eq(0, len(runner.steps), "a bad timeout reached the runner")
		})
	}
}

// TestSendStepsAppendRenderedTextAndReturnSummaries: the rendered output is
// appended to the message text and the summaries ride the response.
func TestSendStepsAppendRenderedTextAndReturnSummaries(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)
	s.SetSendStepRunner(&fakeSendStepRunner{
		render: "R",
		outcome: []protocol.StepSummary{{
			Index: 0, Tool: "read", Where: protocol.StepSiteChild,
			Outcome: "ok", Bytes: 120, Truncated: true, Echo: "first line",
		}},
	})

	resp, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("msg"),
		Steps:   []*rafikiv1.SendStep{readStep(rafikiv1.StepSite_STEP_SITE_CHILD)},
	}))
	c.Require().NoError(err, "Send with steps")
	c.Eq("msg\n\nR", acc.got.Text, "inbox text = %q, want rendered appended", acc.got.Text)
	c.Eq(1, len(resp.Msg.GetSteps()), "response steps")
	c.EqDeep([]*rafikiv1.StepSummary{{
		Index: 0, Tool: "read", Where: rafikiv1.StepSite_STEP_SITE_CHILD,
		Outcome: "ok", Bytes: 120, Truncated: true, Echo: "first line",
	}}, resp.Msg.GetSteps(), "response summaries did not round-trip")
}

// TestSendStepsWithEmptyTextUsesRenderedOnly: a send with no text blocks
// still carries the rendered output alone.
func TestSendStepsWithEmptyTextUsesRenderedOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)
	s.SetSendStepRunner(&fakeSendStepRunner{render: "R"})

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Steps:   []*rafikiv1.SendStep{bashStep(rafikiv1.StepSite_STEP_SITE_SENDER)},
	}))
	c.Require().NoError(err, "Send with steps and no text")
	c.Eq("R", acc.got.Text, "inbox text = %q, want the rendered output only", acc.got.Text)
}

// TestSendStepsCallerIDFromChildScope: the runner's callerID is the child
// scope's own id — the credential, never a request field — and "" for a
// caller with no child scope.
func TestSendStepsCallerIDFromChildScope(t *testing.T) {
	c := assert.NewCollecting(t)
	scoped := connectapi.NewServer(nil)
	scoped.SetInbox(&fakeAccepter{})
	runner := &fakeSendStepRunner{render: "R"}
	scoped.SetSendStepRunner(runner)
	scoped.SetChildScopeSource(func(context.Context) connectapi.ChildScope {
		return sendStepsChildScope{id: "c_caller"}
	})

	_, err := scoped.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("msg"),
		Steps:   []*rafikiv1.SendStep{readStep(rafikiv1.StepSite_STEP_SITE_CHILD)},
	}))
	c.Require().NoError(err, "Send with a child scope")
	c.Eq("c_caller", runner.caller, "callerID")
	c.Eq("c_1", runner.target, "targetID")

	operator := connectapi.NewServer(nil)
	operator.SetInbox(&fakeAccepter{})
	opRunner := &fakeSendStepRunner{render: "R"}
	operator.SetSendStepRunner(opRunner)
	_, err = operator.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("msg"),
		Steps:   []*rafikiv1.SendStep{readStep(rafikiv1.StepSite_STEP_SITE_CHILD)},
	}))
	c.Require().NoError(err, "Send without a child scope")
	c.Eq("", opRunner.caller, "callerID without a child scope")
}

// TestSendStepsRefusedWithAbort: ABORT carries no content, so steps with it
// are a caller error and the runner never runs.
func TestSendStepsRefusedWithAbort(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)
	runner := &fakeSendStepRunner{}
	s.SetSendStepRunner(runner)

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_ABORT,
		Steps:   []*rafikiv1.SendStep{bashStep(rafikiv1.StepSite_STEP_SITE_SENDER)},
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code = %v", err)
	c.False(runner.called, "runner ran for an ABORT send")
}

// TestSendStepsUnwiredRunnerIsUnavailable: steps with no runner fail closed
// (Unavailable), while a send without steps is delivered exactly as before.
func TestSendStepsUnwiredRunnerIsUnavailable(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	acc := &fakeAccepter{}
	s.SetInbox(acc)

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("msg"),
		Steps:   []*rafikiv1.SendStep{bashStep(rafikiv1.StepSite_STEP_SITE_SENDER)},
	}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code = %v", err)
	c.Eq("", acc.got.Text, "an unwired runner delivered something")

	// Without steps the same server delivers normally.
	resp, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("hello"),
	}))
	c.Require().NoError(err, "Send without steps")
	c.Eq("hello", acc.got.Text, "text")
	c.NotEq("", resp.Msg.GetMessageId(), "message id")
	c.Eq(0, len(resp.Msg.GetSteps()), "a stepless send has no summaries")
}

// TestSendStepsRunnerErrorPassesThrough: the runner's *connect.Error is the
// caller's answer, and nothing reaches the inbox.
func TestSendStepsRunnerErrorPassesThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)
	s.SetSendStepRunner(&fakeSendStepRunner{
		err: connect.NewError(connect.CodePermissionDenied, errors.New("bash refused by policy")),
	})

	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("msg"),
		Steps:   []*rafikiv1.SendStep{bashStep(rafikiv1.StepSite_STEP_SITE_SENDER)},
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code = %v", err)
	c.EqualError(err, "permission_denied: bash refused by policy", "the runner's error verbatim")
	c.Eq("", acc.got.ChildID, "the inbox received a message for a child")
	c.Eq("", acc.got.Text, "the inbox received text")
}

// TestSetSendStepRunnerNilIsRefused mirrors TestSetSkillManagerNilIsRefused:
// the setter must refuse nil rather than store the address of a nil
// interface. Handler-shaped here, like every external test: after the
// refusal a steps send still fails closed (Unavailable), and the positive
// control proves that refusal is the setter's nil check — a stored nil would
// have made the seam pass or panic instead.
func TestSetSendStepRunnerNilIsRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	acc := &fakeAccepter{}
	s.SetInbox(acc)
	s.SetSendStepRunner(nil)
	req := &rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Steps:   []*rafikiv1.SendStep{bashStep(rafikiv1.StepSite_STEP_SITE_SENDER)},
	}
	_, err := s.Send(context.Background(), connect.NewRequest(req))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err),
		"after SetSendStepRunner(nil): want CodeUnavailable, got %v", err)
	c.Eq("", acc.got.Text, "the nil-refused send was delivered")

	// The same server accepts a real runner afterwards.
	s.SetSendStepRunner(&fakeSendStepRunner{render: "R"})
	_, err = s.Send(context.Background(), connect.NewRequest(req))
	c.Require().NoError(err, "Send after a real runner")
	c.Eq("R", acc.got.Text, "text")
}
