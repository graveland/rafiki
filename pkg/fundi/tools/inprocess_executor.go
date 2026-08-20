package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
)

// NewInProcessExecutor returns an ExecutorClient that runs the workspace tools
// in THIS process, plus the set of tool names it can serve.
//
// It exists for a process that is genuinely its own workspace: the standalone
// `rafikid fundi` mode, which has no daemon, no pool, and nobody else's
// children to protect. Such a process satisfies the executor rule with a real
// client rather than an exemption, so there is exactly one rule and nothing
// routed around it — which is the failure mode this repo keeps finding.
//
// It deliberately does NOT serve the background-job verbs. Those need a job
// registry, which belongs to pkg/executor; advertising them here would put three
// tools in tools[] that can only fail.
func NewInProcessExecutor(opts ToolOpts) (ExecutorClient, map[string]bool) {
	names := slices.DeleteFunc(WorkspaceTools(), func(n string) bool {
		return n == "bash_start" || n == "bash_output" || n == "bash_kill"
	})

	// The same builder the executor process uses, so the standalone mode and a
	// real executor materialize identical tools from identical opts.
	reg := DefaultBlueprint.MaterializeOnly(opts, names)

	served := make(map[string]bool, len(names))
	for _, def := range reg.Definitions() {
		if def.OfTool != nil {
			served[def.OfTool.Name] = true
		}
	}
	return &inProcessExecutor{reg: reg}, served
}

// inProcessExecutor adapts a Registry to ExecutorClient. Execute is a direct
// call: Registry.Execute and ExecutorClient.Execute already have identical
// signatures, which is what makes this an adapter rather than a translation.
type inProcessExecutor struct{ reg *Registry }

func (e *inProcessExecutor) Execute(ctx context.Context, tool string, input json.RawMessage) (string, error) {
	return e.reg.Execute(ctx, tool, input)
}

var errNoJobRegistry = errors.New(
	"background jobs need an executor process; this agent runs its tools in-process")

func (e *inProcessExecutor) StartJob(context.Context, string) (string, error) {
	return "", errNoJobRegistry
}
func (e *inProcessExecutor) JobOutput(context.Context, string, int64) (JobSnapshot, error) {
	return JobSnapshot{}, errNoJobRegistry
}
func (e *inProcessExecutor) KillJob(context.Context, string) error { return errNoJobRegistry }
func (e *inProcessExecutor) Ping(context.Context) error            { return nil }
