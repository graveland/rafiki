// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// newMCPPyModuleExecutor resolves childID's live executor binding and
// returns a tools.PyModuleExecutor proxying pymodule_run calls to it. ok is
// false when the child has no "rafiki/executor" label, or that executor is
// not currently live in pool.Live() -- the caller (getServer) must leave
// ToolOpts.PyModuleExecutor nil in that case, which is how
// mcpPyModuleRunBlueprint declines.
//
// The returned proxy binds the caller's OWN working directory (snap.Cwd) and
// workspace id (rafiki/workspace, present only for a fundi-kind child —
// a claude child has no workspace concept, so its label is always empty) into
// the proxy, never into a method parameter. Both feed bindCwd and the
// ExecuteRequest below: without them every call lands on the executor's ROOT
// registry, whose cwd is the executor's process root — the executor's
// checkout directory, not the calling child's — and a relative `cwd` on the
// call then resolves against the wrong tree, loudly when the name does not
// exist there and silently when it does.
//
// pool is the pymodulePool interface already defined in pymodulesync.go
// (Live() []execpool.LiveExecutor, ConnectClientFor(string)
// (executorpbconnect.ExecutorServiceClient, error)) -- reused here, not
// redefined, and satisfied directly by *execpool.Pool
// (Controller.execPoolConn) in production. Taking it as an interface
// parameter, rather than reaching into Controller fields inside this
// function, is what makes this function testable against
// pymodulesync_test.go's existing fakePymodulePool.
func newMCPPyModuleExecutor(pool pymodulePool, snap childstore.Snapshot) (tools.PyModuleExecutor, bool) {
	executorID := snap.Labels["rafiki/executor"]
	if executorID == "" {
		return nil, false
	}
	live := false
	for _, le := range pool.Live() {
		if le.Executor.ID == executorID {
			live = true
			break
		}
	}
	if !live {
		return nil, false
	}
	client, err := pool.ConnectClientFor(executorID)
	if err != nil {
		return nil, false
	}
	return &mcpPyModuleRunExecutor{
		client:      client,
		callerCwd:   snap.Cwd,
		workspaceID: snap.Labels["rafiki/workspace"],
	}, true
}

// mcpPyModuleRunExecutor holds an already-resolved executor client and
// proxies Run calls to that executor's own "pymodule_run" tool over the
// generic ExecutorService.Execute RPC -- the same RPC fundi uses when its own
// pymodule_run is routed to a remote executor, with the one difference the
// MCP surface cannot avoid: fundi's routing goes through a
// workspace-scoped client, so its Execute carries the child's workspace id,
// while this proxy must supply it from the child's stored labels. No
// script/module validation happens here: the executor's own pymodule_run
// (pkg/fundi/tools/pymodule_run.go, served via pkg/executor's
// MaterializeOnly) already validates everything. The ONE thing this proxy
// owns is cwd resolution — see bindCwd.
type mcpPyModuleRunExecutor struct {
	client executorpbconnect.ExecutorServiceClient
	// callerCwd is the calling child's own working directory: the "your
	// working directory" the tool description promises. Empty (or, never seen
	// in practice, non-absolute — every spawn path enforces absolute) disables
	// bindCwd, leaving the executor's root as the resolution base.
	callerCwd string
	// workspaceID routes the call to the child's own workspace registry when
	// it has one. A claude child has none (empty label), so its calls stay on
	// the root registry and rely entirely on bindCwd for cwd semantics.
	workspaceID string
}

func (e *mcpPyModuleRunExecutor) Run(ctx context.Context, input json.RawMessage) (string, error) {
	stream, err := e.client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:        "pymodule_run",
		InputJson:   e.bindCwd(input),
		TimeoutMs:   600_000, // matches pkg/executorclient.Client.Execute's bound
		WorkspaceId: e.workspaceID,
	}))
	if err != nil {
		return "", fmt.Errorf("pymodule_run: executor execute: %w", err)
	}
	defer stream.Close()

	var resultText string
	var failure *executorpb.Failure
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *executorpb.ExecuteResponse_Result:
			for _, c := range ev.Result.Content {
				if t := c.GetText(); t != "" {
					resultText += t
				}
			}
		case *executorpb.ExecuteResponse_Failed:
			failure = ev.Failed
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("pymodule_run: executor stream: %w", err)
	}
	if failure != nil {
		return "", fmt.Errorf("pymodule_run: executor: %s (code %v)", failure.Message, failure.Code)
	}
	return resultText, nil
}

