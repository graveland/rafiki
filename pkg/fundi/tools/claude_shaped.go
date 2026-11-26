package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// The Claude-shaped workspace tools.
//
// A claude child running in rafiki workspace-tools mode keeps Claude Code's
// own tool names, because the model's prompt, its permission rules and its
// --disallowedTools list all speak in them — but runs them on its executor
// rather than on whichever host the Claude Code binary happens to sit on. The
// four blueprints here are argument TRANSLATIONS, not implementations: Read,
// Write and Edit forward their input unchanged to this package's read, write
// and edit (which already accept file_path, old_string/new_string/replace_all,
// and clamp Read's offset < 1 to 1), and Bash turns run_in_background into an
// executor job and drops `description`.
//
// They materialize only under ToolOpts.ClaudeShapedWorkspace, which ALSO makes
// the native read/write/edit/bash decline: two spellings of one operation
// leave the model picking by guesswork.
const (
	claudeReadDescription = "Reads a file on an executor. Replaces Claude Code's " +
		"built-in Read: same arguments, plus executor. Output is cat -n style " +
		"(line number, tab, text); strip that prefix before quoting text in Edit."

	claudeWriteDescription = "Writes a file on an executor. Replaces the built-in " +
		"Write; read an existing file first."

	claudeEditDescription = "Edits a file on an executor by exact string " +
		"replacement. Replaces the built-in Edit; read the file first."

	claudeBashDescription = "Runs a shell command on an executor. Replaces the " +
		"built-in Bash. Run long commands (builds, test suites, deploys) directly: " +
		"one still running after 60s is moved to the background as a job, and you " +
		"get its output so far, a handle, and a message when it exits. Then end " +
		"your turn or do other work and read the message when it arrives. Waiting " +
		"is event-driven: never sleep, loop on bash_output, or schedule a wakeup " +
		"to wait for a command, because the exit message is immediate and any " +
		"polling is strictly slower. timeout is a kill deadline for the whole " +
		"command (default 1 hour), not a wait. run_in_background starts a job with " +
		"no deadline at once: read its output with bash_output and stop it with " +
		"bash_kill, naming the same executor."
)

func init() {
	DefaultBlueprint.Register(&ClaudeReadBlueprint{})
	DefaultBlueprint.Register(&ClaudeWriteBlueprint{})
	DefaultBlueprint.Register(&ClaudeEditBlueprint{})
	DefaultBlueprint.Register(&ClaudeBashBlueprint{})
}

// servesClaudeShaped reports whether these opts ask for the Claude-shaped
// four.
//
// The Executor check is not merely "can this run": these tools are translators
// that call opts.Executor themselves, so with no executor they have nothing to
// call — and registering them anyway would run the call in the daemon's own
// process, the one outcome the executor architecture exists to prevent.
// MaterializeAll drops a workspace-tier tool without an executor too; this is
// the same rule stated where the tools are built, so a caller that materializes
// one of these blueprints directly cannot bypass it.
func servesClaudeShaped(opts ToolOpts) bool {
	return opts.ClaudeShapedWorkspace && opts.Executor != nil
}

// claudeForwarder is Read, Write or Edit: one of Claude Code's argument
// shapes, forwarded byte-for-byte to the native tool of the same effect on the
// executor.
type claudeForwarder struct {
	// bp supplies this tool's name, description and schema — the blueprint's
	// own metadata, never a copy that could drift from it.
	bp     Tool
	client ExecutorClient
	target string
}

func (t *claudeForwarder) Name() string        { return t.bp.Name() }
func (t *claudeForwarder) Description() string { return t.bp.Description() }
func (t *claudeForwarder) InputSchema() Schema { return t.bp.InputSchema() }

func (t *claudeForwarder) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	return forwardToExecutor(ctx, t.client, t.target, input)
}

// forwardToExecutor hands a call straight to the executor's native tool,
// keeping the input byte for byte: the native tools already take these
// argument names, so a re-encode would only risk changing them.
func forwardToExecutor(ctx context.Context, client ExecutorClient, tool string, input ToolInput) (ToolResult, error) {
	res, err := client.Execute(ctx, tool, json.RawMessage(input))
	if err != nil {
		// Return the ERROR, not the error's text as a successful result —
		// executorProxy's rule, and the only way the model learns its call
		// failed rather than reading the failure as output.
		return ToolResult{}, err
	}
	return toolResultFromMeta(res), nil
}

// ClaudeReadBlueprint is Claude Code's Read: the executor's read, with
// Claude Code's argument names.
type ClaudeReadBlueprint struct{}

