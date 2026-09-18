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

const pymoduleRunDescription = "Run a Python script, with named modules " +
	"from your pymodule store made importable first. `script` is the " +
	"basename of a file you already wrote (with the write tool) in your " +
	"working directory -- not a path, and not inline code. `modules` names " +
	"the pymodules (saved with pymodule_put) your script imports; their " +
	"synced directories go on PYTHONPATH for this run -- nothing is written " +
	"into your working directory, and a same-named file next to the script " +
	"shadows the stored one. Returns combined stdout/stderr and the exit code."

func init() { DefaultBlueprint.Register(&PyModuleRunBlueprint{}) }

type PyModuleRunBlueprint struct{}

func (PyModuleRunBlueprint) Name() string        { return "pymodule_run" }
func (PyModuleRunBlueprint) Description() string { return pymoduleRunDescription }
func (PyModuleRunBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "script", Type: "string", Description: "Basename of the entry script, e.g. \"analyze.py\". No path components."},
			{Name: "modules", Type: "array", Items: &Schema{Type: "string"}, Description: "Names of pymodules to make importable, from pymodule_put."},
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
	Args    []string `json:"args"`
}

// validBasename rejects anything that isn't a plain filename: empty, ".",
// "..", or containing a path separator. Deliberately stricter than
// resolveToolPath (used by write/read), which allows arbitrary relative and
// absolute paths on native executors -- pymodule_run's script/modules are
// scoped to "a file already in your cwd" by requirement, not by the general
// no-path-scoping rule the rest of the workspace tier follows.
//
// A local guard is used here rather than reusing pkg/executor's validSegment
// because pkg/executor imports pkg/fundi/tools (for tools.Registry and
// friends); the reverse import would be a cycle. Duplicating this ~10-line
// check is the correct fix, not an oversight.
func validBasename(n string) error {
	switch {
	case n == "":
		return fmt.Errorf("empty name")
	case n == "." || n == "..":
		return fmt.Errorf("reserved name %q", n)
	case strings.ContainsAny(n, "/\\"):
		return fmt.Errorf("name %q must be a bare basename, no path separators", n)
	case len(n) > 255:
		return fmt.Errorf("name %q exceeds 255 bytes", n)
	}
	return nil
}

func (rt *pymoduleRunTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in pymoduleRunInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: invalid input: %w", err)
	}
	if err := validBasename(in.Script); err != nil {
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

	scriptPath := filepath.Join(rt.cwd, in.Script)
	if _, err := os.Stat(scriptPath); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: script %q: %w", in.Script, err)
	}

	// Modules are consumed in place, never copied: each named module must be
	// synced to this executor as <cache>/pymodules/<name>/<name>.py, and its
	// directory goes on PYTHONPATH for this run only. The working directory
	// is never written to, and a same-named file next to the script
	// deliberately shadows the stored one (the script's own directory is
	// sys.path[0], ahead of PYTHONPATH).
	cacheDir := filepath.Join(paths.CacheDir(), "pymodules")
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
		// PYTHONDONTWRITEBYTECODE keeps __pycache__ out of the rafiki-managed
		// cache root, where every entry belongs to the prune sweep; bytecode
		// is worthless across runs at these sizes anyway.
		env = append(os.Environ(), "PYTHONPATH="+pp, "PYTHONDONTWRITEBYTECODE=1")
	}

	args := append([]string{scriptPath}, in.Args...)
	out, _, runErr := runSubprocess(ctx, rt.cwd, bashWaitDelay, rt.interpreter, args, env)
	if runErr != nil {
		out += fmt.Sprintf("\n[pymodule_run: %v]\n", runErr)
	}

	spillName := toolmeta.ToolCallID(ctx)
	if spillName == "" {
		spillName = "pymodule_run"
	}
	return NewTextResult(rt.p.Clip(out, spillName)), nil
}
