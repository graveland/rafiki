package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
)

// workspaceOpts is the ToolOpts under which every workspace tool materializes,
// so a test can assert on the whole tier rather than on whichever tools happen
// to survive a bare ToolOpts.
func workspaceOpts(t *testing.T, exec ExecutorClient) ToolOpts {
	t.Helper()
	return ToolOpts{
		Cwd:         t.TempDir(),
		FileTracker: NewFileTracker(),
		LSP:         &fakeLSPClient{},
		Web:         true,
		Executor:    exec,
	}
}

// defsByName indexes a Registry's definitions, which is the only supported way
// to read a registered tool's schema back out.
func defsByName(r *Registry) map[string]msg.ToolDef {
	out := map[string]msg.ToolDef{}
	for _, def := range r.Definitions() {
		out[def.Name] = def
	}
	return out
}

// materializeOne builds exactly one blueprint, for tests that need the Tool
// rather than its definition.
func materializeOne(t *testing.T, opts ToolOpts, name string) Tool {
	t.Helper()
	for _, bp := range DefaultBlueprint.All() {
		if bp.Name() != name {
			continue
		}
		m, ok := bp.(Materializer)
		if !ok {
			return bp
		}
		tool, err := m.Materialize(opts)
		if err != nil {
			t.Fatalf("materialize %q: %v", name, err)
		}
		if tool == nil {
			t.Fatalf("%q declined to materialize under these opts", name)
		}
		return tool
	}
	t.Fatalf("no blueprint named %q", name)
	return nil
}

