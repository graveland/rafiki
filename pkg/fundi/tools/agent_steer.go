package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func init() {
	DefaultBlueprint.Register(&AgentViewBlueprint{})
	DefaultBlueprint.Register(&AgentSendBlueprint{})
	DefaultBlueprint.Register(&AgentKillBlueprint{})
	DefaultBlueprint.Register(&AgentSetBudgetBlueprint{})
}

const (
	agentViewDescription = "Read the recent transcript of an agent you spawned: its " +
		"prompts, what it said, and the tools it called with their results. Use this " +
		"to check on a worker that seems stuck, or to understand a result before " +
		"acting on it — not in a loop to poll for completion; you are notified when a " +
		"subagent settles.\n\n" +
		"For \"what is it actually working on\", prefer task_list with assignee set — " +
		"that is one indexed read of what the agent decided, where this is a wall of " +
		"transcript you have to interpret."

	agentSendDescription = "Send a message to an agent you spawned. Use it to steer a " +
		"worker mid-flight (\"also cover the error path\"), to answer something it is " +
		"blocked on, or to give it the next piece of work when it has settled. The " +
		"message is queued and picked up on its next turn.\n\n" +
		"`agent` is the target's id, `message` the prompt it receives. `steps` " +
		"optionally runs reads, shell commands or pymodules at send time and appends " +
		"their output to `message` — use it to hand over a report file or a git " +
		"status instead of pasting it. The call returns one line per step (tool, " +
		"where, outcome, bytes), plus the output of any step with echo set."

	// agentSendStepsDescription is the `steps` property's description, verbatim
	// from the send-steps design: it is the contract the model authors steps
	// against, so it is pinned by TestAgentSendStepsSchemaShape rather than
	// left to drift.
	agentSendStepsDescription = "Optional tool calls the daemon runs when the message is sent, in order, " +
		"their output appended to the message so the agent starts with it. Each step is " +
		"exactly one of read, bash, pymodule_run. where: \"child\" (default) runs it in " +
		"the agent's own workspace; \"sender\" runs it in yours, under your tool " +
		"allowlist, and only the output reaches the agent — use that to hand over " +
		"content the agent cannot or should not fetch itself. echo: true also returns " +
		"the first 2 KiB of the output to you. Steps are for gathering context (reading " +
		"a report file, git status); never put state-changing or long-running commands " +
		"(test suites) in them — the send waits for them."

	agentKillDescription = "Stop an agent you spawned and everything it spawned in " +
		"turn. Returns once the shutdown is complete and recorded. Its unfinished " +
		"tasks are swept to `orphaned` with the assignee retained, so you can see " +
		"what it was holding — reassign them or drop them with a reason."

	agentSetBudgetDescription = "Change the USD cost budget of an agent you spawned " +
		"DIRECTLY (not a grandchild — ask the intermediate agent to change its own " +
		"child's budget instead). Raising it is capped by your own remaining budget " +
		"(asking for more than you have left — including asking for unlimited under a " +
		"budgeted parent — is refused). Lowering it is unrestricted. You cannot change " +
		"your own budget. Pass 0 for max_cost to make the target's budget unlimited."
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
		if p.Name == "message" || p.Name == "max_cost" {
			req = append(req, p.Name)
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
	return agentIDSchema(
		SchemaProperty{
			Name: "message", Type: "string",
			Description: "The prompt the agent receives. Step output, if any, is appended after it.",
		},
		SchemaProperty{
			Name: "steps", Type: "array",
			Description: agentSendStepsDescription,
			Items: &Schema{
				Type: "object",
				Properties: []SchemaProperty{
					{
						Name: "where", Type: "string",
						Enum: []string{"child", "sender"},
						Description: "Which workspace runs this step. \"child\" (default): the target agent's, " +
							"from its cwd. \"sender\": yours, from your cwd, gated by your own tool " +
							"allowlist — refused when you are not yourself a rafiki agent.",
					},
					{
						Name: "echo", Type: "boolean",
						Description: "Also return the first 2 KiB of this step's output to you. The agent " +
							"gets the output either way.",
					},
					{
						Name: "read", Type: "object",
						Description: "Read one file (not a glob) in the step's workspace.",
						Properties: []SchemaProperty{
							{Name: "path", Type: "string", Description: "Required. A relative path " +
								"resolves against the step's cwd (the agent's for where=child, yours " +
								"for where=sender)."},
							{Name: "start", Type: "integer", Description: "First line to include, " +
								"1-based. Omit to start at line 1."},
							{Name: "end", Type: "integer", Description: "Last line to include " +
								"(inclusive). Omit to read to the end."},
						},
					},
					{
						Name: "bash", Type: "object",
						Description: "Run one shell command in the step's workspace.",
						Properties: []SchemaProperty{
							{Name: "command", Type: "string", Description: "Required. Run by the " +
								"shell from the step's cwd (the agent's for where=child, yours for " +
								"where=sender); its output is what the agent receives."},
							{Name: "timeout_ms", Type: "integer", Description: "Kill the command " +
								"after this many milliseconds. Default 30000, max 60000."},
						},
					},
					{
						Name: "pymodule_run", Type: "object",
						Description: "Run a saved pymodule, with pymodule_run's inputs.",
						Properties: []SchemaProperty{
							{Name: "repo", Type: "string", Description: "Required. Pymodule source: " +
								"\"local\" or a registered git source's name."},
							{Name: "script", Type: "string", Description: "Required. Entry module, as saved."},
							{Name: "cwd", Type: "string", Description: "Working directory for the run. " +
								"Omit for the step's cwd; a relative path resolves against it."},
							{Name: "modules", Type: "array", Items: &Schema{Type: "string"},
								Description: "Further pymodules the script imports."},
							{Name: "args", Type: "array", Items: &Schema{Type: "string"},
								Description: "Extra command-line arguments."},
						},
					},
				},
			},
		},
	)
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
		Agent   string              `json:"agent"`
		Message string              `json:"message"`
		Steps   []protocol.SendStep `json:"steps"`
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
	// The ONE place a step's site is defaulted: protocol.StepSite's zero
	// value is refused, never guessed, so an author who leaves `where` out
	// means "child" — the target's own workspace — and says so exactly here.
	for i := range params.Steps {
		if params.Steps[i].Where == "" {
			params.Steps[i].Where = protocol.StepSiteChild
		}
	}
	res, err := t.agents.Send(ctx, SendSpec{
		ChildID: params.Agent,
		Message: params.Message,
		Steps:   params.Steps,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_send: %w", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "delivered to %s\n", params.Agent)
	for _, s := range res.Steps {
		fmt.Fprintf(&sb, "step %d %s (%s): %s, %d bytes", s.Index, s.Tool, s.Where, s.Outcome, s.Bytes)
		if s.Truncated {
			sb.WriteString(" (truncated)")
		}
		sb.WriteString("\n")
		if s.Echo != "" {
			for _, line := range strings.Split(strings.TrimSuffix(s.Echo, "\n"), "\n") {
				sb.WriteString("  ")
				sb.WriteString(line)
				sb.WriteString("\n")
			}
		}
	}
	return NewTextResult(sb.String()), nil
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

// --- agent_set_budget ---

type AgentSetBudgetBlueprint struct{}

func (AgentSetBudgetBlueprint) Name() string        { return "agent_set_budget" }
func (AgentSetBudgetBlueprint) Description() string { return agentSetBudgetDescription }
func (AgentSetBudgetBlueprint) InputSchema() Schema {
	return agentIDSchema(SchemaProperty{
		Name: "max_cost", Type: "number",
		Description: "New USD budget for the target agent's whole subtree. 0 means unlimited.",
	})
}

func (AgentSetBudgetBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentSetBudgetBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentSetBudgetTool{agents: opts.Agents}, nil
}

type agentSetBudgetTool struct {
	AgentSetBudgetBlueprint
	agents AgentSpawner
}

func (t *agentSetBudgetTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Agent   string  `json:"agent"`
		MaxCost float64 `json:"max_cost"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_set_budget: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if params.Agent == "" {
		return ToolResult{}, errors.New("agent_set_budget: agent is required; use agent_list to find the id")
	}
	if err := t.agents.SetBudget(ctx, params.Agent, params.MaxCost); err != nil {
		return ToolResult{}, fmt.Errorf("agent_set_budget: %w", err)
	}
	capStr := "unlimited"
	if params.MaxCost > 0 {
		capStr = fmt.Sprintf("$%.2f", params.MaxCost)
	}
	return NewTextResult(fmt.Sprintf("%s's budget is now %s\n", params.Agent, capStr)), nil
}
