// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// ssTestNonce is the fixed nonce every send-steps test renders with.
const ssTestNonce = "0011223344556677"

// ssScript is one Execute call's scripted answer: the events the handler
// sends, then (optionally) the error it returns — with no events that error
// fails the Execute call itself, with events it surfaces as stream.Err().
type ssScript struct {
	events []*executorpb.ExecuteResponse
	err    error
}

func ssRun(events ...*executorpb.ExecuteResponse) ssScript { return ssScript{events: events} }

func ssRunThenFail(err error, events ...*executorpb.ExecuteResponse) ssScript {
	return ssScript{events: events, err: err}
}

// ssScriptedExecutor implements executorpbconnect.ExecutorServiceHandler,
// recording every ExecuteRequest and replaying one script per Execute call in
// order. A call with no script left fails loudly: the runner must never
// execute a step the caller did not script.
type ssScriptedExecutor struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	mu      sync.Mutex
	reqs    []*executorpb.ExecuteRequest
	scripts []ssScript
}

func (h *ssScriptedExecutor) Execute(
	ctx context.Context,
	req *connect.Request[executorpb.ExecuteRequest],
	stream *connect.ServerStream[executorpb.ExecuteResponse],
) error {
	h.mu.Lock()
	i := len(h.reqs)
	h.reqs = append(h.reqs, req.Msg)
	h.mu.Unlock()
	if i >= len(h.scripts) {
		return fmt.Errorf("ssScriptedExecutor: no script for Execute call %d", i+1)
	}
	script := h.scripts[i]
	for _, ev := range script.events {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return script.err
}

// calls returns every recorded ExecuteRequest.
func (h *ssScriptedExecutor) calls() []*executorpb.ExecuteRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*executorpb.ExecuteRequest(nil), h.reqs...)
}

// ssScriptedServer serves one ssScriptedExecutor over httptest, the same way
// mcp_pymodule_run_test.go serves fakeExecuteHandler.
func ssScriptedServer(t *testing.T, scripts ...ssScript) (*ssScriptedExecutor, executorpbconnect.ExecutorServiceClient) {
	t.Helper()
	exec := &ssScriptedExecutor{scripts: scripts}
	mux := http.NewServeMux()
	path, handler := executorpbconnect.NewExecutorServiceHandler(exec)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return exec, executorpbconnect.NewExecutorServiceClient(srv.Client(), srv.URL)
}

// ssResultText builds a Result event carrying one text content block.
func ssResultText(text string) *executorpb.ExecuteResponse {
	return &executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Result{
			Result: &executorpb.Result{
				Content: []*executorpb.ContentBlock{
					{Block: &executorpb.ContentBlock_Text{Text: text}},
				},
			},
		},
	}
}

// ssFailureEvent builds a Failed event with the given code and message.
func ssFailureEvent(code executorpb.Failure_Code, msg string) *executorpb.ExecuteResponse {
	return &executorpb.ExecuteResponse{
		Event: &executorpb.ExecuteResponse_Failed{
			Failed: &executorpb.Failure{Code: code, Message: msg},
		},
	}
}

// ssFakePool stands in for *execpool.Pool: a live list and one scripted
// client per executor id. dialed records the ids ConnectClientFor was called
// with, in order, so a test can pin which executor a step ran on.
type ssFakePool struct {
	live    []execpool.LiveExecutor
	clients map[string]executorpbconnect.ExecutorServiceClient
	dialed  []string
}

func (f *ssFakePool) Live() []execpool.LiveExecutor { return f.live }

func (f *ssFakePool) ConnectClientFor(executorID string) (executorpbconnect.ExecutorServiceClient, error) {
	f.dialed = append(f.dialed, executorID)
	c, ok := f.clients[executorID]
	if !ok {
		return nil, fmt.Errorf("ssFakePool: no client for executor %s", executorID)
	}
	return c, nil
}

// ssSnapshots adapts a map into the runner's snap func.
func ssSnapshots(m map[string]childstore.Snapshot) func(string) (childstore.Snapshot, bool) {
	return func(id string) (childstore.Snapshot, bool) {
		s, ok := m[id]
		if !ok && id == "c-1" {
			// The default calling agent of these tests is unrestricted, so a
			// child step is never gated on a snapshot the test didn't write.
			return ssSnap("/w/caller", protocol.KindFundi, nil, false, false), true
		}
		return s, ok
	}
}

// ssRunner builds a sendStepRunner with the fixed test nonce and a map-backed
// snapshot source.
func ssRunner(pool pymodulePool, snaps map[string]childstore.Snapshot) *sendStepRunner {
	return &sendStepRunner{pool: pool, snap: ssSnapshots(snaps), nonce: func() string { return ssTestNonce }}
}

