package tools

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	DefaultBlueprint.Register(&BashStartBlueprint{})
	DefaultBlueprint.Register(&BashOutputBlueprint{})
	DefaultBlueprint.Register(&BashKillBlueprint{})
}

const (
	bashStartDescription = "Start a long-running shell command in the background and " +
		"return a handle immediately. Use this for anything that outlives a single " +
		"tool call — dev servers, log tails, watch modes, test suites longer than ten " +
		"minutes — all of which plain `bash` cannot run, because it is synchronous " +
		"with a 600s ceiling. Poll with bash_output; stop it with bash_kill. The job " +
		"survives a dropped connection: it is not tied to this turn."

	bashOutputDescription = "Read what a background job has printed. Pass the handle " +
		"from bash_start. Reports whether the job is still running and, once it has " +
		"finished, its exit code. Safe to call repeatedly. A job that has printed more " +
		"than fits in one result is clipped to the most recent output, and the reply " +
		"names a file on the executor holding the fuller record — read or grep that " +
		"file rather than assuming the earlier output is gone."

	bashKillDescription = "Stop a background job and everything it spawned. Pass the " +
		"handle from bash_start. A job left running holds a process on the machine " +
		"after you are done with it; kill the ones you started."

	// maxJobOutputChars bounds what one bash_output call puts in the context.
	// The executor caps its own reply too, and keeps far more than either on
	// disk; handing all of it to the model on every poll is how a watch-mode
	// job eats a context window.
	maxJobOutputChars = 20_000
)

// --- bash_start ---

type BashStartBlueprint struct{}

func (BashStartBlueprint) Name() string        { return "bash_start" }
func (BashStartBlueprint) Description() string { return bashStartDescription }
func (BashStartBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "command", Type: "string", Description: "Shell command to run via bash -c in the background."},
		},
		Required: []string{"command"},
	}
}
func (BashStartBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

// Materialize declines when no executor is configured. A background job needs
// a process that outlives the turn, which is exactly what the executor is;
// in-process there is nothing to hand the work to, and a tool that can only
// answer "not configured" costs a turn to learn nothing.
func (BashStartBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Executor == nil {
		return nil, nil
	}
	return &bashStartTool{client: opts.Executor}, nil
}

type bashStartTool struct {
	BashStartBlueprint
	client ExecutorClient
}

func (t *bashStartTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Command string `json:"command"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("bash_start: invalid input: %w", err)
	}
	if params.Command == "" {
		return ToolResult{}, fmt.Errorf("bash_start: command is required")
	}
	handle, err := t.client.StartJob(ctx, params.Command)
	if err != nil {
		return NewErrorResult(fmt.Errorf("bash_start: %w", err)), nil
	}
	return NewTextResult(fmt.Sprintf(
		"Started background job %s\nPoll it with bash_output {\"handle\":%q}; stop it with bash_kill.",
		handle, handle)), nil
}

// --- bash_output ---

type BashOutputBlueprint struct{}

func (BashOutputBlueprint) Name() string        { return "bash_output" }
func (BashOutputBlueprint) Description() string { return bashOutputDescription }
func (BashOutputBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "handle", Type: "string", Description: "Job handle returned by bash_start."},
		},
		Required: []string{"handle"},
	}
}
func (BashOutputBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (BashOutputBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Executor == nil {
		return nil, nil
	}
	return &bashOutputTool{client: opts.Executor}, nil
}

type bashOutputTool struct {
	BashOutputBlueprint
	client ExecutorClient
}

func (t *bashOutputTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Handle string `json:"handle"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("bash_output: invalid input: %w", err)
	}
	if params.Handle == "" {
		return ToolResult{}, fmt.Errorf("bash_output: handle is required")
	}

	// since=0 every time: the model has no offset to carry and the executor
	// already bounds what it retains.
	snap, err := t.client.JobOutput(ctx, params.Handle, 0)
	if err != nil {
		return NewErrorResult(fmt.Errorf("bash_output: %w", err)), nil
	}
	if !snap.Found {
		// Deliberately not a time window: a finished job's output is kept until
		// its agent's workspace is released, and dropped early only when the
		// workspace exceeds its output budget. A wall-clock retention window
		// cannot know when an async agent will come back for its output.
		return NewTextResult(fmt.Sprintf(
			"no such job %q — it was never started, the handle is wrong, or its output was "+
				"released to stay within this workspace's output budget (oldest finished job first).",
			params.Handle)), nil
	}

	data := snap.Data
	if len(data) > maxJobOutputChars {
		data = "... [earlier output omitted] ...\n" + data[len(data)-maxJobOutputChars:]
	}

	var sb strings.Builder
	if snap.Exited {
		fmt.Fprintf(&sb, "Job %s finished with exit code %d.\n", params.Handle, snap.ExitCode)
	} else {
		fmt.Fprintf(&sb, "Job %s is still running.\n", params.Handle)
	}
	if data == "" {
		sb.WriteString("(no output yet)")
	} else {
		sb.WriteString(data)
	}
	return NewTextResult(sb.String()), nil
}

// --- bash_kill ---

type BashKillBlueprint struct{}

func (BashKillBlueprint) Name() string        { return "bash_kill" }
func (BashKillBlueprint) Description() string { return bashKillDescription }
func (BashKillBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "handle", Type: "string", Description: "Job handle returned by bash_start."},
		},
		Required: []string{"handle"},
	}
}
func (BashKillBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (BashKillBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Executor == nil {
		return nil, nil
	}
	return &bashKillTool{client: opts.Executor}, nil
}

type bashKillTool struct {
	BashKillBlueprint
	client ExecutorClient
}

func (t *bashKillTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Handle string `json:"handle"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("bash_kill: invalid input: %w", err)
	}
	if params.Handle == "" {
		return ToolResult{}, fmt.Errorf("bash_kill: handle is required")
	}
	if err := t.client.KillJob(ctx, params.Handle); err != nil {
		return NewErrorResult(fmt.Errorf("bash_kill: %w", err)), nil
	}
	return NewTextResult(fmt.Sprintf("Killed job %s and everything it spawned.", params.Handle)), nil
}
