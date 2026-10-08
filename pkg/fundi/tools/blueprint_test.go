package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/toolmeta"
)

// testEchoTool is a fully configurable Tool for blueprint and registry tests.
type testEchoTool struct {
	name   string
	desc   string
	schema Schema
	result string
}

func (t *testEchoTool) Name() string        { return t.name }
func (t *testEchoTool) Description() string { return t.desc }
func (t *testEchoTool) InputSchema() Schema { return t.schema }
func (t *testEchoTool) Execute(_ context.Context, in ToolInput) (ToolResult, error) {
	var m map[string]any
	if err := in.Unmarshal(&m); err != nil {
		return NewErrorResult(err), err
	}
	return NewTextResult(t.result), nil
}

// matTestBlueprint is a Materializer test helper.
type matTestBlueprint struct {
	testEchoTool
}

func (matTestBlueprint) Name() string { return "mat" }
func (m *matTestBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &testEchoTool{name: "mat", result: "materialized with cwd=" + opts.Cwd}, nil
}

// decliningTestBlueprint is a Materializer that opts out for empty opts, the
// way SkillBlueprint does when there are no skills to load.
type decliningTestBlueprint struct {
	testEchoTool
}

func (decliningTestBlueprint) Name() string { return "declines" }
func (d *decliningTestBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Cwd == "" {
		return nil, nil
	}
	return &testEchoTool{name: "declines", result: "materialized"}, nil
}

func TestBlueprintRegistryRegisterAndAll(t *testing.T) {
	br := &BlueprintRegistry{}
	t1 := &testEchoTool{name: "echo", desc: "echoes", schema: Schema{Type: "object"}}
	br.Register(t1)
	all := br.All()
	assert.NewAborting(t).False(len(all) != 1 || all[0].Name() != "echo", "expected [echo], got %v", all)
}

func TestBlueprintRegistryDuplicatePanic(t *testing.T) {
	br := &BlueprintRegistry{}
	br.Register(&testEchoTool{name: "echo"})
	defer func() {
		assert.NewAborting(t).NotNil(recover(), "expected panic for duplicate registration")
	}()
	br.Register(&testEchoTool{name: "echo"})
}

func TestBuildDefRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	tool := &testEchoTool{
		name: "echo",
		desc: "echoes a thing",
		schema: Schema{
			Type: "object",
			Properties: []SchemaProperty{
				{Name: "path", Type: "string", Description: "the file path"},
			},
			Required: []string{"path"},
		},
	}

	def := BuildDef(tool)
	c.NotNil(def.OfTool, "expected OfTool variant")
	c.Eq("echo", def.OfTool.Name, "name =")
	if !def.OfTool.Description.Valid() || def.OfTool.Description.Value != "echoes a thing" {
		t.Fatalf("description = %+v", def.OfTool.Description)
	}
	if len(def.OfTool.InputSchema.Required) != 1 || def.OfTool.InputSchema.Required[0] != "path" {
		t.Fatalf("required = %v", def.OfTool.InputSchema.Required)
	}

	// Round-trip through the SDK's own serialization.
	b, err := json.Marshal(def)
	c.NoError(err, "marshal def")
	var back anthropic.ToolUnionParam
	c.NoError(json.Unmarshal(b, &back), "unmarshal def")
	c.False(back.OfTool == nil || back.OfTool.Name != "echo", "round-trip lost name")
}

func TestMaterializeAllStatelessTools(t *testing.T) {
	c := assert.NewAborting(t)
	br := &BlueprintRegistry{}
	br.Register(&testEchoTool{name: "a", result: "a ok"})
	br.Register(&testEchoTool{name: "b", result: "b ok"})

	r := br.MaterializeAll(ToolOpts{})
	defs := r.Definitions()
	c.Len(defs, 2, "expected 2 definitions, got %d", len(defs))
	if defs[0].OfTool.Name != "a" || defs[1].OfTool.Name != "b" {
		t.Fatalf("expected [a, b], got %v", []string{defs[0].OfTool.Name, defs[1].OfTool.Name})
	}

	outRes, err := r.Execute(context.Background(), "a", json.RawMessage(`{}`))
	out := outRes.Text
	c.False(err != nil || out != "a ok", "Execute(a) = (%q, %v)", out, err)
}

