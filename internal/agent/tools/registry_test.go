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
	r, tr := NewRegistry(), NewFileTracker()
	RegisterFileTools(r, tr)
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
	r, tr := NewRegistry(), NewFileTracker()
	RegisterFileTools(r, tr)
	defs := r.Definitions()
	names := toolNames(defs)
	if !sort.StringsAreSorted(names) {
		t.Fatalf("not sorted: %v", names)
	}
	if len(names) != 5 {
		t.Fatalf("expected 5 registered file tools, got %d: %v", len(names), names)
	}
}

func TestRegisterAndExecute(t *testing.T) {
	r := NewRegistry()
	schema := `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`
	r.Register(Def("echo", "echoes its input", schema), func(_ context.Context, input json.RawMessage) (string, error) {
		var in struct {
			X string `json:"x"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", err
		}
		return "got:" + in.X, nil
	})
	out, err := r.Execute(context.Background(), "echo", json.RawMessage(`{"x":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "got:hi" {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestExecuteUnknownTool(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "nope", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestDefRoundTripsSchemaAndDescription(t *testing.T) {
	schema := `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
	def := Def("mytool", "does a thing", schema)
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

// TestRegistryConcurrentExecute drives Definitions/Register/Execute from many
// goroutines at once — the loop this Registry serves runs a tool batch under
// an errgroup with real concurrency, so the map access must be race-safe.
// Run with -race.
func TestRegistryConcurrentExecute(t *testing.T) {
	r := NewRegistry()
	schema := `{"type":"object"}`
	r.Register(Def("noop", "does nothing", schema), func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	})

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

// TestExecuteContainsAPanickingTool is the containment test for the entire
// tool surface. agentloop runs each tool call on its own errgroup goroutine
// and errgroup does not recover, so without the recover in Execute a panicking
// tool body unwinds a goroutine nothing owns and kills the daemon — which,
// in this test, means crashing the test binary rather than failing.
//
// The panic must come back as an ordinary tool error, because that is what
// agentloop turns into an is_error tool_result the model can see and react to.
func TestExecuteContainsAPanickingTool(t *testing.T) {
	r := NewRegistry()
	r.Register(Def("boom", "panics", `{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) {
			panic("tool exploded")
		})
	r.Register(Def("fine", "works", `{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) {
			return "still here", nil
		})

	out, err := r.Execute(context.Background(), "boom", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Execute returned a nil error for a panicking tool; the panic was not converted")
	}
	if out != "" {
		t.Errorf("Execute returned result %q for a panicking tool, want empty", out)
	}
	if !strings.Contains(err.Error(), "tool exploded") {
		t.Errorf("error %q does not carry the panic value; the model would learn nothing", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not name the tool that panicked", err)
	}

	// The registry must still be usable: containment is per-call, not a
	// one-way trip to a poisoned Registry.
	out, err = r.Execute(context.Background(), "fine", json.RawMessage(`{}`))
	if err != nil || out != "still here" {
		t.Errorf("Execute(fine) = (%q, %v) after a contained panic, want (\"still here\", nil)", out, err)
	}
}

// TestExecuteContainsAPanicFromConcurrentTools mirrors how agentloop actually
// calls Execute: several tools at once, each on its own goroutine. A recover
// that lived anywhere but inside Execute would miss these entirely.
func TestExecuteContainsAPanicFromConcurrentTools(t *testing.T) {
	r := NewRegistry()
	r.Register(Def("boom", "panics", `{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) {
			panic("concurrent explosion")
		})

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
