package tools

import (
	"context"
	"errors"
	"fmt"
)

func init() {
	DefaultBlueprint.Register(&AgentSpawnBlueprint{})
}

const agentSpawnDescription = "Spawn a subagent to do a piece of work in parallel " +
	"with you. Returns immediately with the new agent's id — it does NOT wait for " +
	"the work to finish. You will be notified when it settles (told why: done, hit " +
	"a cost budget, or failed), and pinged periodically while it is still working on " +
	"something long. Do not sleep, poll, or repeatedly call agent_list/agent_view to " +
	"check whether it is done — that costs you a turn each time and tells you nothing " +
	"sooner than the notification will. Keep doing your own work in the meantime; use " +
	"agent_list/agent_view only when you actually need to look something up (which " +
	"agent is which, or what one has said so far), not as a waiting loop.\n\n" +
	"Give it a `prompt` that stands on its own: the subagent starts with none of " +
	"your context and cannot ask you a follow-up question mid-turn. Pass `task` " +
	"(a handle from your own task list, like \"2.1\") to hand it a specific unit of " +
	"work — the task is assigned to it atomically, so agent_list and task_list agree.\n\n" +
	"Use a subagent when the work is genuinely separable — a review, an independent " +
	"implementation, an investigation you do not want in your own context. Do not " +
	"spawn one for a step you could just do."

type AgentSpawnBlueprint struct{}

func (AgentSpawnBlueprint) Name() string        { return "agent_spawn" }
func (AgentSpawnBlueprint) Description() string { return agentSpawnDescription }
func (AgentSpawnBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "prompt", Type: "string",
				Description: "What the subagent should do. Must stand alone — it inherits none of your context."},
			{Name: "name", Type: "string",
				Description: "Short human-readable name (\"reviewer\", \"impl-auth\"). Optional but makes agent_list readable."},
			{Name: "model", Type: "string",
				Description: "Model id to run it on. Omit to inherit the daemon default. Use agent_models to see the options."},
			{Name: "cwd", Type: "string",
				Description: "Absolute working directory. Omit to use your own."},
			{Name: "task", Type: "string",
				Description: "Handle of a task in YOUR list (e.g. \"2.1\") to assign to this agent."},
			{Name: "kind", Type: "string",
				Description: "Agent runtime: \"fundi\" (default) or \"claude\"."},
			{Name: "max_depth", Type: "integer",
				Description: "How many further levels of agents this one may spawn. 0 = it cannot spawn. Default 1."},
			{Name: "max_cost", Type: "number",
				Description: "USD budget for this agent and everything it spawns. Omit to inherit no limit — but if you are coordinating, set one."},
			{Name: "max_children", Type: "integer",
				Description: "How many agents may be alive beneath it at once. Default 4."},
			{Name: "executor", Type: "string",
				Description: "Where to run this agent, as a label selector over machines " +
					"(e.g. \"env=work,os=linux\"). Omit to confine it to the same machines " +
					"you are confined to — omitting narrows it to your reach, it does not " +
					"free it. You can only ever narrow: a selector naming a machine you " +
					"cannot reach is refused, and the refusal says which machine and why."},
			{Name: "workspace", Type: "string",
				Description: "\"ephemeral\" gives the agent a fresh, isolated checkout that can " +
					"be rebuilt elsewhere if its machine goes away — right for unattended " +
					"workers. \"pinned\" puts it in an existing working tree on one specific " +
					"machine, so it sees your uncommitted changes but cannot be moved. " +
					"Omit to inherit yours."},
		},
		Required: []string{"prompt"},
	}
}

func (AgentSpawnBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentSpawnBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentSpawnTool{agents: opts.Agents, cwd: opts.Cwd}, nil
}

type agentSpawnTool struct {
	AgentSpawnBlueprint
	agents AgentSpawner
	cwd    string
}

func (t *agentSpawnTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Prompt      string   `json:"prompt"`
		Name        string   `json:"name,omitempty"`
		Model       string   `json:"model,omitempty"`
		Cwd         string   `json:"cwd,omitempty"`
		Task        string   `json:"task,omitempty"`
		Kind        string   `json:"kind,omitempty"`
		MaxDepth    *int     `json:"max_depth,omitempty"`
		MaxCost     *float64 `json:"max_cost,omitempty"`
		MaxChildren *int     `json:"max_children,omitempty"`
		Executor    string   `json:"executor,omitempty"`
		Workspace   string   `json:"workspace,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_spawn: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Prompt == "" {
		return ToolResult{}, errors.New("agent_spawn: prompt is required — a subagent with nothing to do costs a process and a model context for no work")
	}
	cwd := params.Cwd
	if cwd == "" {
		cwd = t.cwd
	}

	// An unknown workspace mode is refused at the tool, not silently defaulted.
	// "ephemeral" and "pinned" mean genuinely different things about whether
	// the worker's uncommitted work can survive a machine going away.
	if params.Workspace != "" && params.Workspace != "ephemeral" && params.Workspace != "pinned" {
		return ToolResult{}, fmt.Errorf("agent_spawn: unknown workspace mode %q — must be \"ephemeral\" or \"pinned\"", params.Workspace)
	}

	// SpawnSpec carries no parent. The implementation supplies the caller's
	// own id, which it closed over at construction.
	info, err := t.agents.Spawn(ctx, SpawnSpec{
		Name:             params.Name,
		Model:            params.Model,
		Cwd:              cwd,
		Prompt:           params.Prompt,
		Task:             params.Task,
		Kind:             params.Kind,
		MaxDepth:         params.MaxDepth,
		MaxCost:          params.MaxCost,
		MaxChildren:      params.MaxChildren,
		ExecutorSelector: params.Executor,
		WorkspaceMode:    params.Workspace,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_spawn: %w", err)
	}
	return NewTextResult(RenderAgents([]AgentInfo{info})), nil
}