// ssSnap builds a Snapshot with the fields the runner reads; callers attach
// their own Labels.
func ssSnap(cwd, kind string, tools []string, noTools, noBuiltinTools bool) childstore.Snapshot {
	return childstore.Snapshot{
		Cwd: cwd, Kind: kind, Tools: tools,
		NoTools: noTools, NoBuiltinTools: noBuiltinTools,
	}
}

// ssFundi builds an unrestricted fundi-kind snapshot with executor and
// (optionally) workspace labels — the shape most send-steps tests need.
func ssFundi(cwd, executorID, workspaceID string) childstore.Snapshot {
	s := ssSnap(cwd, protocol.KindFundi, nil, false, false)
	labels := map[string]string{}
	if executorID != "" {
		labels["rafiki/executor"] = executorID
	}
	if workspaceID != "" {
		labels["rafiki/workspace"] = workspaceID
	}
	s.Labels = labels
	return s
}

// ssLive builds a LiveExecutor list naming id live.
func ssLive(ids ...string) []execpool.LiveExecutor {
	live := make([]execpool.LiveExecutor, len(ids))
	for i, id := range ids {
		live[i] = execpool.LiveExecutor{Executor: executors.Executor{ID: id}}
	}
	return live
}

func ssBashStep(where protocol.StepSite, command string) protocol.SendStep {
	return protocol.SendStep{Where: where, Bash: &protocol.BashStep{Command: command}}
}

func ssReadStep(where protocol.StepSite, path string, start, end int) protocol.SendStep {
	return protocol.SendStep{Where: where, Read: &protocol.PrefillRead{Path: path, Start: start, End: end}}
}

// ssErrMessage returns a connect error's bare message: Error() prefixes the
// code ("invalid_argument: …"), and these tests pin code and message
// separately, so the exact text is asserted on Message().
func ssErrMessage(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}

// TestSendStepsValidateBeforeRunning pins phase 1: a later step's refusal
// fires before ANY step runs, so a refused send leaves no side effects.
func TestSendStepsValidateBeforeRunning(t *testing.T) {
	c := assert.NewAborting(t)
	exec, client := ssScriptedServer(t, ssRun(ssResultText("must not run")))
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
	snaps := map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-1", "")}
	r := ssRunner(pool, snaps)

	steps := []protocol.SendStep{
		ssBashStep(protocol.StepSiteChild, "echo first"),
		ssReadStep(protocol.StepSiteChild, "reports/*.md", 0, 0), // glob: refused
	}
	_, _, err := r.RunSendSteps(context.Background(), "c-1", "t-1", steps)
	c.Error(err, "RunSendSteps error")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq("step 2: read takes one file, not a glob", ssErrMessage(err), "message")
	c.Empty(exec.calls(), "Execute calls recorded despite the step-2 refusal")
}

