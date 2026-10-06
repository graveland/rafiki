// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// SendStepRunner runs a send's steps at send time and renders them. The
// daemon implements it; connectapi only converts and forwards. Errors it
// returns are already *connect.Error and are returned to the caller as-is.
type SendStepRunner interface {
	RunSendSteps(ctx context.Context, callerID, targetID string, steps []protocol.SendStep) (rendered string, summaries []protocol.StepSummary, err error)
}

// SetSendStepRunner attaches the send-step runner. A nil runner is refused
// rather than stored: storing the address of a nil interface would make the
// handler's Load return a non-nil pointer to a nil runner, defeating the
// Unavailable path and nil-panicking the first call (the rule
// TestSetSkillManagerNilIsRefused pins).
func (s *Server) SetSendStepRunner(r SendStepRunner) {
	if r == nil {
		return
	}
	s.sendSteps.Store(&r)
}

// sendStepRunner resolves the wired runner, nil when none was attached —
// Send fails closed on that rather than reporting success for steps that ran
// nowhere.
func (s *Server) sendStepRunner() SendStepRunner {
	if p := s.sendSteps.Load(); p != nil {
		return *p
	}
	return nil
}

// sendStepsFromWire converts the request's steps to the protocol shape the
// runner executes. Refusals are per step, 1-indexed in the caller's terms:
// a missing site and a missing kind are caller errors, refused with
// CodeInvalidArgument — never defaulted, because a zero-valued step that
// defaulted to one side would silently run with authority the sender did not
// name.
func sendStepsFromWire(steps []*rafikiv1.SendStep) ([]protocol.SendStep, error) {
	out := make([]protocol.SendStep, 0, len(steps))
	for i, st := range steps {
		var site protocol.StepSite
		switch st.GetWhere() {
		case rafikiv1.StepSite_STEP_SITE_CHILD:
			site = protocol.StepSiteChild
		case rafikiv1.StepSite_STEP_SITE_SENDER:
			site = protocol.StepSiteSender
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: where is required (child or sender)", i+1))
		}

		step := protocol.SendStep{Where: site, Echo: st.GetEcho()}
		switch k := st.GetKind().(type) {
		case *rafikiv1.SendStep_Read:
			step.Read = &protocol.PrefillRead{
				Path:  k.Read.GetPath(),
				Start: int(k.Read.GetStart()),
				End:   int(k.Read.GetEnd()),
			}
		case *rafikiv1.SendStep_Bash:
			timeout := k.Bash.GetTimeout()
			if timeout != nil {
				if err := timeout.CheckValid(); err != nil {
					return nil, connect.NewError(connect.CodeInvalidArgument,
						fmt.Errorf("step %d: timeout: %w", i+1, err))
				}
				if timeout.AsDuration() < 0 {
					return nil, connect.NewError(connect.CodeInvalidArgument,
						fmt.Errorf("step %d: timeout must not be negative", i+1))
				}
			}
			step.Bash = &protocol.BashStep{
				Command:   k.Bash.GetCommand(),
				TimeoutMs: int(timeout.AsDuration() / time.Millisecond),
			}
		case *rafikiv1.SendStep_PymoduleRun:
			step.PymoduleRun = &protocol.PymoduleRunStep{
				Repo:    k.PymoduleRun.GetRepo(),
				Script:  k.PymoduleRun.GetScript(),
				Modules: k.PymoduleRun.GetModules(),
				Args:    k.PymoduleRun.GetArgs(),
				Cwd:     k.PymoduleRun.GetCwd(),
			}
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: exactly one of read, bash, pymodule_run is required", i+1))
		}
		out = append(out, step)
	}
	return out, nil
}

// stepSummariesToWire converts the runner's summaries back onto the wire.
// A summary never carries a step's output — the runner decides what echo,
// if any, the sender sees.
func stepSummariesToWire(summaries []protocol.StepSummary) []*rafikiv1.StepSummary {
	out := make([]*rafikiv1.StepSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, &rafikiv1.StepSummary{
			Index:     int32(s.Index),
			Tool:      s.Tool,
			Where:     stepSiteToWire(s.Where),
			Outcome:   s.Outcome,
			Bytes:     int64(s.Bytes),
			Truncated: s.Truncated,
			Echo:      s.Echo,
		})
	}
	return out
}

// stepSiteToWire maps the protocol site back to its enum value; an
// unrecognized site renders as UNSPECIFIED rather than guessing one, for the
// same reason the wire refuses it inbound.
func stepSiteToWire(site protocol.StepSite) rafikiv1.StepSite {
	switch site {
	case protocol.StepSiteChild:
		return rafikiv1.StepSite_STEP_SITE_CHILD
	case protocol.StepSiteSender:
		return rafikiv1.StepSite_STEP_SITE_SENDER
	default:
		return rafikiv1.StepSite_STEP_SITE_UNSPECIFIED
	}
}

// errSendStepsUnwired is Send's fail-closed answer when steps arrived but no
// runner was attached: the request is refused, never half-delivered.
var errSendStepsUnwired = connect.NewError(connect.CodeUnavailable,
	errors.New("send steps not yet wired"))
