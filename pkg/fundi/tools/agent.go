package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func init() {
	DefaultBlueprint.Register(&AgentListBlueprint{})
	DefaultBlueprint.Register(&AgentModelsBlueprint{})
}

// AgentInfo is one descendant, as the daemon sees it. Every field is
// daemon-stamped: nothing here is asserted by the child it describes.
type AgentInfo struct {
	ChildID string
	Name    string
	Model   string
	Status  string // spawning|idle|streaming|tool_running|compacting|blocked_ui|shutting_down|exited
	Cwd     string
	// Depth is the number of hops from the agent that asked, so a
	// coordinator can tell a worker from a worker's reviewer.
	Depth int
	// Task is the handle of the ledger row assigned to this child at spawn,
	// or "" when it was spawned without one.
	Task string
}

// ModelInfo is one model an agent may spawn a child on.
type ModelInfo struct {
	ID       string
	Provider string
}

// SpawnSpec is a request to create a child. It carries NO parent field: the
// implementation supplies the caller's own id, which is why an agent cannot
// spawn into somebody else's subtree.
type SpawnSpec struct {
	Name   string
	Model  string
	Cwd    string
	Prompt string
	// Task is a handle in the CALLER's ledger to assign to the new child.
	// Assignment happens only after the controller admits the spawn, so a
	// refused spawn leaves no row pointing at a child that never started.
	Task string
	// Kind selects the child runtime ("fundi", "claude", "pi"). Empty means
	// fundi.
	Kind string
}

// AgentSpawner is the daemon-side capability behind the agent_* tools.
//
// Every method is scoped to the subtree of the ONE child this value was
// constructed for. Implementations must reject a childID that is not a
// descendant, reading stored lineage rather than any argument. The fundi
// tools package cannot import cmd/rafikid, so the daemon provides the
// implementation — the same seam as LSPClient and ExecutorClient.
type AgentSpawner interface {
	// List returns every live and exited descendant, nearest first.
	List(ctx context.Context) ([]AgentInfo, error)
	// Models enumerates the models a child may be spawned on.
	Models(ctx context.Context) ([]ModelInfo, error)
	// Spawn creates a descendant and returns it once it is registered.
	Spawn(ctx context.Context, spec SpawnSpec) (AgentInfo, error)
	// View returns the tail of a descendant's transcript as plain text.
	// limit caps the number of transcript entries; 0 means the default.
	View(ctx context.Context, childID string, limit int) (string, error)
	// Send delivers a prompt to a descendant.
	Send(ctx context.Context, childID, message string) error
	// Kill shuts a descendant down and waits for the exit to be recorded.
	Kill(ctx context.Context, childID string) error
}

const (
	agentListDescription = "List the agents you have spawned (your subagents and " +
		"anything they spawned in turn). Shows each one's id, name, model, current " +
		"status, working directory and assigned task handle. Takes no arguments — " +
		"you only ever see your own subtree. Use this before agent_send or " +
		"agent_kill to find the id you mean."

	agentModelsDescription = "List the models you may spawn an agent on. Use this " +
		"before agent_spawn when you want to put a worker on a cheaper or a stronger " +
		"model than your own, rather than guessing at a model id."
)

// --- agent_list ---

type AgentListBlueprint struct{}

func (AgentListBlueprint) Name() string        { return "agent_list" }
func (AgentListBlueprint) Description() string { return agentListDescription }
func (AgentListBlueprint) InputSchema() Schema {
	return Schema{Type: "object", Properties: []SchemaProperty{}}
}

func (AgentListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentListTool{agents: opts.Agents}, nil
}

type agentListTool struct {
	AgentListBlueprint
	agents AgentSpawner
}

func (t *agentListTool) Execute(ctx context.Context, _ ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	kids, err := t.agents.List(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_list: %w", err)
	}
	return NewTextResult(RenderAgents(kids)), nil
}

// RenderAgents formats a descendant list for the model. Exported so the
// daemon-side settle digest renders identically to what agent_list shows.
func RenderAgents(kids []AgentInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d agent(s)\n", len(kids))
	for _, k := range kids {
		indent := strings.Repeat("  ", k.Depth)
		fmt.Fprintf(&sb, "%s%s  %s  [%s]  %s", indent, k.ChildID, orDash(k.Name), k.Status, orDash(k.Model))
		if k.Task != "" {
			fmt.Fprintf(&sb, "  task=%s", k.Task)
		}
		if k.Cwd != "" {
			fmt.Fprintf(&sb, "  cwd=%s", k.Cwd)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- agent_models ---

type AgentModelsBlueprint struct{}

func (AgentModelsBlueprint) Name() string        { return "agent_models" }
func (AgentModelsBlueprint) Description() string { return agentModelsDescription }
func (AgentModelsBlueprint) InputSchema() Schema {
	return Schema{Type: "object", Properties: []SchemaProperty{}}
}

func (AgentModelsBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentModelsBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentModelsTool{agents: opts.Agents}, nil
}

type agentModelsTool struct {
	AgentModelsBlueprint
	agents AgentSpawner
}

func (t *agentModelsTool) Execute(ctx context.Context, _ ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	models, err := t.agents.Models(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_models: %w", err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d model(s)\n", len(ids))
	for _, id := range ids {
		sb.WriteString(id)
		sb.WriteString("\n")
	}
	return NewTextResult(sb.String()), nil
}
