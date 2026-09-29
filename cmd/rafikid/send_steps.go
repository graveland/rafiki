// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/prefill"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Budgets for send steps. Every axis is bounded: step count, one step's
// output, the whole rendered block, the bash timeout, and one deadline over
// all steps, so a send with steps can never wedge the daemon's send path or
// flood a child's context.
const (
	maxSendSteps               = 16
	sendStepDefaultBashTimeout = 30 * time.Second
	sendStepMaxBashTimeout     = 60 * time.Second
	sendStepExecTimeout        = 60 * time.Second
	sendStepsDeadline          = 120 * time.Second
	sendStepOutputCap          = 32 * 1024
	sendStepsRenderedCap       = 128 * 1024
	sendStepEchoCap            = 2 * 1024
	sendStepHeaderSubjectMax   = 200
)

// ssStepsPreamble is the exact first line of a rendered steps block. The
// nonce appears twice — in the announcement and in the delimiter rule — so a
// reader can split the block without guessing where a result ends.
const ssStepsPreamble = `[rafiki steps %s] These were run for you when this message was sent — a snapshot, not live state. Each result follows a header line beginning "=== %s ". A (sender) step ran in the sender's environment; you may not be able to reproduce it.`

// sendStepRunner implements connectapi.SendStepRunner: it validates a send's
// steps, runs them in order on the resolved party's executor, and renders
// their output into the message text. The connectapi interface it satisfies
// is declared in pkg/connectapi (Send-wiring task); this type matches it
// structurally, and the wiring compiles the satisfaction check.
//
// pool is the pymodulePool interface already defined in pymodulesync.go,
// satisfied directly by *execpool.Pool (Controller.execPoolConn). snap is
// Controller.st.Get; nonce is randomStepNonce in production, a fixed value in
// tests.
type sendStepRunner struct {
	pool  pymodulePool // nil: daemon has no executor pool
	snap  func(childID string) (childstore.Snapshot, bool)
	nonce func() string // 16 hex chars; crypto/rand in production
}

// newSendStepRunner builds the daemon's SendStepRunner from a Controller.
//
// pool is set ONLY when c.execPoolConn is non-nil: storing a nil
// *execpool.Pool in the pymodulePool interface would compare non-nil and
// silently defeat the no-pool refusal below — the same nil-into-interface
// trap the connectapi Set* setters refuse.
func newSendStepRunner(c *Controller) *sendStepRunner {
	r := &sendStepRunner{snap: c.st.Get, nonce: randomStepNonce}
	if c.execPoolConn != nil {
		r.pool = c.execPoolConn
	}
	return r
}

// ssStepPlan is one validated step: phase 1 resolves everything its executor
// call needs, so phase 2 never re-validates and can never execute a step
// phase 1 refused.
type ssStepPlan struct {
	step        protocol.SendStep
	tool        string
	index       int // 1-based: messages, headers, summaries
	executorID  string
	workspaceID string
	cwd         string
}

// ssRunOutcome is one executed step's classified result.
type ssRunOutcome struct {
	outcome   string // ok | error | timeout
	body      string // the (possibly truncated) rendered body
	bytes     int    // the body's length BEFORE truncation
	truncated bool
	echo      string // first ssStepEchoCap bytes, only when the step asked
}

func (r *sendStepRunner) RunSendSteps(ctx context.Context, callerID, targetID string, steps []protocol.SendStep) (string, []protocol.StepSummary, error) {
	plans, err := r.validateSteps(callerID, targetID, steps)
	if err != nil {
		return "", nil, err
	}
	return r.executeSteps(ctx, plans)
}

