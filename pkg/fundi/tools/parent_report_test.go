package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"
)

// prFakeReporter records what the tool layer asked the daemon's ParentReporter
// to do. It enforces nothing — which callers may report, and what a report
// does to the event buffer, live on the daemon side (scriptHub), not here.
type prFakeReporter struct {
	reportErr    error
	setResultErr error

	reported []struct {
		Kind    string
		Message string
	}
	results []string
}

func (f *prFakeReporter) Report(_ context.Context, kind, message string) error {
	if f.reportErr != nil {
		return f.reportErr
	}
	f.reported = append(f.reported, struct {
		Kind    string
		Message string
	}{kind, message})
	return nil
}

func (f *prFakeReporter) SetResult(_ context.Context, result string) error {
	if f.setResultErr != nil {
		return f.setResultErr
	}
	f.results = append(f.results, result)
	return nil
}

// prRun materializes just the two parent tools with rep bound — the way the
// daemon adapter binds one — and executes the named one. It returns the
// registry's raw (text, error): the exact strings the model sees.
func prRun(t *testing.T, rep *prFakeReporter, name, input string) (string, error) {
	t.Helper()
	reg := DefaultBlueprint.MaterializeOnly(ToolOpts{Parent: rep}, []string{"agent_report", "agent_result"})
	return reg.Execute(context.Background(), name, json.RawMessage(input))
}

// TestAgentReportDefaultsKindToMessage: an empty kind means "message", and
// the default lives ONLY here at the tool — the daemon never fills one in
// (zero-value-trap rule), so an omitted kind must never reach it empty.
func TestAgentReportDefaultsKindToMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{}
	out, err := prRun(t, rep, "agent_report", `{"message":"picking the library up"}`)
	c.NoError(err, "agent_report with no kind")
	c.Eq("reported\n", out, "result text")
	c.Len(rep.reported, 1, "Report calls")
	c.Eq("message", rep.reported[0].Kind, "defaulted kind")
	c.Eq("picking the library up", rep.reported[0].Message, "message")
	c.Empty(rep.results, "agent_report must not touch SetResult")
}

// TestAgentReportRefusesUnknownKind: ReportKinds is the whole vocabulary, and
// the refusal names it — the model can recover on its next call.
func TestAgentReportRefusesUnknownKind(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{}
	_, err := prRun(t, rep, "agent_report", `{"kind":"status","message":"x"}`)
	c.EqualError(err, "agent_report: kind must be one of message, warning, question, progress")
	c.Empty(rep.reported, "a refused kind must not reach Report")
}

// TestAgentReportRequiresMessage: a note with nothing in it is a wasted
// notification slot on the parent.
func TestAgentReportRequiresMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{}
	_, err := prRun(t, rep, "agent_report", `{"kind":"warning"}`)
	c.EqualError(err, "agent_report: message is required")
	c.Empty(rep.reported, "an empty message must not reach Report")
}

// TestAgentReportPassesKindAndMessage: the tool is a pass-through once the
// input is valid — it adds nothing to, and takes nothing from, the note.
func TestAgentReportPassesKindAndMessage(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, kind := range []string{"warning", "question", "progress"} {
		rep := &prFakeReporter{}
		out, err := prRun(t, rep, "agent_report", `{"kind":"`+kind+`","message":"m-`+kind+`"}`)
		c.NoError(err, "agent_report kind=%s", kind)
		c.Eq("reported\n", out, "result text for kind=%s", kind)
		c.Len(rep.reported, 1, "Report calls for kind=%s", kind)
		c.Eq(kind, rep.reported[0].Kind, "kind for kind=%s", kind)
		c.Eq("m-"+kind, rep.reported[0].Message, "message for kind=%s", kind)
	}
}

// TestAgentReportWrapsReportError: the daemon's own failure is wrapped with
// the tool name, so the model can tell which call failed.
func TestAgentReportWrapsReportError(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{reportErr: errors.New("event log unavailable")}
	_, err := prRun(t, rep, "agent_report", `{"message":"x"}`)
	c.EqualError(err, "agent_report: event log unavailable")
}

// TestAgentResultRequiresResult: an empty result is nothing to settle on.
func TestAgentResultRequiresResult(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{}
	_, err := prRun(t, rep, "agent_result", `{}`)
	c.EqualError(err, "agent_result: result is required")
	c.Empty(rep.results, "an empty result must not reach SetResult")
}

// TestAgentResultPassesResult: the verdict reaches SetResult verbatim, and
// agent_result never touches Report.
func TestAgentResultPassesResult(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{}
	out, err := prRun(t, rep, "agent_result", `{"result":"DONE — two findings, one concern"}`)
	c.NoError(err, "agent_result")
	c.Eq("result recorded; it rides your next settle notification\n", out, "result text")
	c.Len(rep.results, 1, "SetResult calls")
	c.Eq("DONE — two findings, one concern", rep.results[0], "result")
	c.Empty(rep.reported, "agent_result must not touch Report")
}

// TestAgentResultWrapsSetResultError: same wrap rule as agent_report.
func TestAgentResultWrapsSetResultError(t *testing.T) {
	c := assert.NewCollecting(t)
	rep := &prFakeReporter{setResultErr: errors.New("lease lost")}
	_, err := prRun(t, rep, "agent_result", `{"result":"DONE"}`)
	c.EqualError(err, "agent_result: lease lost")
}

// TestParentToolsDeclineWithoutReporter: a caller with no parent capability
// gets neither tool registered — the SkillBlueprint rule (decline, don't
// register something that can only answer "not configured"), not the Tasks
// rule.
func TestParentToolsDeclineWithoutReporter(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, bp := range []Tool{&AgentReportBlueprint{}, &AgentResultBlueprint{}} {
		tool, err := bp.(Materializer).Materialize(ToolOpts{})
		c.NoError(err, "%s: Materialize", bp.Name())
		c.Nil(tool, "%s must decline (nil, nil) without a Parent", bp.Name())
	}

	// And decline means ABSENT from the registry, not present-and-failing.
	reg := DefaultBlueprint.MaterializeAll(ToolOpts{Cwd: t.TempDir()})
	for _, name := range []string{"agent_report", "agent_result"} {
		_, err := reg.Execute(context.Background(), name, json.RawMessage(`{}`))
		c.Error(err, "%s must not be registered without a Parent", name)
		if err != nil {
			c.StrContains(err.Error(), "unknown tool", "%s absence message", name)
		}
	}
}
