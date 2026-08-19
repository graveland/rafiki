package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

func TestRoutedToolsGoToTheExecutor(t *testing.T) {
	fake := executorclient.NewFake()
	fake.SetResult("read", "from executor")
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	out, err := reg.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "from executor" {
		t.Fatalf("read did not route to the executor; got %q", out)
	}
}

// The routed set is exactly the machine-local tools and nothing else.
// Asserting the SET rather than probing one unrouted tool keeps this test
// independent of which other tools happen to be registered — phase 02 adds
// task_* and phase 06 must not start routing them.
func TestOnlyMachineLocalToolsAreRouted(t *testing.T) {
	want := map[string]bool{
		"read": true, "write": true, "edit": true, "glob": true, "grep": true,
		"ls": true, "bash": true, "bash_start": true, "bash_output": true, "bash_kill": true,
	}
	for _, name := range tools.RoutedToExecutor() {
		if !want[name] {
			t.Errorf("tool %q is routed to the executor but is not machine-local; "+
				"anything holding credentials or a DB pool must stay in the parent", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("tool %q should be routed to the executor but is not", name)
	}
}

func TestNilExecutorKeepsEverythingLocal(t *testing.T) {
	// With no executor configured, every tool runs in-process exactly as
	// before. This is what lets phase 06 land without changing default
	// behaviour for anyone who has not deployed an executor.
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd:         t.TempDir(),
		FileTracker: tools.NewFileTracker(),
	})
	p := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(p, []byte("local"), 0o644)
	out, err := reg.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"`+p+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "local") {
		t.Fatalf("got %q", out)
	}
}