// validateSteps is phase 1: validate ALL steps, execute NONE, so a refusal
// never leaves step side effects behind. Shape checks run for every step
// first; only then are parties resolved and sender steps gated.
func (r *sendStepRunner) validateSteps(callerID, targetID string, steps []protocol.SendStep) ([]ssStepPlan, error) {
	if len(steps) > maxSendSteps {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("too many steps: %d (max %d)", len(steps), maxSendSteps))
	}
	for i := range steps {
		if err := ssValidateStepShape(i+1, steps[i]); err != nil {
			return nil, err
		}
	}
	if r.pool == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("send steps need an executor pool; this daemon has none"))
	}
	live := r.pool.Live()
	plans := make([]ssStepPlan, len(steps))
	for i, step := range steps {
		index := i + 1
		tool := ssStepTool(step)
		// A sender step runs on the caller's own executor under the caller's
		// own tool allowlist, so it needs a caller with a position in the
		// agent tree. A child step does not: it is the sender's action, but
		// it runs with the target's authority, and the Send scope check has
		// already admitted the caller to that target.
		if step.Where == protocol.StepSiteSender && callerID == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("step %d: sender steps need a calling agent; this caller has no position in the agent tree", index))
		}
		partyID := targetID
		if step.Where == protocol.StepSiteSender {
			partyID = callerID
		}
		snap, ok := r.snap(partyID)
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("step %d: unknown agent %s", index, partyID))
		}
		executorID := snap.Labels["rafiki/executor"]
		if executorID == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("step %d: %s has no executor binding", index, partyID))
		}
		if !slices.ContainsFunc(live, func(le execpool.LiveExecutor) bool { return le.Executor.ID == executorID }) {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("step %d: executor %s is not live", index, executorID))
		}
		// The sender gate is on the CALLER's snapshot, which for a sender
		// step is the party: the step runs under the caller's own tool
		// allowlist, exactly as if the caller had run the tool itself.
		if step.Where == protocol.StepSiteSender && !ssSenderAllows(snap, tool) {
			return nil, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("step %d: your tool allowlist does not include %s", index, tool))
		}
		plans[i] = ssStepPlan{
			step:        step,
			tool:        tool,
			index:       index,
			executorID:  executorID,
			workspaceID: snap.Labels["rafiki/workspace"],
			cwd:         snap.Cwd,
		}
	}
	return plans, nil
}

// ssValidateStepShape checks one step's own fields, before anything external
// is consulted. The empty Where is refused here — never defaulted — so a
// zero-valued SendStep cannot silently run anywhere.
func ssValidateStepShape(index int, step protocol.SendStep) error {
	set := 0
	if step.Read != nil {
		set++
	}
	if step.Bash != nil {
		set++
	}
	if step.PymoduleRun != nil {
		set++
	}
	if set != 1 {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("step %d: exactly one of read, bash, pymodule_run is required", index))
	}
	if step.Where != protocol.StepSiteChild && step.Where != protocol.StepSiteSender {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("step %d: where must be child or sender", index))
	}
	switch {
	case step.Read != nil:
		if prefill.IsGlob(step.Read.Path) {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: read takes one file, not a glob", index))
		}
		if err := prefill.Validate([]protocol.PrefillRead{*step.Read}); err != nil {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("step %d: %v", index, err))
		}
	case step.Bash != nil:
		if step.Bash.Command == "" {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: bash command is required", index))
		}
		if step.Bash.TimeoutMs < 0 || step.Bash.TimeoutMs > int(sendStepMaxBashTimeout/time.Millisecond) {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: bash timeout_ms must be between 0 and %d",
					index, sendStepMaxBashTimeout/time.Millisecond))
		}
	case step.PymoduleRun != nil:
		if step.PymoduleRun.Repo == "" || step.PymoduleRun.Script == "" {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: pymodule_run needs repo and script", index))
		}
	}
	return nil
}

// ssStepTool names the tool a step runs. Only valid for a step that passed
// ssValidateStepShape.
func ssStepTool(step protocol.SendStep) string {
	switch {
	case step.Read != nil:
		return "read"
	case step.Bash != nil:
		return "bash"
	case step.PymoduleRun != nil:
		return "pymodule_run"
	}
	return ""
}

// ssSenderAllows is the sender gate: may the CALLER run this tool itself?
// fundi resolves its allowlist like the preset tri-state (nil = all, empty =
// none, NoTools/NoBuiltinTools = none); claude allowlists use Claude Code
// names that v1 does not map, so only the unrestricted shape is allowed;
// every other kind has no resolvable allowlist and is refused.
func ssSenderAllows(snap childstore.Snapshot, tool string) bool {
	switch snap.Kind {
	case protocol.KindFundi:
		if snap.NoTools || snap.NoBuiltinTools {
			return false
		}
		if snap.Tools == nil {
			return true
		}
		return slices.Contains(snap.Tools, tool)
	case protocol.KindClaude:
		return snap.Tools == nil && !snap.NoTools
	default:
		return false
	}
}

