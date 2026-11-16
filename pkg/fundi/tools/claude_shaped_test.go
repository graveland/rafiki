package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/toolmeta"
)

// nativeWorkspaceNames are the four tools a claude child in workspace-tools
// mode must NOT be offered.
var nativeWorkspaceNames = []string{"read", "write", "edit", "bash"}

// claudeShapedNames are the workspace-tier blueprints that materialize only in
// claude workspace-tools mode. Tests that walk the whole tier skip them by
// name and assert their absence, so a wholesale decline still can't hide.
var claudeShapedNames = []string{"Read", "Write", "Edit", "Bash"}

// claudeShapedOpts is the ToolOpts of a claude child in rafiki workspace-tools
// mode.
func claudeShapedOpts(t *testing.T, exec ExecutorClient) ToolOpts {
	t.Helper()
	opts := workspaceOpts(t, exec)
	opts.ClaudeShapedWorkspace = true
	return opts
}

// recordingExecutor records what it was handed and answers with a fixed
// result. It is the probe for "the argument translation reaches the executor
// exactly as intended".
type recordingExecutor struct {
	stubExecutorClient

	execTool  string
	execInput string
	execCalls int
	execErr   error
	meta      toolmeta.Result

	jobCommand string
	jobHandle  string
	jobErr     error
}

func (r *recordingExecutor) Execute(_ context.Context, tool string, input json.RawMessage) (toolmeta.Result, error) {
	r.execTool = tool
	r.execInput = string(input)
	r.execCalls++
	return r.meta, r.execErr
}

func (r *recordingExecutor) StartJob(_ context.Context, command string) (string, error) {
	r.jobCommand = command
	return r.jobHandle, r.jobErr
}

// The mode is a swap, not an addition: with it off the child sees the native
// four and none of the Claude-shaped ones, with it on the reverse. With the
// mode set and no executor it must see NEITHER — the translators have nothing
// to translate to, and registering them anyway would run the call in the
// daemon's own process.
func TestClaudeShapedServedOnlyInMode(t *testing.T) {
	c := assert.NewCollecting(t)

	off := registryNames(DefaultBlueprint.MaterializeAll(workspaceOpts(t, stubExecutorClient{})))
	for _, name := range nativeWorkspaceNames {
		c.True(off[name], "%q is missing outside claude workspace-tools mode", name)
	}
	for _, name := range claudeShapedNames {
		c.False(off[name], "%q was served outside claude workspace-tools mode", name)
	}

	on := registryNames(DefaultBlueprint.MaterializeAll(claudeShapedOpts(t, stubExecutorClient{})))
	for _, name := range nativeWorkspaceNames {
		c.False(on[name], "%q is still served in claude workspace-tools mode; the model would have two spellings of one operation", name)
	}
	for _, name := range claudeShapedNames {
		c.True(on[name], "%q is missing in claude workspace-tools mode", name)
	}

	noExec := registryNames(DefaultBlueprint.MaterializeAll(ToolOpts{
		Cwd:                   t.TempDir(),
		FileTracker:           NewFileTracker(),
		ClaudeShapedWorkspace: true,
	}))
	for _, name := range nativeWorkspaceNames {
		c.False(noExec[name], "%q was served with no executor", name)
	}
	for _, name := range claudeShapedNames {
		c.False(noExec[name], "%q materialized with no executor; it would call the executor from the daemon process with nothing bound", name)
	}
}

// Read, Write and Edit are argument translations only: the native tools already
// take file_path, old_string/new_string/replace_all, and read clamps offset < 1
// itself — so the input must reach the executor byte for byte.
func TestClaudeShapedReadForwardsInputUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingExecutor{meta: toolmeta.Result{Text: "     1\talpha"}}
	read := materializeOne(t, claudeShapedOpts(t, rec), "Read")

	const input = `{"file_path":"/w/a.txt","offset":3,"limit":7}`
	res, err := read.Execute(context.Background(), ToolInput(input))
	c.NoError(err, "Execute(Read)")
	c.Eq("read", rec.execTool, "Read forwarded to tool")
	c.Eq(input, rec.execInput, "Read rewrote its input on the way to the executor; got")
	c.Eq("     1\talpha", res.Text, "Read result text")

	// A failing executor call is an ERROR, never a success carrying its text.
	rec.execErr = errors.New("no such file")
	_, err = read.Execute(context.Background(), ToolInput(input))
	c.Error(err, "Read reported the executor's error as a successful result")
}

func TestClaudeShapedWriteForwardsInputUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingExecutor{meta: toolmeta.Result{Text: "wrote 6 bytes to /w/a.txt"}}
	write := materializeOne(t, claudeShapedOpts(t, rec), "Write")

	const input = `{"file_path":"/w/a.txt","content":"hello\n"}`
	res, err := write.Execute(context.Background(), ToolInput(input))
	c.NoError(err, "Execute(Write)")
	c.Eq("write", rec.execTool, "Write forwarded to tool")
	c.Eq(input, rec.execInput, "Write rewrote its input on the way to the executor; got")
	c.Eq("wrote 6 bytes to /w/a.txt", res.Text, "Write result text")
}

func TestClaudeShapedEditForwardsInputUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingExecutor{meta: toolmeta.Result{Text: "replaced 2 occurrence(s) in /w/a.txt"}}
	edit := materializeOne(t, claudeShapedOpts(t, rec), "Edit")

	const input = `{"file_path":"/w/a.txt","old_string":"alpha","new_string":"beta","replace_all":true}`
	res, err := edit.Execute(context.Background(), ToolInput(input))
	c.NoError(err, "Execute(Edit)")
	c.Eq("edit", rec.execTool, "Edit forwarded to tool")
	c.Eq(input, rec.execInput, "Edit rewrote its input on the way to the executor; got")
	c.Eq("replaced 2 occurrence(s) in /w/a.txt", res.Text, "Edit result text")
}

// Bash is the one that translates: Claude Code's `timeout` (a number of
// milliseconds) becomes the executor's timeout_ms, and `description` is dropped
// because the executor's bash has no such argument.
func TestClaudeShapedBashTranslatesTimeout(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{
			"timeout becomes timeout_ms",
			`{"command":"x","timeout":5000,"description":"d"}`,
			`{"command":"x","timeout_ms":5000}`,
		},
		{"absent timeout is omitted", `{"command":"x"}`, `{"command":"x"}`},
		{"zero timeout is omitted", `{"command":"x","timeout":0}`, `{"command":"x"}`},
		{"negative timeout is omitted", `{"command":"x","timeout":-5}`, `{"command":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			rec := &recordingExecutor{meta: toolmeta.Result{Text: "ok"}}
			bash := materializeOne(t, claudeShapedOpts(t, rec), "Bash")

			res, err := bash.Execute(context.Background(), ToolInput(tc.input))
			c.NoError(err, "Execute(Bash)")
			c.Eq("bash", rec.execTool, "Bash forwarded to tool")
			c.Eq(tc.want, rec.execInput, "Bash payload; got")
			c.Eq("ok", res.Text, "Bash result text")
		})
	}
}

// run_in_background means "hand it to the job registry and give me a handle
// now", which is what bash_output/bash_kill operate on — not the command's own
// backgrounding, and emphatically not a foreground wait.
func TestClaudeShapedBashBackgroundStartsJob(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &recordingExecutor{jobHandle: "j-7"}
	bash := materializeOne(t, claudeShapedOpts(t, rec), "Bash")

	res, err := bash.Execute(context.Background(), ToolInput(
		`{"command":"sleep 30","run_in_background":true,"timeout":1000,"description":"wait"}`))
	c.NoError(err, "Execute(Bash)")
	c.Eq("sleep 30", rec.jobCommand, "job command")
	c.Eq(0, rec.execCalls, "a background Bash ran the command in the foreground as well")
	c.Eq(`started background job j-7; read it with bash_output {"handle":"j-7"}`,
		res.Text, "handoff text")
}

// Each of the four is a workspace tool, so the registry's own wrapper adds the
// optional `executor` argument to it — and the blueprint must not declare its
// own, since the wrapper strips that key before the tool body runs.
func TestClaudeShapedGetExecutorArgument(t *testing.T) {
	c := assert.NewCollecting(t)
	opts := claudeShapedOpts(t, stubExecutorClient{})
	defs := defsByName(DefaultBlueprint.MaterializeAll(opts))

	for _, name := range claudeShapedNames {
		def, ok := defs[name]
		if !ok {
			c.Fail("%q did not materialize in claude workspace-tools mode", name)
			continue
		}
		s := parseSchema(t, def)
		prop, has := s.Properties[executorArgName]
		c.True(has, "%q has no %q property; a call to it can name no executor", name, executorArgName)
		if has {
			c.Eq("string", prop.Type, "%q: properties.%s.type", name, executorArgName)
			c.Eq(ExecutorArgDescription, prop.Description, "%q: properties.%s.description", name, executorArgName)
		}
		for _, req := range s.Required {
			c.False(req == executorArgName,
				"%q requires %q; it is optional whenever one executor is reachable", name, executorArgName)
		}

		for _, p := range materializeOne(t, opts, name).InputSchema().Properties {
			c.False(p.Name == executorArgName,
				"%q declares its own %q property; the registry wrapper strips that key, so the argument would never arrive",
				name, executorArgName)
		}
	}
}

// These four are daemon-side translators: the model's `Bash` reaches the
// executor through `bash`, and no executor registry contains a tool named
// "Bash". Listing them in ExecutorLocalTools would proxy the call to a
// registry that can only answer "unknown tool".
func TestClaudeShapedNotExecutorLocal(t *testing.T) {
	c := assert.NewCollecting(t)
	local := ExecutorLocalTools()
	for _, name := range claudeShapedNames {
		c.NotContains(local, name,
			"%q reached ExecutorLocalTools; the executor's own registry has no such tool, so a proxied call could only fail", name)
	}
}