func (ClaudeReadBlueprint) Name() string        { return "Read" }
func (ClaudeReadBlueprint) Description() string { return claudeReadDescription }
func (ClaudeReadBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "file_path", Type: "string", Description: "Path to the file to read, absolute or relative to the executor's working directory."},
			{Name: "offset", Type: "integer", Description: "1-indexed line number to start reading from. Defaults to 1."},
			{Name: "limit", Type: "integer", Description: "Maximum number of lines to return. Defaults to 2000."},
		},
		Required: []string{"file_path"},
	}
}
func (ClaudeReadBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (ClaudeReadBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if !servesClaudeShaped(opts) {
		return nil, nil
	}
	return &claudeForwarder{bp: ClaudeReadBlueprint{}, client: opts.Executor, target: "read"}, nil
}

// ClaudeWriteBlueprint is Claude Code's Write: the executor's write.
type ClaudeWriteBlueprint struct{}

func (ClaudeWriteBlueprint) Name() string        { return "Write" }
func (ClaudeWriteBlueprint) Description() string { return claudeWriteDescription }
func (ClaudeWriteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "file_path", Type: "string", Description: "Path to the file to write, absolute or relative to the executor's working directory."},
			{Name: "content", Type: "string", Description: "Full content to write to the file."},
		},
		Required: []string{"file_path", "content"},
	}
}
func (ClaudeWriteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (ClaudeWriteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if !servesClaudeShaped(opts) {
		return nil, nil
	}
	return &claudeForwarder{bp: ClaudeWriteBlueprint{}, client: opts.Executor, target: "write"}, nil
}

// ClaudeEditBlueprint is Claude Code's Edit: the executor's edit, one exact
// replacement.
type ClaudeEditBlueprint struct{}

func (ClaudeEditBlueprint) Name() string        { return "Edit" }
func (ClaudeEditBlueprint) Description() string { return claudeEditDescription }
func (ClaudeEditBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "file_path", Type: "string", Description: "Path to the file to edit, absolute or relative to the executor's working directory."},
			{Name: "old_string", Type: "string", Description: "Exact text to replace. Must match exactly once unless replace_all is true."},
			{Name: "new_string", Type: "string", Description: "Text to replace old_string with."},
			{Name: "replace_all", Type: "boolean", Description: "Replace every occurrence of old_string instead of requiring exactly one match."},
		},
		Required: []string{"file_path", "old_string", "new_string"},
	}
}
func (ClaudeEditBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (ClaudeEditBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if !servesClaudeShaped(opts) {
		return nil, nil
	}
	return &claudeForwarder{bp: ClaudeEditBlueprint{}, client: opts.Executor, target: "edit"}, nil
}

// ClaudeBashBlueprint is Claude Code's Bash: the executor's bash, plus the two
// arguments that mean something else on this side of the boundary.
type ClaudeBashBlueprint struct{}

func (ClaudeBashBlueprint) Name() string        { return "Bash" }
func (ClaudeBashBlueprint) Description() string { return claudeBashDescription }
func (ClaudeBashBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "command", Type: "string", Description: "Shell command to run via bash -c on the executor."},
			{Name: "timeout", Type: "number", Description: bashTimeoutDescription},
			{Name: "description", Type: "string", Description: "A one-line description of what the command does. Not forwarded to the executor."},
			{Name: "run_in_background", Type: "boolean", Description: "Start the command as a job instead of waiting for it to finish."},
		},
		Required: []string{"command"},
	}
}
func (ClaudeBashBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (ClaudeBashBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if !servesClaudeShaped(opts) {
		return nil, nil
	}
	return &claudeBashTool{client: opts.Executor}, nil
}

type claudeBashTool struct {
	ClaudeBashBlueprint
	client ExecutorClient
}

// claudeBashInput is Claude Code's Bash argument shape. Timeout is a NUMBER
// (milliseconds) and is a pointer so "absent" stays distinguishable from 0:
// the executor's bash treats a non-positive timeout as "use the default",
// while omitting the field entirely is what Claude Code means by absent.
type claudeBashInput struct {
	Command         string   `json:"command"`
	Timeout         *float64 `json:"timeout"`
	Description     string   `json:"description"`
	RunInBackground bool     `json:"run_in_background"`
}

func (t *claudeBashTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in claudeBashInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("bash: invalid input: %w", err)
	}

	// run_in_background starts a JOB on the executor. The command's own
	// backgrounding (&, disown) is not this: Claude Code's flag means "hand it
	// to the job registry and give me a handle now", which is what
	// bash_output/bash_kill operate on.
	if in.RunInBackground {
		handle, err := t.client.StartJob(ctx, in.Command)
		if err != nil {
			return ToolResult{}, err
		}
		return NewTextResult(fmt.Sprintf(
			"started background job %s; read it with bash_output {\"handle\":%q}", handle, handle)), nil
	}

	payload := map[string]any{"command": in.Command}
	if in.Timeout != nil && *in.Timeout > 0 {
		payload["timeout"] = *in.Timeout
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ToolResult{}, fmt.Errorf("bash: invalid input: %w", err)
	}
	// `description` is dropped: the executor's bash has no such argument, and
	// Claude Code's is prose for the transcript rather than an instruction.
	return forwardToExecutor(ctx, t.client, "bash", ToolInput(body))
}
