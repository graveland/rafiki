package tools

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/prefill"
	"go.graveland.dev/rafiki/pkg/protocol"
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
	"spawn one for a step you could just do.\n\n" +
	"Prefer `preset` to choosing a model yourself: a preset (see preset_list) " +
	"fixes the operator's seat policy for a role, and your other fields narrow " +
	"what it grants."

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
				Description: "Overrides the preset's model; only set this when the human named a model. " +
					"Model id to run it on. Omit to inherit the daemon default. Use agent_models to see the options."},
			{Name: "cwd", Type: "string",
				Description: "Absolute working directory for the agent: where its tools start and " +
					"relative paths resolve. For an executor-bound agent the executor must be " +
					"able to see this path in its own filesystem — provisioning is refused if " +
					"it cannot. Omit to use your own. This is how you point a worker at a git " +
					"worktree you created."},
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
				Description: "Where to run this agent: a label selector over machines " +
					"(e.g. \"env=work,os=linux\"), or a bare machine name (e.g. \"greyshift\") " +
					"to target that one executor by name, like the CLI's --executor. " +
					"Omit to use the machines you already reach: a spawned agent " +
					"inherits your confinement, and a top-level agent (no parent, e.g. " +
					"one spawned via MCP) gets any live executor that admits it. You " +
					"can only ever narrow: naming a machine you cannot reach is " +
					"refused, and the refusal says which machine and why. On a daemon " +
					"with no executor pool the agent has no filesystem tools."},
			{Name: "workspace", Type: "string",
				Description: "Which executors may serve this agent, by the workspace_mode their " +
					"operator declared, and what happens if that executor is lost. " +
					"\"ephemeral\": only executors marked ephemeral (operator-declared " +
					"reconstructible, e.g. disposable containers each carrying their own " +
					"checkout); if its executor is lost the daemon re-binds the agent onto " +
					"another one. \"pinned\": only executors marked pinned (the default); if " +
					"its executor is lost the agent fails where it stood. Neither mode " +
					"creates a fresh or isolated checkout — every workspace on an executor " +
					"serves that executor's single root. For an isolated tree, create a git " +
					"worktree yourself and pass it as cwd. Omit to inherit yours."},
			{Name: "preset", Type: "string",
				Description: "Name of a preset (`<group>:<role>`, e.g. default:implementer) fixing this agent's kind, model, tools, system prompt and budget. Prefer this to choosing a model. See preset_list."},
			{Name: "thinking", Type: "string",
				Description: "off|low|medium|high|xhigh; overrides the preset's."},
			{Name: "append_system_prompt", Type: "string",
				Description: "Extra system-prompt text appended after the preset's. Keep it identical across a wave of workers so they share the prompt cache; put per-worker instructions in `prompt`."},
			{Name: "tools", Type: "array", Items: &Schema{Type: "string"},
				Description: "Narrow the preset: a subset of what it allows, or [] for none. Cannot widen it."},
			{Name: "skills", Type: "array", Items: &Schema{Type: "string"},
				Description: "Narrow the preset: a subset of what it allows, or [] for none. Cannot widen it."},
			{Name: "mcp_servers", Type: "array", Items: &Schema{Type: "string"},
				Description: "Narrow the preset: a subset of what it allows, or [] for none. Cannot widen it."},
			{Name: "context_files", Type: "boolean",
				Description: "false to skip CLAUDE.md/AGENTS.md context files. Cannot re-enable them if the preset disables them."},
			{Name: "prefill", Type: "array", Items: &Schema{Type: "string"},
				Description: "Files the agent starts having already read, one entry each: a path resolved against " +
					"the SPAWNED agent's cwd (not yours; use an absolute path for files outside that cwd), " +
					"optionally with a 1-based inclusive line range (path:10-40, path:200-, path:-80), or a glob " +
					"(src/**/*.rs, no range). The reads run on the agent's own machine before its first turn and " +
					"cost you almost nothing to send; use this instead of pasting file contents into prompt. " +
					"Put CLAUDE.md or skill files here too, with ranges. Needs the read tool (and glob for globs)."},
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
		Prompt             string    `json:"prompt"`
		Name               string    `json:"name,omitempty"`
		Model              string    `json:"model,omitempty"`
		Cwd                string    `json:"cwd,omitempty"`
		Task               string    `json:"task,omitempty"`
		Kind               string    `json:"kind,omitempty"`
		MaxDepth           *int      `json:"max_depth,omitempty"`
		MaxCost            *float64  `json:"max_cost,omitempty"`
		MaxChildren        *int      `json:"max_children,omitempty"`
		Executor           string    `json:"executor,omitempty"`
		Workspace          string    `json:"workspace,omitempty"`
		Preset             string    `json:"preset,omitempty"`
		Thinking           string    `json:"thinking,omitempty"`
		AppendSystemPrompt string    `json:"append_system_prompt,omitempty"`
		Tools              *[]string `json:"tools,omitempty"`
		Skills             *[]string `json:"skills,omitempty"`
		MCPServers         *[]string `json:"mcp_servers,omitempty"`
		ContextFiles       *bool     `json:"context_files,omitempty"`
		Prefill            []string  `json:"prefill,omitempty"`
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

	// A pre-fill list is parsed at the tool, so a malformed entry fails the
	// call instead of failing inside the child's engine worker; an absent
	// field (nil) means no pre-fill, while an explicit empty list is a parse
	// error like any other.
	var prefillReads []protocol.PrefillRead
	if params.Prefill != nil {
		parsed, err := prefill.ParseEntries(params.Prefill)
		if err != nil {
			return ToolResult{}, fmt.Errorf("agent_spawn: %w", err)
		}
		prefillReads = parsed
	}

	// SpawnSpec carries no parent. The implementation supplies the caller's
	// own id, which it closed over at construction.
	info, err := t.agents.Spawn(ctx, SpawnSpec{
		Name:               params.Name,
		Model:              params.Model,
		Cwd:                cwd,
		Prompt:             params.Prompt,
		Task:               params.Task,
		Kind:               params.Kind,
		MaxDepth:           params.MaxDepth,
		MaxCost:            params.MaxCost,
		MaxChildren:        params.MaxChildren,
		ExecutorSelector:   params.Executor,
		WorkspaceMode:      params.Workspace,
		Preset:             params.Preset,
		Thinking:           params.Thinking,
		AppendSystemPrompt: params.AppendSystemPrompt,
		Tools:              params.Tools,
		Skills:             params.Skills,
		MCPServers:         params.MCPServers,
		ContextFiles:       params.ContextFiles,
		Prefill:            prefillReads,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_spawn: %w", err)
	}
	return NewTextResult(RenderAgents([]AgentInfo{info})), nil
}
