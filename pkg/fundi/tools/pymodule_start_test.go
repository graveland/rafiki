// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// pymodule_start must arrive at the spawner as a kind=script SpawnSpec with
// the pymodule spec, labels and budgets intact, and the child's name default
// to the script's name.
func TestPyModuleStartBuildsAScriptSpec(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	in := `{"repo":"local","script":"driver","modules":["helpers"],` +
		`"args":["--fast","5"],"labels":{"env":"work"},` +
		`"max_cost":2.5,"max_children":2,"executor":"env=work"}`
	if _, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(in)); err != nil {
		t.Fatalf("pymodule_start: %v", err)
	}
	if len(sp.spawned) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(sp.spawned))
	}
	spec := sp.spawned[0]
	if spec.Kind != protocol.KindScript {
		t.Errorf("Kind = %q, want %q", spec.Kind, protocol.KindScript)
	}
	if spec.Name != "driver" {
		t.Errorf("Name = %q, want the script's name as the default", spec.Name)
	}
	if spec.Script == nil {
		t.Fatalf("Script = nil, want the pymodule spec")
	}
	if spec.Script.Repo != "local" || spec.Script.Script != "driver" {
		t.Errorf("Script = %+v, want {local driver}", spec.Script)
	}
	if len(spec.Script.Modules) != 1 || spec.Script.Modules[0] != "helpers" {
		t.Errorf("Script.Modules = %v, want [helpers]", spec.Script.Modules)
	}
	if len(spec.Script.Args) != 2 || spec.Script.Args[0] != "--fast" || spec.Script.Args[1] != "5" {
		t.Errorf("Script.Args = %v, want [--fast 5]", spec.Script.Args)
	}
	if spec.Labels["env"] != "work" {
		t.Errorf("Labels = %v, want env=work", spec.Labels)
	}
	if spec.MaxCost == nil || *spec.MaxCost != 2.5 {
		t.Errorf("MaxCost = %v, want 2.5", spec.MaxCost)
	}
	if spec.MaxChildren == nil || *spec.MaxChildren != 2 {
		t.Errorf("MaxChildren = %v, want 2", spec.MaxChildren)
	}
	if spec.ExecutorSelector != "env=work" {
		t.Errorf("ExecutorSelector = %q, want env=work", spec.ExecutorSelector)
	}
	// The prompt must never be set: a script child's stdin carries no
	// protocol, and a prompt would be an inbox row the script did not ask
	// for.
	if spec.Prompt != "" {
		t.Errorf("Prompt = %q, want empty — pymodule_start takes no prompt", spec.Prompt)
	}
}

// Absent optionals must arrive as their zero requests, not as defaults that
// would silently widen or budget the child.
func TestPyModuleStartAbsentOptionalsStayNil(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	if _, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(`{"repo":"local","script":"job"}`)); err != nil {
		t.Fatalf("pymodule_start: %v", err)
	}
	spec := sp.spawned[0]
	if spec.MaxCost != nil || spec.MaxChildren != nil {
		t.Errorf("budgets arrived as %v/%v, want nil (nil means the daemon's default, per SpawnSpec's grant rule)", spec.MaxCost, spec.MaxChildren)
	}
	if spec.Labels != nil {
		t.Errorf("Labels = %v, want nil", spec.Labels)
	}
	if spec.ExecutorSelector != "" {
		t.Errorf("ExecutorSelector = %q, want empty (inherit confinement)", spec.ExecutorSelector)
	}
	if spec.Cwd == "" {
		t.Errorf("Cwd = empty, want the tool's bound cwd")
	}
}

// The name can be seen in the returned rendering, and the child id comes
// straight back — the tool returns at once, it does not wait.
func TestPyModuleStartReturnsTheChildAtOnce(t *testing.T) {
	sp := &fakeSpawner{nextID: "c_started"}
	sp.children = nil
	reg, ctx := newAgentTools(t, sp)
	text, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(`{"repo":"local","script":"driver"}`))
	if err != nil {
		t.Fatalf("pymodule_start: %v", err)
	}
	if !strings.Contains(text, "c_started") {
		t.Errorf("result %q does not name the child id", text)
	}
	if !strings.Contains(text, "script") {
		t.Errorf("result %q does not name the kind; a coordinator cannot tell a script child from a fundi one", text)
	}
}

// Same refusal shape as pymodule_run: a missing repo is a loud error, never
// a silent default — an implicit "local" would start a git-sourced call
// running (or missing) a same-named saved module with nothing in the result
// saying which scope it started.
func TestPyModuleStartRefusesMissingRepo(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(`{"script":"driver"}`))
	if err == nil {
		t.Fatal("missing repo must be refused")
	}
	if !strings.Contains(err.Error(), "repo is required") {
		t.Errorf("error = %v, want the repo-required wording", err)
	}
	if len(sp.spawned) != 0 {
		t.Fatalf("a refused call must not spawn")
	}
}

// Name validation mirrors pymodule_run: each name becomes a path
// segment and an import on the hosting side, and the daemon re-checks
// every one — the tool-side copy is what turns a typo into a refused
// call instead of a child that spawns and immediately fails. A non-
// local repo is validated by the same rule pymodule_run applies to it.
func TestPyModuleStartValidatesNames(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	for name, in := range map[string]string{
		"script with a slash": `{"repo":"local","script":"../etc/passwd"}`,
		"module with a space": `{"repo":"local","script":"ok","modules":["bad name"]}`,
		"script not a name":   `{"repo":"local","script":"1abc"}`,
		"empty modules entry": `{"repo":"local","script":"ok","modules":[""]}`,
		"repo a path segment": `{"repo":"a/b","script":"ok"}`,
		"git repo bad script": `{"repo":"ops","script":"not-a-name"}`,
	} {
		if _, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(in)); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if len(sp.spawned) != 0 {
		t.Fatalf("a refused call must not spawn; got %d", len(sp.spawned))
	}
}

// Without a spawner the tool declines entirely, like agent_spawn — a start
// tool that can only answer "not configured" costs a turn to learn nothing.
func TestPyModuleStartDeclinesWithoutSpawner(t *testing.T) {
	reg := DefaultBlueprint.MaterializeAll(ToolOpts{Cwd: t.TempDir()})
	if _, err := reg.Execute(context.Background(), "pymodule_start", json.RawMessage(`{}`)); err == nil {
		t.Fatal("pymodule_start must not be registered without a spawner")
	} else if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("decline must be a registry absence, got %v", err)
	}
}
