package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
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

func TestBlueprintRegistryRegisterAndAll(t *testing.T) {
	br := &BlueprintRegistry{}
	t1 := &testEchoTool{name: "echo", desc: "echoes", schema: Schema{Type: "object"}}
	br.Register(t1)
	all := br.All()
	if len(all) != 1 || all[0].Name() != "echo" {
		t.Fatalf("expected [echo], got %v", all)
	}
}

func TestBlueprintRegistryDuplicatePanic(t *testing.T) {
	br := &BlueprintRegistry{}
	br.Register(&testEchoTool{name: "echo"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate registration")
		}
	}()
	br.Register(&testEchoTool{name: "echo"})
}

func TestBuildDefRoundTrip(t *testing.T) {
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
	if def.OfTool == nil {
		t.Fatal("expected OfTool variant")
	}
	if def.OfTool.Name != "echo" {
		t.Fatalf("name = %q", def.OfTool.Name)
	}
	if !def.OfTool.Description.Valid() || def.OfTool.Description.Value != "echoes a thing" {
		t.Fatalf("description = %+v", def.OfTool.Description)
	}
	if len(def.OfTool.InputSchema.Required) != 1 || def.OfTool.InputSchema.Required[0] != "path" {
		t.Fatalf("required = %v", def.OfTool.InputSchema.Required)
	}

	// Round-trip through the SDK's own serialization.
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal def: %v", err)
	}
	var back anthropic.ToolUnionParam
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal def: %v", err)
	}
	if back.OfTool == nil || back.OfTool.Name != "echo" {
		t.Fatal("round-trip lost name")
	}
}

func TestMaterializeAllStatelessTools(t *testing.T) {
	br := &BlueprintRegistry{}
	br.Register(&testEchoTool{name: "a", result: "a ok"})
	br.Register(&testEchoTool{name: "b", result: "b ok"})

	r := br.MaterializeAll(ToolOpts{})
	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	if defs[0].OfTool.Name != "a" || defs[1].OfTool.Name != "b" {
		t.Fatalf("expected [a, b], got %v", []string{defs[0].OfTool.Name, defs[1].OfTool.Name})
	}

	out, err := r.Execute(context.Background(), "a", json.RawMessage(`{}`))
	if err != nil || out != "a ok" {
		t.Fatalf("Execute(a) = (%q, %v)", out, err)
	}
}

func TestMaterializeAllWithMaterializer(t *testing.T) {
	br := &BlueprintRegistry{}
	br.Register(&matTestBlueprint{})
	r := br.MaterializeAll(ToolOpts{Cwd: "/tmp"})

	out, err := r.Execute(context.Background(), "mat", json.RawMessage(`{}`))
	if err != nil || out != "materialized with cwd=/tmp" {
		t.Fatalf("Execute(mat) = (%q, %v)", out, err)
	}
}
