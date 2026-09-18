// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

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
	return &mcpPyModuleRunExecutor{client: client}, true
}

// mcpPyModuleRunExecutor holds an already-resolved executor client and
// proxies Run calls to that executor's own "pymodule_run" tool over the
// generic ExecutorService.Execute RPC -- the same RPC and tool name fundi
// uses when its own pymodule_run is routed to a remote executor. No
// script/module validation happens here: the executor's own pymodule_run
// (pkg/fundi/tools/pymodule_run.go, served via pkg/executor's
// MaterializeOnly) already validates everything: this is a thin proxy, not
// a second implementation.
type mcpPyModuleRunExecutor struct {
	client executorpbconnect.ExecutorServiceClient
}

func (e *mcpPyModuleRunExecutor) Run(ctx context.Context, input json.RawMessage) (string, error) {
	stream, err := e.client.Execute(ctx, connect.NewRequest(&executorpb.ExecuteRequest{
		Tool:      "pymodule_run",
		InputJson: input,
		TimeoutMs: 600_000, // matches pkg/executorclient.Client.Execute's bound
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

const mcpPymoduleRunDescription = "Run a Python script in your own executor workspace, " +
	"with named modules from your pymodule store made importable first. `script` is the " +
	"basename of a file you already wrote (with your own file-writing tool) in your " +
	"working directory -- not a path, and not inline code. `modules` names the pymodules " +
	"(saved with pymodule_put) your script imports. Returns combined stdout/stderr and the " +
	"exit code. This tool is present only when you are running as a rafiki-managed agent " +
	"with a live executor binding -- it is absent otherwise, never merely erroring, so its " +
	"absence from your tool list means there is no workspace for it to run in right now."

type mcpPyModuleRunBlueprint struct{}

func (mcpPyModuleRunBlueprint) Name() string        { return "pymodule_run" }
func (mcpPyModuleRunBlueprint) Description() string { return mcpPymoduleRunDescription }
func (mcpPyModuleRunBlueprint) InputSchema() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: []tools.SchemaProperty{
			{Name: "script", Type: "string", Description: "Basename of the entry script, e.g. \"analyze.py\". No path components."},
			{Name: "modules", Type: "array", Items: &tools.Schema{Type: "string"}, Description: "Names of pymodules to make importable, from pymodule_put."},
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