// parsedSchema is the subset of a tool's JSON Schema these tests read.
type parsedSchema struct {
	Properties map[string]struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"properties"`
	Required []string `json:"required"`
}

func parseSchema(t *testing.T, def msg.ToolDef) parsedSchema {
	t.Helper()
	var s parsedSchema
	if err := json.Unmarshal(def.InputSchema, &s); err != nil {
		t.Fatalf("%s: input schema is not valid JSON: %v\n%s", def.Name, err, def.InputSchema)
	}
	return s
}

// TestExecutorArgAddedToEveryWorkspaceTool pins the registry-layer promise:
// with an executor configured, every workspace tool the child actually gets
// carries the optional `executor` property, and no workspace tool requires it.
func TestExecutorArgAddedToEveryWorkspaceTool(t *testing.T) {
	c := assert.NewCollecting(t)
	defs := defsByName(DefaultBlueprint.MaterializeAll(workspaceOpts(t, stubExecutorClient{})))

	present := 0
	for _, name := range WorkspaceTools() {
		def, ok := defs[name]
		if !ok {
			// Declines under these opts.
			continue
		}
		present++
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
	}

	// A vacuous pass would hide the whole tier declining.
	c.True(present >= 10, "only %d workspace tools materialized; the assertion above proves nothing", present)
}

// TestExecutorArgAbsentOnDaemonTools keeps the argument off the tools that
// have no executor to name. agent_spawn is the interesting one: it has its own
// `executor` argument (where to run the CHILD), and a wrapper property on top
// would shadow it.
func TestExecutorArgAbsentOnDaemonTools(t *testing.T) {
	c := assert.NewCollecting(t)
	opts := workspaceOpts(t, stubExecutorClient{})
	opts.Agents = &fakeSpawner{}

	daemonNames := []string{"webfetch", "task_add", "agent_spawn"}
	wrapped := defsByName(DefaultBlueprint.MaterializeAll(opts))
	unwrapped := defsByName(DefaultBlueprint.MaterializeOnly(opts, daemonNames))

	for _, name := range daemonNames {
		w, wrappedOK := wrapped[name]
		u, unwrappedOK := unwrapped[name]
		if !wrappedOK || !unwrappedOK {
			c.Fail("%q did not materialize (%v wrapped, %v unwrapped)", name, wrappedOK, unwrappedOK)
			continue
		}
		c.EqDeep(u.InputSchema, w.InputSchema,
			"%q gained a property when an executor was configured; a daemon-tier tool runs on no executor", name)
	}

	// agent_spawn's own `executor` must be the ONLY one. A JSON object cannot
	// carry a duplicate key, so the duplicate a wrapper could introduce would
	// live in the builder's Properties slice — check there.
	count := 0
	for _, p := range materializeOne(t, opts, "agent_spawn").InputSchema().Properties {
		if p.Name == executorArgName {
			count++
		}
	}
	c.Eq(1, count, "agent_spawn's schema declares %q %d times; its own spawn argument must be the only one", executorArgName, count)
}

// targetProbeTool is an inner workspace tool. It records what the wrapper
// handed it and can fill in the ExecutorTarget the way an ExecutorClient would.
type targetProbeTool struct {
	name   string
	schema Schema

	calls     int
	gotInput  string
	gotTarget *ExecutorTarget

	fill func(*ExecutorTarget)
	res  ToolResult
	err  error
}

func (p *targetProbeTool) Name() string { return p.name }
func (p *targetProbeTool) Description() string {
	return "probe"
}
func (p *targetProbeTool) InputSchema() Schema { return p.schema }

func (p *targetProbeTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	p.calls++
	p.gotInput = string(input)
	p.gotTarget = ExecutorTargetFrom(ctx)
	if p.fill != nil && p.gotTarget != nil {
		p.fill(p.gotTarget)
	}
	return p.res, p.err
}

func newProbe() *targetProbeTool {
	return &targetProbeTool{
		name:   "read",
		schema: Schema{Type: "object", Properties: []SchemaProperty{{Name: "path", Type: "string"}}},
	}
}

// TestExecutorArgStrippedBeforeExecution pins the strip: the tool body — which
// may run on the executor — never sees the argument, and the request reaches
// it on the context instead.
func TestExecutorArgStrippedBeforeExecution(t *testing.T) {
	c := assert.NewAborting(t)
	probe := newProbe()
	probe.res = NewTextResult("out")

	res, err := (&executorTargeted{tool: probe}).Execute(
		context.Background(), ToolInput(`{"path":"x","executor":"ublk-s2"}`))

	c.NoError(err)
	c.Eq("out", res.Text, "result =")
	c.Eq(`{"path":"x"}`, probe.gotInput, "inner tool saw")
	c.NotNil(probe.gotTarget, "inner tool got no ExecutorTarget on the context")
	if probe.gotTarget != nil {
		c.Eq("ublk-s2", probe.gotTarget.Requested, "ExecutorTarget.Requested =")
	}
}

// TestExecutorArgNonStringRefused: a non-string executor is a malformed call,
// refused before the tool runs — never silently coerced or passed through.
func TestExecutorArgNonStringRefused(t *testing.T) {
	c := assert.NewAborting(t)
	probe := newProbe()

	_, err := (&executorTargeted{tool: probe}).Execute(
		context.Background(), ToolInput(`{"path":"x","executor":3}`))

	c.Error(err, "a non-string executor was accepted")
	if err != nil {
		c.Eq("executor must be a string (a machine name or executor id)", err.Error(), "error =")
	}
	c.Eq(0, probe.calls, "the inner tool ran on a refused call")
}

// TestExecutorArgOmittedPassesThrough: a call that does not use the argument
// reaches the inner tool byte-identical, and any non-object input is left
// alone rather than being rejected as malformed.
func TestExecutorArgOmittedPassesThrough(t *testing.T) {
	t.Run("object without the key", func(t *testing.T) {
		c := assert.NewAborting(t)
		probe := newProbe()
		probe.res = NewTextResult("out")

		_, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{"path":"x"}`))

		c.NoError(err)
		c.Eq(`{"path":"x"}`, probe.gotInput, "inner tool saw")
		c.NotNil(probe.gotTarget, "inner tool got no ExecutorTarget on the context")
		if probe.gotTarget != nil {
			c.Eq("", probe.gotTarget.Requested, "ExecutorTarget.Requested =")
		}
	})

	for _, input := range []string{"", `[]`, `"str"`, `null`, `not json`} {
		t.Run("passthrough "+input, func(t *testing.T) {
			c := assert.NewAborting(t)
			probe := newProbe()
			_, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(input))
			c.NoError(err)
			c.Eq(input, probe.gotInput, "inner tool saw")
		})
	}
}