// executeSteps is phase 2: run the validated plans in order under one
// deadline. An inline outcome (ok/error/timeout) is collected and the send
// continues; a refusal (denied/lost/transport/deadline) returns an error and
// nothing is rendered or enqueued — earlier steps' side effects stand.
func (r *sendStepRunner) executeSteps(ctx context.Context, plans []ssStepPlan) (string, []protocol.StepSummary, error) {
	execCtx, cancel := context.WithTimeout(ctx, sendStepsDeadline)
	defer cancel()

	outcomes := make([]ssRunOutcome, len(plans))
	for i, p := range plans {
		if err := execCtx.Err(); err != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("step %d: %s did not run: %v", p.index, p.tool, err))
		}
		client, err := r.pool.ConnectClientFor(p.executorID)
		if err != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("step %d: executor %s unavailable: %v", p.index, p.executorID, err))
		}
		req, err := ssExecuteRequest(p)
		if err != nil {
			return "", nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("step %d: %s input: %v", p.index, p.tool, err))
		}
		stream, err := client.Execute(execCtx, connect.NewRequest(req))
		if err != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("step %d: %s did not run: %v", p.index, p.tool, err))
		}
		text, failure, streamErr := ssDrainExecute(stream)
		if streamErr != nil {
			return "", nil, connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("step %d: %s did not run: %v", p.index, p.tool, streamErr))
		}
		switch {
		case failure == nil:
			outcomes[i] = ssRunOutcome{outcome: "ok", body: text, bytes: len(text)}
		case failure.GetCode() == executorpb.Failure_CODE_TOOL_FAILED:
			body := "(error: " + failure.GetMessage() + ")"
			outcomes[i] = ssRunOutcome{outcome: "error", body: body, bytes: len(body)}
		case failure.GetCode() == executorpb.Failure_CODE_TIMEOUT:
			body := "(timed out: " + failure.GetMessage() + ")"
			outcomes[i] = ssRunOutcome{outcome: "timeout", body: body, bytes: len(body)}
		case failure.GetCode() == executorpb.Failure_CODE_DENIED:
			return "", nil, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("step %d: executor denied %s: %s", p.index, p.tool, failure.GetMessage()))
		default: // CODE_EXECUTOR_LOST, CODE_UNSPECIFIED
			return "", nil, connect.NewError(connect.CodeUnavailable,
				fmt.Errorf("step %d: %s did not run: executor failure %v: %s",
					p.index, p.tool, failure.GetCode(), failure.GetMessage()))
		}
		body := outcomes[i].body
		if len(body) > sendStepOutputCap {
			kept := ssTruncateUTF8(body, sendStepOutputCap)
			body = kept + fmt.Sprintf("\n[truncated: showing %d of %d bytes]", len(kept), len(outcomes[i].body))
			outcomes[i].truncated = true
		}
		outcomes[i].body = body
		if p.step.Echo {
			outcomes[i].echo = ssTruncateUTF8(body, sendStepEchoCap)
		}
	}

	nonce := r.nonce()
	var b strings.Builder
	fmt.Fprintf(&b, ssStepsPreamble, nonce, nonce)
	summaries := make([]protocol.StepSummary, 0, len(plans))
	for i, p := range plans {
		fmt.Fprintf(&b, "\n\n=== %s %d %s (%s): %s ===\n%s",
			nonce, p.index, p.tool, p.step.Where, ssStepSubject(p), outcomes[i].body)
		summaries = append(summaries, protocol.StepSummary{
			Index:     p.index,
			Tool:      p.tool,
			Where:     p.step.Where,
			Outcome:   outcomes[i].outcome,
			Bytes:     outcomes[i].bytes,
			Truncated: outcomes[i].truncated,
			Echo:      outcomes[i].echo,
		})
	}
	rendered := b.String()
	if len(rendered) > sendStepsRenderedCap {
		return "", nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("steps output %d bytes exceeds the %d-byte cap; largest: %s",
				len(rendered), sendStepsRenderedCap, ssLargestSteps(plans, outcomes)))
	}
	return rendered, summaries, nil
}

// ssReadInput carries the read step's fields onto the wire. The executor's
// read resolves offset/limit like the read tool: 1-based inclusive bounds.
type ssReadInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit,omitempty"`
}

type ssBashInput struct {
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeout_ms"`
}

