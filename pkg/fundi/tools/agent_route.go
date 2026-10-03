package tools

import (
	"context"
	"errors"
	"fmt"
)

const agentRouteDescription = "Change where a child's model requests are served, " +
	"effective on its next request. spec is a bracket-free routing spec such as " +
	"\"prefer=fireworks\" (try that provider first, fall back to the rest) or " +
	"\"sort=throughput,quant=fp8+\". It merges over the child's current routing. You " +
	"may steer your own subtree only, and you may set prefer, sort and quant — only= " +
	"(a hard provider pin) is reserved for the operator."

// --- agent_route ---

// AgentRouteBlueprint is the agent-facing steering verb: it hands a spec delta
// to the spawner, which owns the authority rule. The tool re-implements
// nothing — subtree membership and the child-provenance only= refusal live in
// the daemon method the spawner calls, so the MCP face and the Connect RPC
// cannot drift apart.
type AgentRouteBlueprint struct{}

func (AgentRouteBlueprint) Name() string        { return "agent_route" }
func (AgentRouteBlueprint) Description() string { return agentRouteDescription }
func (AgentRouteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "agent_id", Type: "string",
				Description: "Id of the agent to steer, as shown by agent_list (e.g. \"c_01J…\")."},
			{Name: "spec", Type: "string",
				Description: "Bracket-free routing-spec delta, e.g. \"prefer=fireworks\" or " +
					"\"sort=throughput,quant=fp8+\". Merged over the agent's current routing."},
		},
		Required: []string{"agent_id", "spec"},
	}
}

func (AgentRouteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentRouteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentRouteTool{agents: opts.Agents}, nil
}

type agentRouteTool struct {
	AgentRouteBlueprint
	agents AgentSpawner
}

func (t *agentRouteTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		AgentID string `json:"agent_id"`
		Spec    string `json:"spec"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_route: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.AgentID == "" {
		return ToolResult{}, errors.New("agent_route: agent_id is required; use agent_list to find the id")
	}
	if params.Spec == "" {
		return ToolResult{}, errors.New("agent_route: spec is required")
	}
	routing, err := t.agents.SetRouting(ctx, params.AgentID, params.Spec)
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_route: %w", err)
	}
	return NewTextResult(fmt.Sprintf("routing for %s is now: %s\n", params.AgentID, routing)), nil
}