// bindCwd makes the input's `cwd` mean the CALLING CHILD's working directory
// -- the "your working directory" the tool description promises -- instead of
// the executor's root, which is what the executor-side implementation would
// otherwise resolve against: its registry is materialized with Cwd = root for
// every workspace-less caller, so a relative path and the empty default both
// land there. Absent means the caller's cwd; relative means relative to it,
// exactly like the file tools resolve against a workspace; absolute and
// ~-prefixed pass through untouched, the executor expanding ~ against its own
// home as it always did.
//
// The rewrite happens BEFORE the bytes leave, so the executor-side
// implementation stays untouched: what arrives is an absolute path, and
// resolveToolPath's own cwd join never fires. Fields the proxy does not know
// about survive the round-trip byte-exact (the map carries json.RawMessage,
// not decoded values), keeping the patch surgical.
//
// The raw input is forwarded as-is, rather than rewritten or rejected, when
// the caller carries no usable cwd, or its JSON cannot be read, or `cwd` is
// not a string (json's null is a silent no-op into a string field, so it
// reads as the default on both sides — exactly how the executor's own
// unmarshal treats it): the executor's own unmarshal then reports the
// malformed input, so this proxy never invents a second diagnostic for the
// same bytes.
func (e *mcpPyModuleRunExecutor) bindCwd(input json.RawMessage) json.RawMessage {
	if e.callerCwd == "" || !filepath.IsAbs(e.callerCwd) {
		return input
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return input
	}
	resolved := e.callerCwd
	if raw, ok := fields["cwd"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return input
		}
		if s == "" {
			// Explicit empty means the default, as it does on the executor.
		} else if filepath.IsAbs(s) || strings.HasPrefix(s, "~") {
			return input
		} else {
			resolved = filepath.Join(e.callerCwd, s)
		}
	}
	cwd, err := json.Marshal(resolved)
	if err != nil {
		return input
	}
	fields["cwd"] = cwd
	out, err := json.Marshal(fields)
	if err != nil {
		return input
	}
	return out
}

const mcpPymoduleRunDescription = "Run a Python script you saved with pymodule_put, by its " +
	"module name. `script` is the module name exactly as saved with pymodule_put -- not " +
	"a path, and not inline code; if you have edited a module's code since saving it, " +
	"pymodule_put it again before running. `modules` names further saved pymodules the " +
	"script imports. If the script or any named module declares a requirements block " +
	"(`# pymodule-requirements:`, see pymodule_put), its installed packages are on " +
	"PYTHONPATH for the run and the script's own venv interpreter is used (script only). " +
	"`cwd` optionally sets the working directory -- absolute, or " +
	"relative to your working directory; the default is your working directory. Returns " +
	"combined stdout/stderr and the exit code."

type mcpPyModuleRunBlueprint struct{}

func (mcpPyModuleRunBlueprint) Name() string        { return "pymodule_run" }
func (mcpPyModuleRunBlueprint) Description() string { return mcpPymoduleRunDescription }
func (mcpPyModuleRunBlueprint) InputSchema() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: []tools.SchemaProperty{
			{Name: "script", Type: "string", Description: "Name of the pymodule to run, exactly as saved with pymodule_put (e.g. \"analyze\"). A bare Python identifier, not a path."},
			{Name: "modules", Type: "array", Items: &tools.Schema{Type: "string"}, Description: "Names of further pymodules the script imports, from pymodule_put."},
			{Name: "cwd", Type: "string", Description: "Optional working directory for the run -- absolute, ~-expanded, or relative to your working directory. Default: your working directory."},
			{Name: "args", Type: "array", Items: &tools.Schema{Type: "string"}, Description: "Extra command-line arguments passed to the script."},
		},
		Required: []string{"script"},
	}
}
func (mcpPyModuleRunBlueprint) Execute(context.Context, tools.ToolInput) (tools.ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (mcpPyModuleRunBlueprint) Materialize(opts tools.ToolOpts) (tools.Tool, error) {
	if opts.PyModuleExecutor == nil {
		return nil, nil
	}
	return &mcpPyModuleRunTool{exec: opts.PyModuleExecutor}, nil
}

type mcpPyModuleRunTool struct {
	mcpPyModuleRunBlueprint
	exec tools.PyModuleExecutor
}

func (t *mcpPyModuleRunTool) Execute(ctx context.Context, input tools.ToolInput) (tools.ToolResult, error) {
	out, err := t.exec.Run(ctx, json.RawMessage(input))
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.NewTextResult(out), nil
}
