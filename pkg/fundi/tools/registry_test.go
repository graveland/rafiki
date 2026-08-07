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
)

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
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := DefaultBlueprint.MaterializeAll(ToolOpts{FileTracker: NewFileTracker(), Cwd: dir})
	_, err := r.Execute(context.Background(), "edit",
		json.RawMessage(`{"path":"`+p+`","old_string":"hello","new_string":"bye"}`))
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("expected read-before-edit error, got %v", err)
	}
	if _, err := r.Execute(context.Background(), "read", json.RawMessage(`{"path":"`+p+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(context.Background(), "edit",
		json.RawMessage(`{"path":"`+p+`","old_string":"hello","new_string":"bye"}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "bye world" {
		t.Fatalf("edit result: %s", b)
	}
}

func TestDefinitionsSortedByName(t *testing.T) {
	r := DefaultBlueprint.MaterializeAll(ToolOpts{FileTracker: NewFileTracker(), Cwd: t.TempDir()})
	defs := r.Definitions()
	names := toolNames(defs)
	if !sort.StringsAreSorted(names) {
		t.Fatalf("not sorted: %v", names)
	}
	if len(names) < 5 {
		t.Fatalf("expected at least 5 registered file tools, got %d: %v", len(names), names)
	}
}

func TestRegisterAndExecute(t *testing.T) {
	r := NewRegistry()
	r.Register(&testEchoTool{name: "echo", desc: "echoes its input", schema: schemaWithRequiredX(), result: "got:hi"})
	out, err := r.Execute(context.Background(), "echo", json.RawMessage(`{"x":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "got:hi" {
		t.Fatalf("unexpected output %q", out)
	}
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
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestBuildDefFromRegistryTest(t *testing.T) {
	schema := Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string"},
		},
		Required: []string{"path"},
	}
	def := BuildDef(&testEchoTool{name: "mytool", desc: "does a thing", schema: schema})
	if def.OfTool == nil {
		t.Fatal("expected OfTool variant")
	}
	if def.OfTool.Name != "mytool" {
		t.Fatalf("name = %q", def.OfTool.Name)
	}
	if !def.OfTool.Description.Valid() || def.OfTool.Description.Value != "does a thing" {
		t.Fatalf("description = %+v", def.OfTool.Description)
	}
	if len(def.OfTool.InputSchema.Required) != 1 || def.OfTool.InputSchema.Required[0] != "path" {
		t.Fatalf("required = %v", def.OfTool.InputSchema.Required)
	}
}

// panickingTool panics in Execute for containment tests.
type panickingTool struct{ msg string }

func (p panickingTool) Name() string                                    { return p.msg }
func (p panickingTool) Description() string                             { return "" }
func (p panickingTool) InputSchema() Schema                             { return Schema{Type: "object"} }
func (p panickingTool) Execute(context.Context, ToolInput) (ToolResult, error) { panic(p.msg) }

// fineTool returns a fixed string.
type fineTool struct{}

func (fineTool) Name() string                                    { return "fine" }
func (fineTool) Description() string                             { return "works" }
func (fineTool) InputSchema() Schema                              { return Schema{Type: "object"} }
func (fineTool) Execute(context.Context, ToolInput) (ToolResult, error) { return NewTextResult("still here"), nil }

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
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecuteContainsAPanickingTool(t *testing.T) {
	r := NewRegistry()
	r.Register(panickingTool{msg: "boom"})
	r.Register(fineTool{})

	out, err := r.Execute(context.Background(), "boom", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Execute returned a nil error for a panicking tool; the panic was not converted")
	}
	if out != "" {
		t.Errorf("Execute returned result %q for a panicking tool, want empty", out)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not carry the panic value; the model would learn nothing", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not name the tool that panicked", err)
	}

	out, err = r.Execute(context.Background(), "fine", json.RawMessage(`{}`))
	if err != nil || out != "still here" {
		t.Errorf("Execute(fine) = (%q, %v) after a contained panic, want (\"still here\", nil)", out, err)
	}
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
		if err == nil {
			t.Errorf("concurrent call %d: nil error, want the contained panic", i)
		}
	}
}
