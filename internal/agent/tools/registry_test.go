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
