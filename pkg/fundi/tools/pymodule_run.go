// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/toolmeta"
)

const pymoduleRunDescription = "Run a Python script you saved with pymodule_put, by its " +
	"module name. `script` is the module name exactly as saved with " +
	"pymodule_put -- not a path, and not inline code; if you have edited a " +
	"module's code since saving it, pymodule_put it again before running. " +
	"`modules` names further saved pymodules the script imports; they go on " +
	"PYTHONPATH for the run. `cwd` optionally sets the working directory -- " +
	"absolute, or relative to your working directory; the default is your " +
	"working directory. Returns combined stdout/stderr and the exit code."

func init() { DefaultBlueprint.Register(&PyModuleRunBlueprint{}) }

type PyModuleRunBlueprint struct{}

func (PyModuleRunBlueprint) Name() string        { return "pymodule_run" }
func (PyModuleRunBlueprint) Description() string { return pymoduleRunDescription }
func (PyModuleRunBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "script", Type: "string", Description: "Name of the pymodule to run, exactly as saved with pymodule_put (e.g. \"analyze\"). A bare Python identifier, not a path."},
			{Name: "modules", Type: "array", Items: &Schema{Type: "string"}, Description: "Names of further pymodules the script imports, from pymodule_put."},
			{Name: "cwd", Type: "string", Description: "Optional working directory for the run -- absolute, ~-expanded, or relative to your working directory. Default: your working directory."},
			{Name: "args", Type: "array", Items: &Schema{Type: "string"}, Description: "Extra command-line arguments passed to the script."},
		},
		Required: []string{"script"},
	}
}
func (PyModuleRunBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PyModuleRunBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	interp := os.Getenv("RAFIKI_PYMODULE_PYTHON")
	if interp == "" {
		interp = "python3"
	}
	return &pymoduleRunTool{
		PyModuleRunBlueprint: PyModuleRunBlueprint{},
		cwd:                  opts.Cwd,
		interpreter:          interp,
		p:                    opts.OutputPolicy,
	}, nil
}

type pymoduleRunTool struct {
	PyModuleRunBlueprint
	cwd         string
	interpreter string
	p           OutputPolicy
}

type pymoduleRunInput struct {
	Script  string   `json:"script"`
	Modules []string `json:"modules"`
	Cwd     string   `json:"cwd"`
	Args    []string `json:"args"`
}

func (rt *pymoduleRunTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in pymoduleRunInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: invalid input: %w", err)
	}
	// The entry script is itself a pymodule: a bare Python identifier exactly
	// as saved with pymodule_put, never a path into a workspace. This is a
	// deliberately stricter rule than resolveToolPath (read/write) follows --
	// pymodule_run has no workspace vocabulary at all.
	if err := pymodules.ValidName(in.Script); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: script: %w", err)
	}
	for _, m := range in.Modules {
		if err := pymodules.ValidName(m); err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: module %q: %w", m, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	// The entry script is itself a saved pymodule: it runs from the synced
	// cache as <cache>/pymodules/<name>/<name>.py, never from a workspace
	// file. The process's working directory is the calling agent's own
	// workspace, or the call's own `cwd` resolved like the file tools' paths
	// -- so a script sees the same relative world a bash call would.
	// pymodule_run itself writes nothing there. Modules are consumed in
	// place, never copied: each named module must be synced to this
	// executor, and its directory goes on PYTHONPATH for this run only (the
	// script's own directory is sys.path[0] already). Bytecode from imports
	// lands inside the modules' own cache dirs (__pycache__/) -- the point
	// of running from the cache: it survives between runs, and the sync
	// prune sweeps at root level only, so it is left alone.
	cacheDir := filepath.Join(paths.CacheDir(), "pymodules")
	scriptDir := filepath.Join(cacheDir, in.Script)
	scriptPath := filepath.Join(scriptDir, in.Script+".py")
	if _, err := os.Stat(scriptPath); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: script %q is not synced to this executor (has it been saved with pymodule_put yet?): %w", in.Script, err)
	}
	modDirs := make([]string, 0, len(in.Modules))
	for _, m := range in.Modules {
		dir := filepath.Join(cacheDir, m)
		if _, err := os.Stat(filepath.Join(dir, m+".py")); err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: module %q is not synced to this executor (has it been saved with pymodule_put yet?): %w", m, err)
		}
		modDirs = append(modDirs, dir)
	}

	var env []string
	if len(modDirs) > 0 {
		pp := strings.Join(modDirs, string(os.PathListSeparator))
		if existing := os.Getenv("PYTHONPATH"); existing != "" {
			pp += string(os.PathListSeparator) + existing
		}
		env = append(env, "PYTHONPATH="+pp)
	}

	// Default cwd is the calling agent's workspace. An explicit cwd stands
	// in for bash's `cd <dir> && python …`: the run goes through the same
	// subprocess plumbing bash uses (runSubprocess sets the child's
	// directory -- the exec equivalent of cd), and sys.path[0] stays the
	// script's cache dir either way. The path resolves by the same rules as
	// the file tools: ~-expanded, relative against the workspace, absolute
	// out. Which COPY of the script runs is never affected: that is decided
	// solely by the synced cache.
	runCwd := rt.cwd
	if in.Cwd != "" {
		p, err := resolveToolPath(in.Cwd, "", rt.cwd)
		if err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: cwd: %w", err)
		}
		fi, statErr := os.Stat(p)
		if statErr != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: cwd %q: %w", in.Cwd, statErr)
		}
		if !fi.IsDir() {
			return ToolResult{}, fmt.Errorf("pymodule_run: cwd %q is not a directory", in.Cwd)
		}
		runCwd = p
	}

	args := append([]string{scriptPath}, in.Args...)
	out, _, runErr := runSubprocess(ctx, runCwd, bashWaitDelay, rt.interpreter, args, env)
	if runErr != nil {
		out += fmt.Sprintf("\n[pymodule_run: %v]\n", runErr)
	}

	spillName := toolmeta.ToolCallID(ctx)
	if spillName == "" {
		spillName = "pymodule_run"
	}
	return NewTextResult(rt.p.Clip(out, spillName)), nil
}
