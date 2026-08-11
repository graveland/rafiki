package tools

import (
	"context"
	"errors"
	"fmt"
)

func init() {
	DefaultBlueprint.Register(&AgentViewBlueprint{})
	DefaultBlueprint.Register(&AgentSendBlueprint{})
	DefaultBlueprint.Register(&AgentKillBlueprint{})
}

const (
	agentViewDescription = "Read the recent transcript of an agent you spawned: its " +
		"prompts, what it said, and the tools it called with their results. Use this " +
		"to check on a worker that seems stuck, or to understand a result before " +
		"acting on it.\n\n" +
		"For \"what is it actually working on\", prefer task_list with assignee set — " +
		"that is one indexed read of what the agent decided, where this is a wall of " +
		"transcript you have to interpret."

	agentSendDescription = "Send a message to an agent you spawned. Use it to steer a " +
		"worker mid-flight (\"also cover the error path\"), to answer something it is " +
		"blocked on, or to give it the next piece of work when it has settled. The " +
		"message is queued and picked up on its next turn."

	agentKillDescription = "Stop an agent you spawned and everything it spawned in " +
		"turn. Returns once the shutdown is complete and recorded. Its unfinished " +
		"tasks are swept to `orphaned` with the assignee retained, so you can see " +
		"what it was holding — reassign them or drop them with a reason."
)

// agentIDSchema is the one property every steering verb shares.
func agentIDSchema(extra ...SchemaProperty) Schema {
	props := []SchemaProperty{
		{Name: "agent", Type: "string",
			Description: "Id of the agent, as shown by agent_list (e.g. \"c_01J…\")."},
	}
	props = append(props, extra...)
	req := []string{"agent"}
	for _, p := range extra {
		if p.Name == "message" {
			req = append(req, "message")
		}
	}
	return Schema{Type: "object", Properties: props, Required: req}
}

// --- agent_view ---

type AgentViewBlueprint struct{}

func (AgentViewBlueprint) Name() string        { return "agent_view" }
func (AgentViewBlueprint) Description() string { return agentViewDescription }
func (AgentViewBlueprint) InputSchema() Schema {
	return agentIDSchema(SchemaProperty{
		Name: "limit", Type: "integer",
		Description: "How many recent transcript entries to show (default 40, max 200).",
	})
}

func (AgentViewBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentViewBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentViewTool{agents: opts.Agents}, nil
}

type agentViewTool struct {
	AgentViewBlueprint
	agents AgentSpawner
}

func (t *agentViewTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Agent string `json:"agent"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_view: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Agent == "" {
		return ToolResult{}, errors.New("agent_view: agent is required; use agent_list to find the id")
	}
	text, err := t.agents.View(ctx, params.Agent, params.Limit)
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_view: %w", err)
	}
	return NewTextResult(text), nil
}

// --- agent_send ---

type AgentSendBlueprint struct{}

func (AgentSendBlueprint) Name() string        { return "agent_send" }
func (AgentSendBlueprint) Description() string { return agentSendDescription }
func (AgentSendBlueprint) InputSchema() Schema {
	return agentIDSchema(SchemaProperty{
		Name: "message", Type: "string",
		Description: "The message to deliver.",
	})
}

func (AgentSendBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentSendBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentSendTool{agents: opts.Agents}, nil
}

type agentSendTool struct {
	AgentSendBlueprint
	agents AgentSpawner
}

func (t *agentSendTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Agent   string `json:"agent"`
		Message string `json:"message"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_send: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Agent == "" {
		return ToolResult{}, errors.New("agent_send: agent is required; use agent_list to find the id")
	}
	if params.Message == "" {
		return ToolResult{}, errors.New("agent_send: message is required")
	}
	if err := t.agents.Send(ctx, params.Agent, params.Message); err != nil {
		return ToolResult{}, fmt.Errorf("agent_send: %w", err)
	}
	return NewTextResult("delivered to " + params.Agent + "\n"), nil
}

// --- agent_kill ---

type AgentKillBlueprint struct{}

func (AgentKillBlueprint) Name() string        { return "agent_kill" }
func (AgentKillBlueprint) Description() string { return agentKillDescription }
func (AgentKillBlueprint) InputSchema() Schema { return agentIDSchema() }

func (AgentKillBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentKillBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentKillTool{agents: opts.Agents}, nil
}

type agentKillTool struct {
	AgentKillBlueprint
	agents AgentSpawner
}

func (t *agentKillTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Agent string `json:"agent"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_kill: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Agent == "" {
		return ToolResult{}, errors.New("agent_kill: agent is required; use agent_list to find the id")
	}
	if err := t.agents.Kill(ctx, params.Agent); err != nil {
		return ToolResult{}, fmt.Errorf("agent_kill: %w", err)
	}
	return NewTextResult("stopped " + params.Agent + "\n"), nil
}
