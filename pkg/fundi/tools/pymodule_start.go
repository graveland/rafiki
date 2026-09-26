// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

func init() { DefaultBlueprint.Register(&PyModuleStartBlueprint{}) }

// pymoduleStartDescription is written for a FUNDI child (the registry
// surface). The MCP face ships its own wording (cmd/rafikid's
// mcp_pymodule_start.go) — a face caller has no guaranteed notification
// channel, so the settlement promise is worded differently there.
const pymoduleStartDescription = "Start a saved Python script as a rafiki SCRIPT child — a " +
	"daemon-managed process that runs to completion on its own, with no " +
	"model, no API key and no turn loop. Like agent_spawn, it returns " +
	"immediately with the child's id (it does NOT wait for the script to " +
	"finish); you are notified when it settles — exit 0 settles it done, " +
	"anything else failed. Choose this over pymodule_run when the work " +
	"should run on its own: a workflow driver that orchestrates subagents, " +
	"a long-running poller, a batch job too long to hold your turn. Choose " +
	"pymodule_run instead when you want the output back in this " +
	"conversation: it blocks until the script exits and returns its " +
	"stdout, costing nothing to steer.\n\n" +
	"`repo` picks the source: \"local\" for your own saved pymodules " +
	"(pymodule_put), or a git source's name as reported by discovery. " +
	"`script` is the module name exactly as saved with pymodule_put -- not " +
	"a path, and not inline code. `modules` names further saved pymodules " +
	"the script imports, resolved within the same repo. The child gets the " +
	"ordinary child furniture: `labels` (key=value pairs for `rafiki list` " +
	"filters; the rafiki/ prefix and the owner key are daemon-reserved), " +
	"`max_cost` (USD budget for the child and everything IT spawns — set " +
	"one when you are coordinating), `max_children` (how many agents may " +
	"be alive beneath it), and `executor` (a label selector or a bare " +
	"machine name, like agent_spawn's). The child's name defaults to the " +
	"script's name."

type PyModuleStartBlueprint struct{}

func (PyModuleStartBlueprint) Name() string        { return "pymodule_start" }
func (PyModuleStartBlueprint) Description() string { return pymoduleStartDescription }
func (PyModuleStartBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "repo", Type: "string", Description: "Which pymodule source to use: \"local\" for your own saved pymodules (pymodule_put), or a git source's name as reported by discovery."},
			{Name: "script", Type: "string", Description: "Name of the pymodule to run as the child (e.g. \"driver\"): for repo \"local\", exactly as saved with pymodule_put; otherwise a discovered script name of the repo, as reported by discovery. A bare Python identifier, not a path."},
			{Name: "modules", Type: "array", Items: &Schema{Type: "string"}, Description: "Names of further pymodules the script imports, resolved within the same repo: for repo \"local\", from pymodule_put; otherwise discovered packages of the repo, as reported by discovery."},
			{Name: "args", Type: "array", Items: &Schema{Type: "string"}, Description: "Extra command-line arguments passed to the script."},
			{Name: "labels", Type: "object", Description: "User labels on the child, as key=value pairs (e.g. {\"env\": \"work\"}) for rafiki list filters. The rafiki/ and fundi/ prefixes and the \"owner\" key are daemon-reserved and refused."},
			{Name: "max_cost", Type: "number", Description: "USD budget for this child and everything it spawns. Omit to inherit no limit — but if the script will spawn subagents, set one."},
			{Name: "max_children", Type: "integer", Description: "How many agents may be alive beneath this child at once. Default 4."},
			{Name: "executor", Type: "string", Description: "Where to run this child: a label selector over machines (e.g. \"env=work,os=linux\"), or a bare machine name (e.g. \"greyshift\") to target that one executor, like the CLI's --executor. Omit to inherit your confinement; you can only ever narrow."},
			{Name: "cwd", Type: "string", Description: "Absolute working directory for the child's process. Omit to use your own. Only set this when the script must start somewhere specific — the script's code always comes from the synced pymodule cache, never from this directory."},
		},
		Required: []string{"script", "repo"},
	}
}
func (PyModuleStartBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (PyModuleStartBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &pymoduleStartTool{PyModuleStartBlueprint: PyModuleStartBlueprint{}, agents: opts.Agents, cwd: opts.Cwd}, nil
}

type pymoduleStartTool struct {
	PyModuleStartBlueprint
	agents AgentSpawner
	cwd    string
}

type pymoduleStartInput struct {
	Repo        string            `json:"repo"`
	Script      string            `json:"script"`
	Modules     []string          `json:"modules"`
	Args        []string          `json:"args"`
	Labels      map[string]string `json:"labels"`
	MaxCost     *float64          `json:"max_cost,omitempty"`
	MaxChildren *int              `json:"max_children,omitempty"`
	Executor    string            `json:"executor,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
}

func (t *pymoduleStartTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in pymoduleStartInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_start: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	// `repo` is required by the schema, but a model can still omit it: fail
	// loudly instead of silently defaulting, exactly like pymodule_run — an
	// implicit "local" would leave a git-sourced call silently running (or
	// missing) a same-named saved module, with nothing in the result saying
	// which scope it started.
	if in.Repo == "" {
		return ToolResult{}, errors.New("pymodule_start: repo is required: \"local\" for your own saved pymodules (pymodule_put), or a git source's name as reported by discovery")
	}
	// A non-local repo is validated by the same rule pymodule_run applies to
	// it: the name becomes a path segment under the git cache on the hosting
	// side, and the daemon re-checks it against the registered sources.
	if in.Repo != pymodules.LocalRepo {
		if err := pymodules.ValidName(in.Repo); err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_start: repo: %w", err)
		}
	}
	// Name validation mirrors pymodule_run's: each name becomes a path
	// segment and an import on the hosting side, and the daemon re-checks
	// every one — the tool-side copy is what turns a typo into a refused
	// call instead of a child that spawns and immediately fails.
	if err := pymodules.ValidName(in.Script); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_start: script: %w", err)
	}
	for _, m := range in.Modules {
		if err := pymodules.ValidName(m); err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_start: module %q: %w", m, err)
		}
	}

	cwd := in.Cwd
	if cwd == "" {
		cwd = t.cwd
	}

	info, err := t.agents.Spawn(ctx, SpawnSpec{
		// The child's name defaults to the script's name: `rafiki list`
		// renders a name when it has one, and a fleet of same-script
		// drivers reading as "driver" is the honest summary of what they
		// are. Ids stay unique regardless.
		Name: in.Script,
		Kind: protocol.KindScript,
		Script: &protocol.ScriptSpec{
			Repo:    in.Repo,
			Script:  in.Script,
			Modules: in.Modules,
			Args:    in.Args,
		},
		Cwd:              cwd,
		Labels:           in.Labels,
		MaxCost:          in.MaxCost,
		MaxChildren:      in.MaxChildren,
		ExecutorSelector: in.Executor,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_start: %w", err)
	}
	return NewTextResult(RenderAgents([]AgentInfo{info})), nil
}
