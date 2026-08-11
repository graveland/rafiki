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
	"the work to finish. You will be told when it settles; until then you can keep " +
	"working, and you can watch it with agent_list and agent_view.\n\n" +
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
		Prompt string `json:"prompt"`
		Name   string `json:"name,omitempty"`
		Model  string `json:"model,omitempty"`
		Cwd    string `json:"cwd,omitempty"`
		Task   string `json:"task,omitempty"`
		Kind   string `json:"kind,omitempty"`
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

	// SpawnSpec carries no parent. The implementation supplies the caller's
	// own id, which it closed over at construction.
	info, err := t.agents.Spawn(ctx, SpawnSpec{
		Name:   params.Name,
		Model:  params.Model,
		Cwd:    cwd,
		Prompt: params.Prompt,
		Task:   params.Task,
		Kind:   params.Kind,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_spawn: %w", err)
	}
	return NewTextResult(RenderAgents([]AgentInfo{info})), nil
}
