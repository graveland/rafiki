package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

func TestRoutedToolsGoToTheExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	fake := executorclient.NewFake()
	fake.SetResult("read", "from executor")
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	out, err := reg.Execute(context.Background(), "read", json.RawMessage(`{"file_path":"/x"}`))
	c.NoError(err)
	c.Eq("from executor", out, "read did not route to the executor; got")
}

// The routed set is exactly the machine-local tools and nothing else.
// Asserting the SET rather than probing one unrouted tool keeps this test
// independent of which other tools happen to be registered — phase 02 adds
// task_* and phase 06 must not start routing them.
func TestOnlyMachineLocalToolsAreRouted(t *testing.T) {
	want := map[string]bool{
		"read": true, "write": true, "edit": true, "glob": true, "grep": true,
		"ls": true, "bash": true, "bash_start": true, "bash_output": true, "bash_kill": true, "download": true,
		"lsp_call_hierarchy": true, "lsp_definition": true, "lsp_diagnostics": true,
		"lsp_references": true, "lsp_rename": true, "lsp_replace_symbol": true, "lsp_restart": true, "lsp_symbols": true,
		"pymodule_run": true,
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

// With no executor there is no workspace, so the tools that touch one are not
// registered at all — not registered and failing, and above all not running
// against the daemon's own filesystem. This is the rule the whole executor
// architecture exists to establish; everything else is plumbing.
func TestNoExecutorMeansNoWorkspaceTools(t *testing.T) {
	c := assert.NewCollecting(t)
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd:         t.TempDir(),
		FileTracker: tools.NewFileTracker(),
	})

	names := map[string]bool{}
	for _, def := range reg.Definitions() {
		if def.OfTool != nil {
			names[def.OfTool.Name] = true
		}
	}

	for _, name := range tools.WorkspaceTools() {
		c.False(names[name], "%q was registered with no executor; it would run on the daemon's own filesystem", name)
	}

	// The daemon tier is unaffected: an agent with no workspace is still an
	// agent. The task ledger is the sentinel — it materializes with a bare
	// ToolOpts, while agent_spawn would decline without an AgentSpawner and
	// webfetch/websearch decline without ToolsWeb.
	for _, name := range []string{"task_add", "task_list", "task_update", "task_drop"} {
		c.False(!names[name], "%q is missing; a workspace-less agent still has the daemon tier", name)
	}
}