// TestMaterializeAllSkipsDecliningMaterializer pins the (nil, nil) contract:
// a blueprint that declines is absent from the Registry entirely rather than
// registered as a nil Tool (which would panic on the next Definitions call).
func TestMaterializeAllSkipsDecliningMaterializer(t *testing.T) {
	c := assert.NewAborting(t)
	br := &BlueprintRegistry{}
	br.Register(&decliningTestBlueprint{})
	br.Register(&testEchoTool{name: "kept", result: "kept ok"})

	declined := br.MaterializeAll(ToolOpts{})
	if got := len(declined.Definitions()); got != 1 {
		t.Fatalf("declining blueprint: got %d definitions, want 1 (only \"kept\")", got)
	}
	_, err := declined.Execute(context.Background(), "declines", json.RawMessage(`{}`))
	c.Error(err, "declined tool is still executable")

	// Same registry, opts that satisfy it: the tool comes back.
	accepted := br.MaterializeAll(ToolOpts{Cwd: "/tmp"})
	c.Eq(2, len(accepted.Definitions()), "satisfied blueprint: got")
}

func TestMaterializeAllWithMaterializer(t *testing.T) {
	br := &BlueprintRegistry{}
	br.Register(&matTestBlueprint{})
	r := br.MaterializeAll(ToolOpts{Cwd: "/tmp"})

	outRes, err := r.Execute(context.Background(), "mat", json.RawMessage(`{}`))
	out := outRes.Text
	assert.NewAborting(t).False(err != nil || out != "materialized with cwd=/tmp", "Execute(mat) = (%q, %v)", out, err)
}

// webAwareBlueprint records whether Materialize received Web=true.
type webAwareBlueprint struct {
	testEchoTool
	gotWeb *bool
}

func (b *webAwareBlueprint) Name() string { return "web-aware" }
func (b *webAwareBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	*b.gotWeb = opts.Web
	return &testEchoTool{name: "web-aware", result: "ok"}, nil
}

func TestToolOptsWebFlowsToMaterialize(t *testing.T) {
	c := assert.NewAborting(t)
	var gotWeb bool
	br := &BlueprintRegistry{}
	br.Register(&webAwareBlueprint{gotWeb: &gotWeb})

	br.MaterializeAll(ToolOpts{Web: true})
	c.True(gotWeb, "Materialize received Web=false when ToolOpts.Web was true")

	gotWeb = false
	br.MaterializeAll(ToolOpts{})
	c.False(gotWeb, "Materialize received Web=true when ToolOpts.Web was false (zero value)")
}

// A routed tool whose executor call fails must FAIL, not return the failure
// text as a successful result. agentloop computes is_error as `err != nil`, so
// swallowing the error reported every executor-routed tool failure — read,
// write, edit, glob, grep, ls, bash, every lsp_* — as a success carrying the
// diagnostic as output. The TUI drew a ✓ on it, and the model was told the
// same. An unbindable executor is the common way in: nothing ran at all.
func TestRoutedToolPropagatesAnExecutorFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	proxy := &executorProxy{
		tool:   readOnlyProbeTool{},
		client: failingExecClient{err: errors.New(`spawn refused: no executor satisfies "machine=greyshift"`)},
	}

	res, err := proxy.Execute(context.Background(), ToolInput(`{}`))
	c.Require().Error(err, "an executor failure returned success with result %q; "+
		"agentloop marks is_error from the error, so this renders as a ✓ "+
		"and tells the model its call worked", res.Text)
	c.StrContains(err.Error(), "no executor satisfies", "error = %v, want the executor's own diagnostic", err)
}

// failingExecClient embeds the existing stub so only Execute has to differ.
type failingExecClient struct {
	stubExecutorClient
	err error
}

func (f failingExecClient) Execute(context.Context, string, json.RawMessage) (toolmeta.Result, error) {
	return toolmeta.Result{}, f.err
}

type readOnlyProbeTool struct{}

func (readOnlyProbeTool) Name() string        { return "bash" }
func (readOnlyProbeTool) Description() string { return "probe" }
func (readOnlyProbeTool) InputSchema() Schema { return Schema{Type: "object"} }
func (readOnlyProbeTool) Execute(context.Context, ToolInput) (ToolResult, error) {
	return NewTextResult("unused"), nil
}
