// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// pymodule_start must arrive at the spawner as a kind=script SpawnSpec with
// the pymodule spec, labels and budgets intact, and the child's name default
// to the script's name.
func TestPyModuleStartBuildsAScriptSpec(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	in := `{"repo":"local","script":"driver","modules":["helpers"],` +
		`"args":["--fast","5"],"labels":{"env":"work"},` +
		`"max_cost":2.5,"max_children":2,"executor":"env=work"}`
	_, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(in))
	c.Require().NoError(err, "pymodule_start")
	c.Require().Len(sp.spawned, 1, "want 1 spawn, got %d", len(sp.spawned))
	spec := sp.spawned[0]
	c.Eq(protocol.KindScript, spec.Kind, "Kind")
	c.Eq("driver", spec.Name, "Name")
	c.Require().NotNil(spec.Script, "Script = nil, want the pymodule spec")
	c.False(spec.Script.Repo != "local" || spec.Script.Script != "driver", "Script = %+v, want {local driver}", spec.Script)
	if len(spec.Script.Modules) != 1 || spec.Script.Modules[0] != "helpers" {
		t.Errorf("Script.Modules = %v, want [helpers]", spec.Script.Modules)
	}
	if len(spec.Script.Args) != 2 || spec.Script.Args[0] != "--fast" || spec.Script.Args[1] != "5" {
		t.Errorf("Script.Args = %v, want [--fast 5]", spec.Script.Args)
	}
	c.Eq("work", spec.Labels["env"], "Labels = %v, want env=work", spec.Labels)
	c.False(spec.MaxCost == nil || *spec.MaxCost != 2.5, "MaxCost = %v, want 2.5", spec.MaxCost)
	c.False(spec.MaxChildren == nil || *spec.MaxChildren != 2, "MaxChildren = %v, want 2", spec.MaxChildren)
	c.Eq("env=work", spec.ExecutorSelector, "ExecutorSelector")
	// The prompt must never be set: a script child's stdin carries no
	// protocol, and a prompt would be an inbox row the script did not ask
	// for.
	c.Eq("", spec.Prompt, "Prompt")
}

// Absent optionals must arrive as their zero requests, not as defaults that
// would silently widen or budget the child.
func TestPyModuleStartAbsentOptionalsStayNil(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(`{"repo":"local","script":"job"}`))
	c.Require().NoError(err, "pymodule_start")
	spec := sp.spawned[0]
	c.False(spec.MaxCost != nil || spec.MaxChildren != nil, "budgets arrived as %v/%v, want nil (nil means the daemon's default, per SpawnSpec's grant rule)", spec.MaxCost, spec.MaxChildren)
	c.Nil(spec.Labels, "Labels")
	c.Eq("", spec.ExecutorSelector, "ExecutorSelector")
	c.NotEq("", spec.Cwd, "Cwd = empty, want the tool's bound cwd")
}

// The name can be seen in the returned rendering, and the child id comes
// straight back — the tool returns at once, it does not wait.
func TestPyModuleStartReturnsTheChildAtOnce(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{nextID: "c_started"}
	sp.children = nil
	reg, ctx := newAgentTools(t, sp)
	textRes, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(`{"repo":"local","script":"driver"}`))
	text := textRes.Text
	c.Require().NoError(err, "pymodule_start")
	c.StrContains(text, "c_started", "result")
	c.StrContains(text, "script", "result")
}

// Same refusal shape as pymodule_run: a missing repo is a loud error, never
// a silent default — an implicit "local" would start a git-sourced call
// running (or missing) a same-named saved module with nothing in the result
// saying which scope it started.
func TestPyModuleStartRefusesMissingRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(`{"script":"driver"}`))
	c.Require().Error(err, "missing repo must be refused")
	c.StrContains(err.Error(), "repo is required", "error = %v, want the repo-required wording", err)
	c.Require().Empty(sp.spawned, "a refused call must not spawn")
}

// Name validation mirrors pymodule_run: each name becomes a path
// segment and an import on the hosting side, and the daemon re-checks
// every one — the tool-side copy is what turns a typo into a refused
// call instead of a child that spawns and immediately fails. A non-
// local repo is validated by the same rule pymodule_run applies to it.
func TestPyModuleStartValidatesNames(t *testing.T) {
	c := assert.NewCollecting(t)
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
		_, err := reg.Execute(ctx, "pymodule_start", json.RawMessage(in))
		c.Error(err, "%s: must be refused", name)
	}
	c.Require().Empty(sp.spawned, "a refused call must not spawn; got %d", len(sp.spawned))
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