type ssPymoduleRunInput struct {
	Repo    string   `json:"repo"`
	Script  string   `json:"script"`
	Modules []string `json:"modules"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
}

// ssExecuteRequest builds one step's executor call. The party's workspace
// label decides cwd semantics: with a workspace the registry resolves
// relative paths and the command runs as given; without one, the party's Cwd
// is bound into the call exactly as the MCP pymodule_run proxy does.
func ssExecuteRequest(p ssStepPlan) (*executorpb.ExecuteRequest, error) {
	var input json.RawMessage
	var err error
	timeoutMs := int(sendStepExecTimeout / time.Millisecond)
	switch {
	case p.step.Read != nil:
		input, err = json.Marshal(ssReadStepInput(p))
	case p.step.Bash != nil:
		command := p.step.Bash.Command
		if p.workspaceID == "" && p.cwd != "" {
			command = "cd " + shellSingleQuote(p.cwd) + " && " + command
		}
		timeout := p.step.Bash.TimeoutMs
		if timeout == 0 {
			timeout = int(sendStepDefaultBashTimeout / time.Millisecond)
		}
		input, err = json.Marshal(ssBashInput{Command: command, TimeoutMs: timeout})
		timeoutMs = timeout + 5_000 // the step's own bound, plus drain slack
	case p.step.PymoduleRun != nil:
		input, err = json.Marshal(ssPymoduleRunInput{
			Repo:    p.step.PymoduleRun.Repo,
			Script:  p.step.PymoduleRun.Script,
			Modules: p.step.PymoduleRun.Modules,
			Args:    p.step.PymoduleRun.Args,
			Cwd:     p.step.PymoduleRun.Cwd,
		})
		if err == nil {
			input = bindCwdTo(p.cwd, input)
		}
	}
	if err != nil {
		return nil, err
	}
	return &executorpb.ExecuteRequest{
		Tool:        p.tool,
		InputJson:   input,
		TimeoutMs:   int64(timeoutMs),
		WorkspaceId: p.workspaceID,
	}, nil
}

// ssReadStepInput resolves a read step's wire input. With a workspace the
// path passes through (the workspace registry resolves it); without one, a
// relative path is joined onto the party's Cwd daemon-side.
func ssReadStepInput(p ssStepPlan) ssReadInput {
	path := p.step.Read.Path
	if p.workspaceID == "" && !filepath.IsAbs(path) {
		path = filepath.Join(p.cwd, path)
	}
	offset := p.step.Read.Start
	if offset == 0 {
		offset = 1
	}
	in := ssReadInput{Path: path, Offset: offset}
	if p.step.Read.End > 0 {
		in.Limit = p.step.Read.End - offset + 1
	}
	return in
}

// ssDrainExecute drains an Execute stream exactly as mcpPyModuleRunExecutor.Run
// does: concatenate Result text content, remember the last Failed event. The
// stream is always closed; err is stream.Err() after the loop.
func ssDrainExecute(stream *connect.ServerStreamForClient[executorpb.ExecuteResponse]) (text string, failure *executorpb.Failure, err error) {
	defer stream.Close()
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					text += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			failure = ev.Failed
		}
	}
	return text, failure, stream.Err()
}

// ssStepSubject renders a header's subject. Read shows the path AS GIVEN
// (the wire input may be cwd-joined; the subject is what the sender asked
// for); bash shows the ORIGINAL command, before any cd prefix; pymodule_run
// shows repo/script then the args.
func ssStepSubject(p ssStepPlan) string {
	switch {
	case p.step.Read != nil:
		r := p.step.Read
		subject := r.Path
		if r.Start > 0 || r.End > 0 {
			var start, end string
			if r.Start > 0 {
				start = strconv.Itoa(r.Start)
			}
			if r.End > 0 {
				end = strconv.Itoa(r.End)
			}
			subject += ":" + start + "-" + end
		}
		return subject
	case p.step.Bash != nil:
		flat := strings.ReplaceAll(p.step.Bash.Command, "\n", " ")
		return ssTruncateUTF8(flat, sendStepHeaderSubjectMax)
	case p.step.PymoduleRun != nil:
		pm := p.step.PymoduleRun
		subject := pm.Repo + "/" + pm.Script
		if len(pm.Args) > 0 {
			subject += " " + strings.Join(pm.Args, " ")
		}
		return subject
	}
	return ""
}

// ssLargestSteps names the five biggest steps by bytes for the rendered-cap
// refusal, ordered by bytes descending then index ascending.
func ssLargestSteps(plans []ssStepPlan, outcomes []ssRunOutcome) string {
	type ssSized struct {
		index int
		tool  string
		bytes int
	}
	sizes := make([]ssSized, len(plans))
	for i, p := range plans {
		sizes[i] = ssSized{index: p.index, tool: p.tool, bytes: outcomes[i].bytes}
	}
	slices.SortFunc(sizes, func(a, b ssSized) int {
		if c := cmp.Compare(b.bytes, a.bytes); c != 0 {
			return c
		}
		return cmp.Compare(a.index, b.index)
	})
	if len(sizes) > 5 {
		sizes = sizes[:5]
	}
	parts := make([]string, len(sizes))
	for i, s := range sizes {
		parts[i] = fmt.Sprintf("step %d %s %dB", s.index, s.tool, s.bytes)
	}
	return strings.Join(parts, ", ")
}

// ssTruncateUTF8 cuts s to at most max bytes, backing off to a UTF-8 rune
// boundary so the result is always valid UTF-8.
func ssTruncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	k := max
	for k > 0 && !utf8.RuneStart(s[k]) {
		k--
	}
	return s[:k]
}

// shellSingleQuote single-quotes s for a POSIX shell, escaping embedded
// single quotes the `'"'"'` way — the only quoting that survives any cwd.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// randomStepNonce draws the rendered block's delimiter nonce: 8 bytes of
// crypto/rand as 16 hex chars. crypto/rand failing means the OS CSPRNG is
// broken; the nonce only delimits rendered output, so degrading to a
// time-based value keeps the send working rather than failing it.
func randomStepNonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