// TestSendStepsLimits pins each shape refusal's exact message.
func TestSendStepsLimits(t *testing.T) {
	c := assert.NewAborting(t)
	exec, client := ssScriptedServer(t)
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
	r := ssRunner(pool, map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-1", "")})

	seventeen := make([]protocol.SendStep, 17)
	for i := range seventeen {
		seventeen[i] = ssBashStep(protocol.StepSiteChild, "x")
	}
	cases := []struct {
		name string
		want string
		then func() []protocol.SendStep
	}{
		{"too many", "too many steps: 17 (max 16)", func() []protocol.SendStep { return seventeen }},
		{"bash timeout over max", "step 1: bash timeout_ms must be between 0 and 60000", func() []protocol.SendStep {
			s := ssBashStep(protocol.StepSiteChild, "x")
			s.Bash.TimeoutMs = 60001
			return []protocol.SendStep{s}
		}},
		{"bash timeout negative", "step 1: bash timeout_ms must be between 0 and 60000", func() []protocol.SendStep {
			s := ssBashStep(protocol.StepSiteChild, "x")
			s.Bash.TimeoutMs = -1
			return []protocol.SendStep{s}
		}},
		{"empty where", "step 1: where must be child or sender", func() []protocol.SendStep {
			return []protocol.SendStep{ssReadStep("", "/w/file.txt", 0, 0)}
		}},
		{"two kinds", "step 1: exactly one of read, bash, pymodule_run is required", func() []protocol.SendStep {
			s := ssBashStep(protocol.StepSiteChild, "x")
			s.Read = &protocol.PrefillRead{Path: "/w/file.txt"}
			return []protocol.SendStep{s}
		}},
		{"no kind", "step 1: exactly one of read, bash, pymodule_run is required", func() []protocol.SendStep {
			return []protocol.SendStep{{Where: protocol.StepSiteChild}}
		}},
		{"empty bash command", "step 1: bash command is required", func() []protocol.SendStep {
			return []protocol.SendStep{ssBashStep(protocol.StepSiteChild, "")}
		}},
		{"pymodule without script", "step 1: pymodule_run needs repo and script", func() []protocol.SendStep {
			return []protocol.SendStep{{Where: protocol.StepSiteChild,
				PymoduleRun: &protocol.PymoduleRunStep{Repo: "ops_tools"}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.RunSendSteps(context.Background(), "c-1", "t-1", tc.then())
			cc := assert.NewAborting(t)
			cc.Error(err, "RunSendSteps error")
			cc.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
			cc.Eq(tc.want, ssErrMessage(err), "message")
		})
	}
	c.Empty(exec.calls(), "no step ran during validation tests")
}

// TestSendStepsSenderGate pins the CALLER's allowlist gate on sender steps:
// fundi resolves its tri-state tool list, claude only the unrestricted shape,
// every other kind is refused, and a sender step without a calling agent is
// FailedPrecondition. The target's snapshot is never consulted for a sender
// step, so each case's caller is the only party.
func TestSendStepsSenderGate(t *testing.T) {
	type gateCase struct {
		name     string
		caller   childstore.Snapshot
		callerID string
		allow    bool
		wantCode connect.Code
	}
	fundi := func(mutate func(*childstore.Snapshot)) childstore.Snapshot {
		s := ssSnap("/w/caller", protocol.KindFundi, nil, false, false)
		if mutate != nil {
			mutate(&s)
		}
		s.Labels = map[string]string{"rafiki/executor": "e-1"}
		return s
	}
	cases := []gateCase{
		{name: "fundi Tools nil allows",
			caller: fundi(nil), callerID: "c-1", allow: true},
		{name: "fundi Tools bash allows",
			caller: fundi(func(s *childstore.Snapshot) { s.Tools = []string{"bash"} }), callerID: "c-1", allow: true},
		{name: "fundi Tools read denies bash",
			caller: fundi(func(s *childstore.Snapshot) { s.Tools = []string{"read"} }), callerID: "c-1",
			wantCode: connect.CodePermissionDenied},
		{name: "fundi NoTools denies",
			caller: fundi(func(s *childstore.Snapshot) { s.NoTools = true }), callerID: "c-1",
			wantCode: connect.CodePermissionDenied},
		{name: "fundi NoBuiltinTools denies",
			caller: fundi(func(s *childstore.Snapshot) { s.NoBuiltinTools = true }), callerID: "c-1",
			wantCode: connect.CodePermissionDenied},
		{name: "fundi empty Tools denies",
			caller: fundi(func(s *childstore.Snapshot) { s.Tools = []string{} }), callerID: "c-1",
			wantCode: connect.CodePermissionDenied},
		{name: "claude Tools nil allows",
			caller: func() childstore.Snapshot {
				s := ssSnap("/w/caller", protocol.KindClaude, nil, false, false)
				s.Labels = map[string]string{"rafiki/executor": "e-1"}
				return s
			}(), callerID: "c-1", allow: true},
		{name: "claude Tools Bash denies",
			caller: func() childstore.Snapshot {
				s := ssSnap("/w/caller", protocol.KindClaude, []string{"Bash"}, false, false)
				s.Labels = map[string]string{"rafiki/executor": "e-1"}
				return s
			}(), callerID: "c-1", wantCode: connect.CodePermissionDenied},
		{name: "script kind denies",
			caller: func() childstore.Snapshot {
				s := ssSnap("/w/caller", protocol.KindScript, nil, false, false)
				s.Labels = map[string]string{"rafiki/executor": "e-1"}
				return s
			}(), callerID: "c-1", wantCode: connect.CodePermissionDenied},
		{name: "no calling agent is FailedPrecondition",
			caller: fundi(nil), callerID: "", wantCode: connect.CodeFailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec, client := ssScriptedServer(t, ssRun(ssResultText("ran")))
			pool := &ssFakePool{
				live:    ssLive("e-1"),
				clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
			}
			r := ssRunner(pool, map[string]childstore.Snapshot{"c-1": tc.caller})
			_, _, err := r.RunSendSteps(context.Background(), tc.callerID, "t-1",
				[]protocol.SendStep{ssBashStep(protocol.StepSiteSender, "git status")})
			cc := assert.NewAborting(t)
			if tc.allow {
				cc.NoError(err, "RunSendSteps error")
				cc.Len(exec.calls(), 1, "Execute calls")
				return
			}
			cc.Error(err, "RunSendSteps error")
			cc.Eq(tc.wantCode, connect.CodeOf(err), "code")
			if tc.wantCode == connect.CodePermissionDenied {
				cc.Eq("step 1: your tool allowlist does not include bash", ssErrMessage(err), "message")
			} else {
				cc.Eq("step 1: sender steps need a calling agent; this caller has no position in the agent tree", ssErrMessage(err), "message")
			}
			cc.Empty(exec.calls(), "Execute calls recorded despite the gate refusal")
		})
	}
}

// TestSendStepsPartyResolution pins which snapshot a step runs under: child
// steps resolve the target, sender steps the caller, and a party with no
// binding, a not-live executor, or no snapshot at all is refused before
// anything runs.
func TestSendStepsPartyResolution(t *testing.T) {
	c := assert.NewAborting(t)

	target := ssFundi("/w/target", "e-t", "ws-t")
	caller := ssFundi("/w/caller", "e-c", "ws-c")
	snaps := map[string]childstore.Snapshot{"t-1": target, "c-1": caller}

	execT, clientT := ssScriptedServer(t, ssRun(ssResultText("on target")))
	execC, clientC := ssScriptedServer(t, ssRun(ssResultText("on caller")))
	pool := &ssFakePool{
		live: ssLive("e-t", "e-c"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{
			"e-t": clientT, "e-c": clientC,
		},
	}
	r := ssRunner(pool, snaps)

	_, _, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
	c.NoError(err, "child step error")
	c.EqDeep([]string{"e-t"}, pool.dialed, "child step dialed executor")
	c.Len(execT.calls(), 1, "target executor Execute calls")
	c.Empty(execC.calls(), "caller executor Execute calls for a child step")
	c.Eq("ws-t", execT.calls()[0].GetWorkspaceId(), "child step workspace")

	_, _, err = r.RunSendSteps(context.Background(), "c-1", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteSender, "git status")})
	c.NoError(err, "sender step error")
	c.EqDeep([]string{"e-t", "e-c"}, pool.dialed, "sender step dialed executor")
	c.Len(execC.calls(), 1, "caller executor Execute calls")
	c.Eq("ws-c", execC.calls()[0].GetWorkspaceId(), "sender step workspace")

	// Refusals, each before anything runs.
	r = ssRunner(pool, map[string]childstore.Snapshot{"c-1": caller}) // no target snapshot
	_, _, err = r.RunSendSteps(context.Background(), "c-1", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
	c.Error(err, "missing snapshot error")
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "missing snapshot code")
	c.Eq("step 1: unknown agent t-1", ssErrMessage(err), "missing snapshot message")

	noLabel := ssSnap("/w/target", protocol.KindFundi, nil, false, false)
	noLabel.Labels = map[string]string{"rafiki/workspace": "ws-t"}
	r = ssRunner(pool, map[string]childstore.Snapshot{"t-1": noLabel, "c-1": caller})
	_, _, err = r.RunSendSteps(context.Background(), "c-1", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
	c.Error(err, "missing binding error")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "missing binding code")
	c.Eq("step 1: t-1 has no executor binding", ssErrMessage(err), "missing binding message")

	notLive := ssFundi("/w/target", "e-gone", "")
	r = ssRunner(pool, map[string]childstore.Snapshot{"t-1": notLive, "c-1": caller})
	_, _, err = r.RunSendSteps(context.Background(), "c-1", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
	c.Error(err, "not-live error")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "not-live code")
	c.Eq("step 1: executor e-gone is not live", ssErrMessage(err), "not-live message")

	r = &sendStepRunner{snap: ssSnapshots(snaps), nonce: func() string { return ssTestNonce }}
	_, _, err = r.RunSendSteps(context.Background(), "c-1", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
	c.Error(err, "no-pool error")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "no-pool code")
	c.Eq("send steps need an executor pool; this daemon has none", ssErrMessage(err), "no-pool message")
}

// TestSendStepsCwdBinding pins the cwd semantics of a workspace-less party:
// a relative read path joins onto Cwd, the bash command gets a single-quoted
// cd prefix, and pymodule_run's cwd binds through bindCwdTo. With a
// workspace label everything passes through unchanged.
func TestSendStepsCwdBinding(t *testing.T) {
	c := assert.NewAborting(t)
	exec, client := ssScriptedServer(t,
		ssRun(ssResultText("1")),
		ssRun(ssResultText("2")),
		ssRun(ssResultText("3")),
		ssRun(ssResultText("4")),
		ssRun(ssResultText("5")),
		ssRun(ssResultText("6")),
	)
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}

	steps := []protocol.SendStep{
		ssReadStep(protocol.StepSiteChild, "rel.txt", 0, 0),
		ssBashStep(protocol.StepSiteChild, "git status"),
		{Where: protocol.StepSiteChild, PymoduleRun: &protocol.PymoduleRunStep{Repo: "ops_tools", Script: "rotate", Cwd: "sub"}},
	}
	r := ssRunner(pool, map[string]childstore.Snapshot{"t-1": ssFundi("/w/it's", "e-1", "")})
	_, _, err := r.RunSendSteps(context.Background(), "c-1", "t-1", steps)
	c.Require().NoError(err, "workspace-less run error")
	calls := exec.calls()
	c.Len(calls, 3, "Execute calls")

	var read struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
	}
	c.Require().NoError(json.Unmarshal(calls[0].GetInputJson(), &read), "read input")
	c.Eq("/w/it's/rel.txt", read.Path, "read path joined onto Cwd")
	c.Eq(1, read.Offset, "read offset")

	var bash struct {
		Command   string `json:"command"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	c.Require().NoError(json.Unmarshal(calls[1].GetInputJson(), &bash), "bash input")
	c.Eq("cd '/w/it'\"'\"'s' || exit 1\ngit status", bash.Command, "bash cd prefix")
	c.Eq(30000, bash.TimeoutMs, "bash default timeout_ms")

	var pm struct {
		Repo   string   `json:"repo"`
		Script string   `json:"script"`
		Args   []string `json:"args"`
		Cwd    string   `json:"cwd"`
	}
	c.Require().NoError(json.Unmarshal(calls[2].GetInputJson(), &pm), "pymodule input")
	c.Eq("/w/it's/sub", pm.Cwd, "pymodule cwd bound through bindCwdTo")
	c.Empty(pm.Args, "pymodule args")

	// With a workspace label, everything passes through unchanged.
	steps = []protocol.SendStep{
		ssReadStep(protocol.StepSiteChild, "rel.txt", 0, 0),
		ssBashStep(protocol.StepSiteChild, "git status"),
	}
	r = ssRunner(pool, map[string]childstore.Snapshot{
		"t-1": ssFundi("/w/it's", "e-1", "ws-9"),
	})
	_, _, err = r.RunSendSteps(context.Background(), "c-1", "t-1", steps)
	c.Require().NoError(err, "workspace run error")
	calls = exec.calls()
	c.Len(calls, 5, "Execute calls")
	c.Require().NoError(json.Unmarshal(calls[3].GetInputJson(), &read), "read input")
	c.Eq("rel.txt", read.Path, "read path passes through with a workspace")
	c.Require().NoError(json.Unmarshal(calls[4].GetInputJson(), &bash), "bash input")
	c.Eq("git status", bash.Command, "bash command passes through with a workspace")
	c.Eq("ws-9", calls[3].GetWorkspaceId(), "workspace id rides the request")
}

// TestSendStepsOutcomes pins the outcome classification, keyed on
// executorpb.Failure.Code — never on message text.
func TestSendStepsOutcomes(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t, ssRun(ssResultText("hello"))), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
		c.Require().NoError(err, "RunSendSteps error")
		c.StrContains(rendered, "hello", "rendered body")
		want := protocol.StepSummary{Index: 1, Tool: "bash", Where: protocol.StepSiteChild,
			Outcome: "ok", Bytes: 5}
		c.Eq(want, summaries[0], "summary")
	})

	t.Run("tool failed is inline and the next step still runs", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t,
			ssRun(ssFailureEvent(executorpb.Failure_CODE_TOOL_FAILED, "boom")),
			ssRun(ssResultText("next")),
		), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1", []protocol.SendStep{
			ssBashStep(protocol.StepSiteChild, "failing"),
			ssBashStep(protocol.StepSiteChild, "after"),
		})
		c.Require().NoError(err, "RunSendSteps error")
		c.StrContains(rendered, "(error: boom)", "first step's body")
		c.StrContains(rendered, "next", "second step's body")
		c.Eq("error", summaries[0].Outcome, "first outcome")
		c.Eq("ok", summaries[1].Outcome, "second outcome")
		c.Eq(len("(error: boom)"), summaries[0].Bytes, "first Bytes")
	})

	t.Run("timeout is inline", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t,
			ssRun(ssFailureEvent(executorpb.Failure_CODE_TIMEOUT, "too slow")),
		), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "sleep 90")})
		c.Require().NoError(err, "RunSendSteps error")
		c.StrContains(rendered, "(timed out: too slow)", "rendered body")
		c.Eq("timeout", summaries[0].Outcome, "outcome")
	})

	t.Run("denied refuses the send", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t,
			ssRun(ssFailureEvent(executorpb.Failure_CODE_DENIED, "not allowed")),
		), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "rm -rf /")})
		c.Error(err, "RunSendSteps error")
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		c.Eq("step 1: executor denied bash: not allowed", ssErrMessage(err), "message")
		c.Empty(rendered, "rendered despite refusal")
		c.Nil(summaries, "summaries despite refusal")
	})

	t.Run("executor lost refuses the send", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t,
			ssRun(ssFailureEvent(executorpb.Failure_CODE_EXECUTOR_LOST, "gone")),
		), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
		c.Error(err, "RunSendSteps error")
		c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
		c.ErrorContains(err, "step 1: bash did not run:", "message")
		c.Empty(rendered, "rendered despite refusal")
		c.Nil(summaries, "summaries despite refusal")
	})

	t.Run("unspecified code refuses the send", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t,
			ssRun(ssFailureEvent(executorpb.Failure_CODE_UNSPECIFIED, "mystery")),
		), ssTargetSnaps())
		rendered, _, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
		c.Error(err, "RunSendSteps error")
		c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
		c.Empty(rendered, "rendered despite refusal")
	})

	t.Run("stream error refuses the send", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t,
			ssRunThenFail(errors.New("kaboom"), ssResultText("partial")),
		), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
		c.Error(err, "RunSendSteps error")
		c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
		c.ErrorContains(err, "step 1: bash did not run:", "message")
		c.Empty(rendered, "rendered despite refusal")
		c.Nil(summaries, "summaries despite refusal")
	})

	t.Run("execute error refuses the send", func(t *testing.T) {
		c := assert.NewAborting(t)
		r := ssRunner(ssOutcomesPool(t, ssRunThenFail(errors.New("nope"))), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
		c.Error(err, "RunSendSteps error")
		c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
		c.ErrorContains(err, "step 1: bash did not run:", "message")
		c.Empty(rendered, "rendered despite refusal")
		c.Nil(summaries, "summaries despite refusal")
	})
}

// ssOutcomesPool builds a pool whose "e-1" executor answers the given scripts.
func ssOutcomesPool(t *testing.T, scripts ...ssScript) *ssFakePool {
	t.Helper()
	_, client := ssScriptedServer(t, scripts...)
	return &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
}

// ssTargetSnaps is the snapshot map every outcomes subtest needs: a fundi
// target with an executor binding, and a fundi caller (never consulted for a
// child step, but present so a mistake reads as a gate failure, not a
// NotFound).
func ssTargetSnaps() map[string]childstore.Snapshot {
	return map[string]childstore.Snapshot{
		"t-1": ssFundi("/w/target", "e-1", ""),
		"c-1": ssFundi("/w/caller", "e-1", ""),
	}
}

// TestSendStepsTruncationAndRenderedCap pins the two output caps: a step's
// body over 32 KiB is cut (with the marker and honest counts, and Bytes keeps
// the pre-truncation length), and a rendered block over 128 KiB refuses the
// send naming the five largest steps.
func TestSendStepsTruncationAndRenderedCap(t *testing.T) {
	t.Run("per-step truncation", func(t *testing.T) {
		c := assert.NewAborting(t)
		big := strings.Repeat("a", 40960)
		r := ssRunner(ssOutcomesPool(t, ssRun(ssResultText(big))), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1",
			[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "cat big")})
		c.Require().NoError(err, "RunSendSteps error")
		c.StrContains(rendered, "\n[truncated: showing 32768 of 40960 bytes]", "marker")
		c.StrContains(rendered, strings.Repeat("a", 32768), "kept bytes present")
		c.NotStrContains(rendered, strings.Repeat("a", 32769),
			"more than the kept bytes leaked into the render")
		c.Eq(40960, summaries[0].Bytes, "Bytes is the pre-truncation length")
		c.True(summaries[0].Truncated, "Truncated")
	})

	t.Run("rendered cap refuses the send", func(t *testing.T) {
		c := assert.NewAborting(t)
		scripts := make([]ssScript, 5)
		steps := make([]protocol.SendStep, 5)
		for i := range scripts {
			scripts[i] = ssRun(ssResultText(strings.Repeat("a", 32768)))
			steps[i] = ssBashStep(protocol.StepSiteChild, fmt.Sprintf("cat big%d", i))
		}
		// What the block would have rendered: the golden-pinned preamble plus
		// each untruncated header+body. The cap error must report exactly that
		// length, and name the five largest steps — here, all five, equal bytes
		// so index ascending.
		headers := make([]string, len(steps))
		for i := range steps {
			headers[i] = fmt.Sprintf("\n\n=== %s %d bash (child): %s ===\n%s",
				ssTestNonce, i+1, fmt.Sprintf("cat big%d", i), strings.Repeat("a", 32768))
		}
		wouldRender := fmt.Sprintf(ssStepsPreamble, ssTestNonce, ssTestNonce) + strings.Join(headers, "")
		r := ssRunner(ssOutcomesPool(t, scripts...), ssTargetSnaps())
		rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1", steps)
		c.Error(err, "RunSendSteps error")
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
		c.Eq(fmt.Sprintf("steps output %d bytes exceeds the 131072-byte cap; largest: "+
			"step 1 bash 32768B, step 2 bash 32768B, step 3 bash 32768B, step 4 bash 32768B, step 5 bash 32768B",
			len(wouldRender)), ssErrMessage(err), "message")
		c.Empty(rendered, "rendered despite refusal")
		c.Nil(summaries, "summaries despite refusal")
	})
}

// TestSendStepsRenderFormat is the golden: the preamble and each header's
// exact bytes, with the fixed nonce.
func TestSendStepsRenderFormat(t *testing.T) {
	c := assert.NewAborting(t)
	exec, client := ssScriptedServer(t,
		ssRun(ssResultText("line3\nline4\nline5\nline6\nline7")),
		ssRun(ssResultText("M file.txt")),
		ssRun(ssResultText("rotated")),
	)
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
	r := ssRunner(pool, map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-1", "")})
	steps := []protocol.SendStep{
		ssReadStep(protocol.StepSiteChild, "/abs/reports/task-3-review.md", 3, 7),
		ssBashStep(protocol.StepSiteChild, "git status\ngit log -1 --oneline"),
		{Where: protocol.StepSiteChild, PymoduleRun: &protocol.PymoduleRunStep{
			Repo: "ops_tools", Script: "rotate", Args: []string{"--dry", "run"},
		}},
	}
	rendered, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1", steps)
	c.Require().NoError(err, "RunSendSteps error")

	want := `[rafiki steps 0011223344556677] These were run for you when this message was sent — a snapshot, not live state. Each result follows a header line beginning "=== 0011223344556677 ". A (sender) step ran in the sender's environment; you may not be able to reproduce it.

=== 0011223344556677 1 read (child): /abs/reports/task-3-review.md:3-7 ===
line3
line4
line5
line6
line7

=== 0011223344556677 2 bash (child): git status git log -1 --oneline ===
M file.txt

=== 0011223344556677 3 pymodule_run (child): ops_tools/rotate --dry run ===
rotated`
	c.Eq(want, rendered, "rendered block")

	// The subjects are what the sender asked for, not what went on the wire:
	// pin the two that differ. The bash subject is the ORIGINAL command,
	// flattened, while the wire command carries the cd prefix; the read
	// subject is the path as given, ranged.
	calls := exec.calls()
	var bash struct {
		Command string `json:"command"`
	}
	c.Require().NoError(json.Unmarshal(calls[1].GetInputJson(), &bash), "bash input")
	c.Eq("cd '/w' || exit 1\ngit status\ngit log -1 --oneline", bash.Command, "bash wire command")
	var read struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	c.Require().NoError(json.Unmarshal(calls[0].GetInputJson(), &read), "read input")
	c.Eq("/abs/reports/task-3-review.md", read.Path, "read wire path")
	c.Eq(3, read.Offset, "read wire offset")
	c.Eq(5, read.Limit, "read wire limit (7-3+1)")

	c.Len(summaries, 3, "summaries")
	c.Eq(3, summaries[2].Index, "third summary index")
	c.Eq("pymodule_run", summaries[2].Tool, "third summary tool")

	t.Run("subjects are what the sender asked for, cut to 200 bytes", func(t *testing.T) {
		cc := assert.NewAborting(t)
		exec2, client2 := ssScriptedServer(t, ssRun(ssResultText("ok")), ssRun(ssResultText("ok")))
		pool2 := &ssFakePool{
			live:    ssLive("e-1"),
			clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client2},
		}
		r2 := ssRunner(pool2, map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-1", "")})
		// 70 three-byte runes = 210 bytes: the subject cut backs off to the
		// rune boundary at 198 bytes, never splitting one.
		long := strings.Repeat("\u4e2d", 70)
		rendered2, _, err := r2.RunSendSteps(context.Background(), "c-1", "t-1", []protocol.SendStep{
			ssReadStep(protocol.StepSiteChild, "rel.txt", 0, 0),
			ssBashStep(protocol.StepSiteChild, long),
		})
		cc.Require().NoError(err, "RunSendSteps error")

		// The read subject is the path AS GIVEN, though the wire input carries
		// the cwd-joined one.
		cc.StrContains(rendered2,
			"=== 0011223344556677 1 read (child): rel.txt ===", "read subject as given")
		calls2 := exec2.calls()
		var read2 struct {
			Path string `json:"path"`
		}
		cc.Require().Len(calls2, 2, "Execute calls")
		cc.Require().NoError(json.Unmarshal(calls2[0].GetInputJson(), &read2), "read input")
		cc.Eq("/w/rel.txt", read2.Path, "wire path joined")

		// The bash subject is the 198-byte rune-boundary cut of the original.
		cc.StrContains(rendered2,
			"=== 0011223344556677 2 bash (child): "+strings.Repeat("\u4e2d", 66)+" ===",
			"bash subject cut on a rune boundary")
	})
}

// TestSendStepsEcho pins echo: true returns the first 2048 bytes of the
// post-truncation body; false returns nothing.
func TestSendStepsEcho(t *testing.T) {
	t.Run("echo true returns the first 2048 bytes", func(t *testing.T) {
		c := assert.NewAborting(t)
		step := ssBashStep(protocol.StepSiteChild, "cat big")
		step.Echo = true
		r := ssRunner(ssOutcomesPool(t, ssRun(ssResultText(strings.Repeat("x", 4096)))), ssTargetSnaps())
		_, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1", []protocol.SendStep{step})
		c.Require().NoError(err, "RunSendSteps error")
		c.Eq(strings.Repeat("x", 2048), summaries[0].Echo, "echo")
		c.Eq(4096, summaries[0].Bytes, "Bytes")
	})

	t.Run("echo false returns nothing", func(t *testing.T) {
		c := assert.NewAborting(t)
		step := ssBashStep(protocol.StepSiteChild, "cat big")
		r := ssRunner(ssOutcomesPool(t, ssRun(ssResultText(strings.Repeat("x", 4096)))), ssTargetSnaps())
		_, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1", []protocol.SendStep{step})
		c.Require().NoError(err, "RunSendSteps error")
		c.Empty(summaries[0].Echo, "echo")
	})

	t.Run("echo is of the post-truncation body", func(t *testing.T) {
		c := assert.NewAborting(t)
		step := ssBashStep(protocol.StepSiteChild, "cat big")
		step.Echo = true
		r := ssRunner(ssOutcomesPool(t, ssRun(ssResultText(strings.Repeat("y", 40960)))), ssTargetSnaps())
		_, summaries, err := r.RunSendSteps(context.Background(), "c-1", "t-1", []protocol.SendStep{step})
		c.Require().NoError(err, "RunSendSteps error")
		c.True(summaries[0].Truncated, "Truncated")
		c.Eq(strings.Repeat("y", 2048), summaries[0].Echo, "echo of the kept bytes")
		c.False(strings.Contains(summaries[0].Echo, "[truncated"), "echo carries no truncation marker")
	})
}

// TestSendStepsNilExecPoolIsNotANonNilInterface pins the constructor against
// the nil-into-interface trap: a Controller with a nil execPoolConn must
// yield a runner whose pool is genuinely nil — a nil *execpool.Pool stored in
// the interface would compare non-nil and defeat the no-pool refusal.
func TestSendStepsNilExecPoolIsNotANonNilInterface(t *testing.T) {
	c := assert.NewAborting(t)
	r := newSendStepRunner(&Controller{})
	c.False(r.pool != nil, "pool is non-nil despite a nil execPoolConn")

	_, _, err := r.RunSendSteps(context.Background(), "", "t-1",
		[]protocol.SendStep{ssBashStep(protocol.StepSiteChild, "git status")})
	c.Error(err, "RunSendSteps error")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Eq("send steps need an executor pool; this daemon has none", ssErrMessage(err), "message")
}

// A caller whose own allowlist excludes bash cannot reach it through a
// descendant; the operator (no caller) is ungated.
func TestSendStepsChildStepsHonourCallerAllowlist(t *testing.T) {
	c := assert.NewCollecting(t)
	exec, client := ssScriptedServer(t, ssRun(ssResultText("ok")))
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
	caller := ssFundi("/w", "e-1", "")
	caller.Tools = []string{"read"}
	snaps := map[string]childstore.Snapshot{"c-2": caller, "t-1": ssFundi("/w", "e-1", "")}
	r := ssRunner(pool, snaps)
	steps := []protocol.SendStep{ssBashStep(protocol.StepSiteChild, "id")}

	_, _, err := r.RunSendSteps(context.Background(), "c-2", "t-1", steps)
	c.Error(err, "restricted caller")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
	c.Empty(exec.calls(), "nothing may run")

	_, _, err = r.RunSendSteps(context.Background(), "", "t-1", steps)
	c.NoError(err, "operator is ungated")
}

func TestSendStepsHeaderSubjectStaysOnOneLine(t *testing.T) {
	c := assert.NewCollecting(t)
	p := ssStepPlan{step: protocol.SendStep{Read: &protocol.PrefillRead{Path: "a\nb\rc"}}}
	c.Eq("a b c", ssStepSubject(p), "read subject")
}

func TestSendStepsTruncateUTF8BackoffIsBounded(t *testing.T) {
	c := assert.NewCollecting(t)
	got := ssTruncateUTF8(strings.Repeat("\x80", 100), 50)
	c.Eq(50, len(got), "binary run is cut at the limit, not walked back")
}

func TestSendStepsEmptyStreamIsUnavailable(t *testing.T) {
	c := assert.NewCollecting(t)
	_, client := ssScriptedServer(t, ssRun())
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
	r := ssRunner(pool, map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-1", "")})
	_, _, err := r.RunSendSteps(context.Background(), "", "t-1", []protocol.SendStep{ssBashStep(protocol.StepSiteChild, "id")})
	c.Error(err, "a stream with no result")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

// A target the send could not reach is refused before any step runs.
func TestSendStepsUndeliverableTargetRunsNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	exec, client := ssScriptedServer(t, ssRun(ssResultText("must not run")))
	pool := &ssFakePool{
		live:    ssLive("e-1"),
		clients: map[string]executorpbconnect.ExecutorServiceClient{"e-1": client},
	}
	r := ssRunner(pool, map[string]childstore.Snapshot{"t-1": ssFundi("/w", "e-1", "")})
	r.deliverable = func(string) error {
		return &connectapi.ControllerError{Code: protocol.ErrChildExited, Message: "child has exited"}
	}
	_, _, err := r.RunSendSteps(context.Background(), "", "t-1", []protocol.SendStep{ssBashStep(protocol.StepSiteChild, "id")})
	c.Error(err, "exited target")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Empty(exec.calls(), "no step may run")
}