// TestExecutorTargetPrefixesWhenMulti: when the agent could reach several
// executors, every result says which one served it — including a first-call
// note about the fresh binding.
func TestExecutorTargetPrefixesWhenMulti(t *testing.T) {
	t.Run("text result", func(t *testing.T) {
		c := assert.NewAborting(t)
		probe := newProbe()
		probe.res = NewTextResult("out")
		probe.fill = func(t *ExecutorTarget) { t.Resolved = "ublk-s2"; t.Multi = true }

		res, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{}`))
		c.NoError(err)
		c.Eq("[ublk-s2] out", res.Text, "result text =")
	})

	t.Run("with a note", func(t *testing.T) {
		c := assert.NewAborting(t)
		probe := newProbe()
		probe.res = NewTextResult("out")
		probe.fill = func(t *ExecutorTarget) {
			t.Resolved = "ublk-s2"
			t.Multi = true
			t.Note = "[ublk-s2] working from /srv"
		}

		res, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{}`))
		c.NoError(err)
		c.Eq("[ublk-s2] working from /srv\n[ublk-s2] out", res.Text, "result text =")
	})

	t.Run("blocks result", func(t *testing.T) {
		c := assert.NewAborting(t)
		probe := newProbe()
		probe.res = ToolResult{Blocks: []ContentBlock{
			ImageBlock{MediaType: "image/png", Data: []byte{1}},
			TextBlock{Text: "out"},
		}}
		probe.fill = func(t *ExecutorTarget) { t.Resolved = "ublk-s2"; t.Multi = true }

		res, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{}`))
		c.NoError(err)
		c.Len(res.Blocks, 3, "blocks =")
		first, ok := res.Blocks[0].(TextBlock)
		c.True(ok, "first block is %T, want a TextBlock carrying the prefix", res.Blocks[0])
		if ok {
			c.Eq("[ublk-s2] ", first.Text, "leading block text =")
		}
		c.Eq("", res.Text, "Text must stay empty when Blocks carry the result")
	})
}

// TestExecutorTargetUntouchedWhenSingle: a single reachable executor is the
// ordinary case, and its results must be exactly what the tool produced — no
// decoration, nothing to strip, no prompt-cache churn.
func TestExecutorTargetUntouchedWhenSingle(t *testing.T) {
	c := assert.NewAborting(t)

	probe := newProbe()
	probe.res = NewTextResult("out")
	probe.fill = func(t *ExecutorTarget) { t.Resolved = "ublk-s2" }

	res, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err)
	c.Eq("out", res.Text, "result text =")
	c.Len(res.Blocks, 0, "blocks =")

	withBlocks := newProbe()
	withBlocks.res = ToolResult{Blocks: []ContentBlock{TextBlock{Text: "out"}}}
	withBlocks.fill = func(t *ExecutorTarget) { t.Resolved = "ublk-s2" }

	res, err = (&executorTargeted{tool: withBlocks}).Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err)
	c.Len(res.Blocks, 1, "blocks =")
	if len(res.Blocks) == 1 {
		c.Eq("out", res.Blocks[0].(TextBlock).Text, "block text =")
	}
}

// TestExecutorTargetErrorPrefixedWhenMulti: an error from a call that could
// have gone anywhere must name where it went, and must stay the same error.
func TestExecutorTargetErrorPrefixedWhenMulti(t *testing.T) {
	inner := errors.New("no such file")

	t.Run("prefixed", func(t *testing.T) {
		c := assert.NewAborting(t)
		probe := newProbe()
		probe.err = inner
		probe.fill = func(t *ExecutorTarget) { t.Resolved = "ublk-s2"; t.Multi = true }

		_, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{}`))
		c.Error(err, "the inner error was swallowed")
		if err != nil {
			c.Eq("[ublk-s2] no such file", err.Error(), "error =")
			c.ErrorIs(err, inner, "errors.Is lost the inner error")
		}
	})

	t.Run("unresolved is left alone", func(t *testing.T) {
		c := assert.NewAborting(t)
		probe := newProbe()
		probe.err = inner
		probe.fill = func(t *ExecutorTarget) { t.Multi = true }

		_, err := (&executorTargeted{tool: probe}).Execute(context.Background(), ToolInput(`{}`))
		c.Error(err, "the inner error was swallowed")
		if err != nil {
			c.Eq("no such file", err.Error(), "an unresolved executor must not prefix with an empty name")
			c.ErrorIs(err, inner, "errors.Is lost the inner error")
		}
	})
}

// TestNoWorkspaceToolDeclaresExecutor: the wrapper OWNS the `executor`
// property, and deletes the key before the tool body runs. A blueprint that
// declared its own would have it silently stripped — a real argument shadowed
// by the wrapper.
func TestNoWorkspaceToolDeclaresExecutor(t *testing.T) {
	c := assert.NewCollecting(t)
	// A non-nil executor so the background-job verbs (which decline without
	// one) materialize too. materializeOne never wraps, so these are the
	// blueprints' own schemas.
	opts := workspaceOpts(t, stubExecutorClient{})

	present := 0
	for _, name := range WorkspaceTools() {
		tool := materializeOne(t, opts, name)
		for _, p := range tool.InputSchema().Properties {
			c.False(p.Name == executorArgName,
				"%q declares its own %q property; the registry wrapper strips that key, so the argument would never arrive",
				name, executorArgName)
		}
		present++
	}
	c.Eq(len(WorkspaceTools()), present, "workspace tools materialized")
}
