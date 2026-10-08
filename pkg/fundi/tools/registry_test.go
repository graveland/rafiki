package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/multigres/testkit/assert"
)

func TestToolResultContentBlocksFromText(t *testing.T) {
	c := assert.NewAborting(t)
	r := NewTextResult("hello")
	blocks := r.ContentBlocks()
	c.Len(blocks, 1, "got %d blocks, want 1", len(blocks))
	tb, ok := blocks[0].(TextBlock)
	if !ok {
		t.Fatalf("got block type %T, want TextBlock", blocks[0])
	}
	c.Eq("hello", tb.Text, "got")
}

func TestToolResultContentBlocksEmptyTextYieldsNoBlocks(t *testing.T) {
	assert.NewAborting(t).Eq(0, len(ToolResult{}.ContentBlocks()), "got")
}

func TestToolResultContentBlocksPrefersExplicitBlocks(t *testing.T) {
	c := assert.NewAborting(t)
	r := ToolResult{Text: "ignored", Blocks: []ContentBlock{TextBlock{Text: "explicit"}}}
	blocks := r.ContentBlocks()
	c.Len(blocks, 1, "got %d blocks, want 1", len(blocks))
	c.Eq("explicit", blocks[0].(TextBlock).Text, "Blocks did not take precedence over Text")
}

func TestParseRTKMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want RTKMode
	}{
		{"", RTKAuto},
		{"auto", RTKAuto},
		{"on", RTKOn},
		{"off", RTKOff},
		{"OFF", RTKOff},
		{"nonsense", RTKAuto},
	} {
		got := ParseRTKMode(tc.in)
		assert.NewAborting(t).Eq(tc.want, got, "ParseRTKMode(%q) = %q, want", tc.in, got)
	}
}

func toolNames(defs []anthropic.ToolUnionParam) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		if d.OfTool != nil {
			names[i] = d.OfTool.Name
		}
	}
	return names
}

func TestEditRequiresPriorRead(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	c.NoError(os.WriteFile(p, []byte("hello world"), 0o644))
	r := DefaultBlueprint.MaterializeOnly(ToolOpts{FileTracker: NewFileTracker(), Cwd: dir},
		[]string{"read", "edit"})
	_, err := r.Execute(context.Background(), "edit",
		json.RawMessage(`{"path":"`+p+`","old_string":"hello","new_string":"bye"}`))
	c.False(err == nil || !strings.Contains(err.Error(), "read"), "expected read-before-edit error, got %v", err)
	if _, err := r.Execute(context.Background(), "read", json.RawMessage(`{"path":"`+p+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), "edit",
		json.RawMessage(`{"path":"`+p+`","old_string":"hello","new_string":"bye"}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	c.NoError(err)
	c.Eq("bye world", string(b), "edit result: %s", b)
}

func TestDefinitionsSortedByName(t *testing.T) {
	c := assert.NewAborting(t)
	r := DefaultBlueprint.MaterializeAll(ToolOpts{FileTracker: NewFileTracker(), Cwd: t.TempDir(), Executor: stubExecutorClient{}})
	defs := r.Definitions()
	names := toolNames(defs)
	c.True(sort.StringsAreSorted(names), "not sorted: %v", names)
	c.GreaterOrEqual(5, len(names), "expected at least 5 registered file tools, got %d: %v", len(names), names)
}

func TestRegisterAndExecute(t *testing.T) {
	c := assert.NewAborting(t)
	r := NewRegistry()
	r.Register(&testEchoTool{name: "echo", desc: "echoes its input", schema: schemaWithRequiredX(), result: "got:hi"})
	outRes, err := r.Execute(context.Background(), "echo", json.RawMessage(`{"x":"hi"}`))
	out := outRes.Text
	c.NoError(err)
	c.Eq("got:hi", out, "unexpected output")
}

func schemaWithRequiredX() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "x", Type: "string"},
		},
		Required: []string{"x"},
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "nope", json.RawMessage(`{}`))
	assert.NewAborting(t).Error(err, "expected error for unknown tool")
}

func TestBuildDefFromRegistryTest(t *testing.T) {
	c := assert.NewAborting(t)
	schema := Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string"},
		},
		Required: []string{"path"},
	}
	def := BuildDef(&testEchoTool{name: "mytool", desc: "does a thing", schema: schema})
	c.NotNil(def.OfTool, "expected OfTool variant")
	c.Eq("mytool", def.OfTool.Name, "name =")
	if !def.OfTool.Description.Valid() || def.OfTool.Description.Value != "does a thing" {
		t.Fatalf("description = %+v", def.OfTool.Description)
	}
	if len(def.OfTool.InputSchema.Required) != 1 || def.OfTool.InputSchema.Required[0] != "path" {
		t.Fatalf("required = %v", def.OfTool.InputSchema.Required)
	}
}

// panickingTool panics in Execute for containment tests.
type panickingTool struct{ msg string }

func (p panickingTool) Name() string                                           { return p.msg }
func (p panickingTool) Description() string                                    { return "" }
func (p panickingTool) InputSchema() Schema                                    { return Schema{Type: "object"} }
func (p panickingTool) Execute(context.Context, ToolInput) (ToolResult, error) { panic(p.msg) }

// fineTool returns a fixed string.
type fineTool struct{}

func (fineTool) Name() string        { return "fine" }
func (fineTool) Description() string { return "works" }
func (fineTool) InputSchema() Schema { return Schema{Type: "object"} }
func (fineTool) Execute(context.Context, ToolInput) (ToolResult, error) {
	return NewTextResult("still here"), nil
}

func TestRegistryConcurrentExecute(t *testing.T) {
	r := NewRegistry()
	r.Register(&testEchoTool{name: "noop", result: "ok", schema: Schema{Type: "object"}})

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Definitions()
			if _, err := r.Execute(context.Background(), "noop", json.RawMessage(`{}`)); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		assert.NewAborting(t).NoError(err)
	}
}

func TestExecuteContainsAPanickingTool(t *testing.T) {
	c := assert.NewCollecting(t)
	r := NewRegistry()
	r.Register(panickingTool{msg: "boom"})
	r.Register(fineTool{})

	outRes, err := r.Execute(context.Background(), "boom", json.RawMessage(`{}`))
	out := outRes.Text
	c.Require().Error(err, "Execute returned a nil error for a panicking tool; the panic was not converted")
	c.Eq("", out, "Execute returned result")
	c.StrContains(err.Error(), "boom", "error %q does not carry the panic value; the model would learn nothing", err)
	c.StrContains(err.Error(), "boom", "error %q does not name the tool that panicked", err)

	outRes, err = r.Execute(context.Background(), "fine", json.RawMessage(`{}`))
	out = outRes.Text
	c.False(err != nil || out != "still here", "Execute(fine) = (%q, %v) after a contained panic, want (\"still here\", nil)", out, err)
}

func TestExecuteContainsAPanicFromConcurrentTools(t *testing.T) {
	r := NewRegistry()
	r.Register(panickingTool{msg: "boom"})

	const calls = 8
	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.Execute(context.Background(), "boom", json.RawMessage(`{}`))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		assert.NewCollecting(t).Error(err, "concurrent call %d: nil error, want the contained panic", i)
	}
}
