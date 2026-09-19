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

const pymoduleRunDescription = "Run a Python script you saved with pymodule_put, or one from " +
	"a registered git source, by its module name. `repo` picks the source: " +
	"\"local\" for your own saved pymodules (pymodule_put), or a git " +
	"source's name as reported by discovery. `script` is the module name " +
	"exactly as saved with pymodule_put -- not a path, and not inline code; " +
	"if you have edited a module's code since saving it, pymodule_put it " +
	"again before running. `modules` names further pymodules the script " +
	"imports, resolved within the same repo; they go on " +
	"PYTHONPATH for the run, and a git source's own top-level packages are " +
	"importable without naming them. If the script or any named module " +
	"declares a requirements block (`# pymodule-requirements:`, see " +
	"pymodule_put), its installed packages are on PYTHONPATH for the run and " +
	"the script's own venv interpreter is used (script only). `cwd` " +
	"optionally sets the working directory -- absolute, or relative to your " +
	"working directory; the default is your working directory. Returns " +
	"combined stdout/stderr and the exit code."

func init() { DefaultBlueprint.Register(&PyModuleRunBlueprint{}) }

type PyModuleRunBlueprint struct{}

func (PyModuleRunBlueprint) Name() string        { return "pymodule_run" }
func (PyModuleRunBlueprint) Description() string { return pymoduleRunDescription }
func (PyModuleRunBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "repo", Type: "string", Description: "Which pymodule source to use: \"local\" for your own saved pymodules (pymodule_put), or a git source's name as reported by discovery."},
			{Name: "script", Type: "string", Description: "Name of the pymodule to run (e.g. \"analyze\"): for repo \"local\", exactly as saved with pymodule_put; otherwise a discovered script name of the repo, as reported by discovery. A bare Python identifier, not a path."},
			{Name: "modules", Type: "array", Items: &Schema{Type: "string"}, Description: "Names of further pymodules the script imports, resolved within the same repo: for repo \"local\", from pymodule_put; otherwise discovered packages of the repo, as reported by discovery."},
			{Name: "cwd", Type: "string", Description: "Optional working directory for the run -- absolute, ~-expanded, or relative to your working directory. Default: your working directory."},
			{Name: "args", Type: "array", Items: &Schema{Type: "string"}, Description: "Extra command-line arguments passed to the script."},
		},
		Required: []string{"script", "repo"},
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
	Repo    string   `json:"repo"`
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
	// `repo` is required by the schema, but a model can still omit it: fail
	// loudly instead of silently defaulting. An implicit default would leave
	// a git-sourced call silently running (or missing) a same-named
	// blob-sourced module, with nothing in the result saying which scope it
	// ran in.
	if in.Repo == "" {
		return ToolResult{}, fmt.Errorf("pymodule_run: repo is required: \"local\" for your own saved pymodules (pymodule_put), or a git source's name as reported by discovery")
	}
	if in.Repo != "local" {
		return rt.executeGitRepo(ctx, in)
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

	// The script's venv interpreter and site-packages are resolved per call,
	// not at Materialize time: the script's requirements can change between
	// materialization and this call, so whether it has a venv is only known
	// here. A venv always runs its OWN .venv/bin/python3, so a run stays
	// self-consistent with the venv that was built -- a changed
	// RAFIKI_PYMODULE_PYTHON never re-points or rebuilds an existing venv;
	// only future venv builds use the new interpreter.
	scriptVenvPython := pymoduleVenvPython(scriptDir)
	_, scriptVenvErr := os.Stat(scriptVenvPython)
	scriptHasVenv := scriptVenvErr == nil
	scriptCode, err := os.ReadFile(scriptPath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: script %q: %w", in.Script, err)
	}
	if err := pymoduleVenvReadiness("script", in.Script, scriptDir, pymodules.ParseRequirements(string(scriptCode)), scriptHasVenv); err != nil {
		return ToolResult{}, err
	}

	type pymoduleEntry struct {
		dir     string
		hasVenv bool
	}
	entries := make([]pymoduleEntry, 0, len(in.Modules))
	for _, m := range in.Modules {
		dir := filepath.Join(cacheDir, m)
		path := filepath.Join(dir, m+".py")
		if _, err := os.Stat(path); err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: module %q is not synced to this executor (has it been saved with pymodule_put yet?): %w", m, err)
		}
		_, venvErr := os.Stat(pymoduleVenvPython(dir))
		hasVenv := venvErr == nil
		code, err := os.ReadFile(path)
		if err != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: module %q: %w", m, err)
		}
		if err := pymoduleVenvReadiness("module", m, dir, pymodules.ParseRequirements(string(code)), hasVenv); err != nil {
			return ToolResult{}, err
		}
		entries = append(entries, pymoduleEntry{dir: dir, hasVenv: hasVenv})
	}

	// PYTHONPATH is a union, not dependency resolution: every entry with a
	// venv contributes its site-packages alongside its code directory, and
	// conflicts resolve by PYTHONPATH order like any other collision. The
	// script's own directory stays off PYTHONPATH (it is sys.path[0], which
	// brings no site-packages with it -- hence its site-packages here),
	// followed by the modules in call order, each emitting its code dir then
	// its site-packages; the pre-existing PYTHONPATH value trails last.
	var ppEntries []string
	if scriptHasVenv {
		if sp, ok := pymoduleSitePackages(scriptDir); ok {
			ppEntries = append(ppEntries, sp)
		}
	}
	for _, e := range entries {
		ppEntries = append(ppEntries, e.dir)
		if e.hasVenv {
			if sp, ok := pymoduleSitePackages(e.dir); ok {
				ppEntries = append(ppEntries, sp)
			}
		}
	}
	var env []string
	if len(ppEntries) > 0 {
		pp := strings.Join(ppEntries, string(os.PathListSeparator))
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

	// The venv interpreter wins over the fallback only for the script: a
	// module's venv never runs the process, its packages ride PYTHONPATH.
	interpreter := rt.interpreter
	if scriptHasVenv {
		interpreter = scriptVenvPython
	}
	args := append([]string{scriptPath}, in.Args...)
	out, _, runErr := runSubprocess(ctx, runCwd, bashWaitDelay, interpreter, args, env)
	if runErr != nil {
		out += fmt.Sprintf("\n[pymodule_run: %v]\n", runErr)
	}

	spillName := toolmeta.ToolCallID(ctx)
	if spillName == "" {
		spillName = "pymodule_run"
	}
	return NewTextResult(rt.p.Clip(out, spillName)), nil
}

// executeGitRepo is the resolution branch for any repo other than "local":
// script, modules and interpreter resolve against a git source's synced
// checkout at <cache>/pymodule-repos/<repo> -- the layout pkg/executor's
// SyncPyModuleGitSource receiver produces on THIS executor (the executor that
// ran the sync is the one running this tool), re-derived here from
// paths.CacheDir() rather than imported.
func (rt *pymoduleRunTool) executeGitRepo(ctx context.Context, in pymoduleRunInput) (ToolResult, error) {
	// The repo name becomes a path segment under the cache root, so it is
	// guarded by the same bare-identifier rule the blob path applies to its
	// own names. gitpymodules.ValidateName applies exactly this rule (plus
	// the "local" reservation, which the dispatch above already handled) to
	// every registered source, so this matches what a stored name can be
	// rather than being stricter; each consumption point still validates
	// independently, never trusting an earlier check.
	if err := pymodules.ValidName(in.Repo); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: repo: %w", err)
	}
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

	// A checkout keeps its callable scripts under scripts/, not the flat
	// <name>/<name>.py layout of the blob-sourced cache.
	repoDir := filepath.Join(paths.CacheDir(), "pymodule-repos", in.Repo)
	scriptPath := filepath.Join(repoDir, "scripts", in.Script+".py")
	if _, err := os.Stat(scriptPath); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_run: script %q is not synced to this executor under repo %q (has the git source been refreshed on it yet?): %w", in.Script, in.Repo, err)
	}

	// The whole repo shares ONE venv, so the interpreter question is a single
	// lookup, not the blob path's per-name one: the repo's own
	// .venv/bin/python3 when its build succeeded, otherwise the plain
	// fallback -- the same posture the blob path takes for a name with no
	// per-module venv. A checkout whose build failed or never ran therefore
	// runs against missing imports rather than being refused up front: the
	// refresh/inventory surface already reports VenvReady/VenvError for
	// exactly this, and a hard refusal here would black out every script of
	// the repo, ready or not. There is deliberately no requirements-block
	// readiness check either: a checkout's dependencies come from its own
	// manifest (uv sync's input), not from a per-name block.
	interpreter := rt.interpreter
	repoVenvPython := pymoduleVenvPython(repoDir)
	if _, err := os.Stat(repoVenvPython); err == nil {
		interpreter = repoVenvPython
	}

	// The checkout root itself joins PYTHONPATH unconditionally -- added
	// once, not once per module -- so a script's own intra-repo imports
	// (`import ops_tools`) resolve without the caller having to know or name
	// which top-level packages the repo happens to contain. A named module
	// must then exist as a top-level directory of the SAME checkout (modules
	// entries resolve within this repo only; there is no cross-source
	// composition), and its directory joins the path in call order. The
	// repo's venv needs no site-packages entry of its own: when it exists it
	// is the interpreter, and a venv python brings its own site-packages.
	ppEntries := []string{repoDir}
	for _, m := range in.Modules {
		dir := filepath.Join(repoDir, m)
		fi, statErr := os.Stat(dir)
		if statErr != nil {
			return ToolResult{}, fmt.Errorf("pymodule_run: module %q is not synced to this executor under repo %q (has the git source been refreshed on it yet?): %w", m, in.Repo, statErr)
		}
		if !fi.IsDir() {
			return ToolResult{}, fmt.Errorf("pymodule_run: module %q under repo %q is not a package directory", m, in.Repo)
		}
		ppEntries = append(ppEntries, dir)
	}
	pp := strings.Join(ppEntries, string(os.PathListSeparator))
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pp += string(os.PathListSeparator) + existing
	}
	env := []string{"PYTHONPATH=" + pp}

	// cwd resolution and the run itself are the blob path's mechanics
	// unchanged: the repo argument decides WHAT runs, never WHERE.
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
	out, _, runErr := runSubprocess(ctx, runCwd, bashWaitDelay, interpreter, args, env)
	if runErr != nil {
		out += fmt.Sprintf("\n[pymodule_run: %v]\n", runErr)
	}

	spillName := toolmeta.ToolCallID(ctx)
	if spillName == "" {
		spillName = "pymodule_run"
	}
	return NewTextResult(rt.p.Clip(out, spillName)), nil
}

// pymoduleVenvPython is the interpreter path of a pymodule's per-module
// dependency venv, a sibling of the module's own .py inside its synced cache
// directory. Venvs are consumed in place, never copied.
func pymoduleVenvPython(dir string) string {
	return filepath.Join(dir, ".venv", "bin", "python3")
}

// pymoduleSitePackages resolves a pymodule's venv site-packages directory by
// globbing <dir>/.venv/lib/python3.*/site-packages and taking the first
// match. ok is false when the glob finds nothing (a malformed or partially
// built venv): the caller then proceeds with the code directory alone,
// silently.
func pymoduleSitePackages(dir string) (sitePackages string, ok bool) {
	matches, err := filepath.Glob(filepath.Join(dir, ".venv", "lib", "python3.*", "site-packages"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	fi, err := os.Stat(matches[0])
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return matches[0], true
}

// pymoduleVenvReadiness refuses a run whose entry declares dependencies but
// has no usable venv, instead of running against missing or half-installed
// packages. An entry with no requirements -- the common case -- or with a
// venv already present always passes. The staging check distinguishes a
// build still in flight (retryable) from one that failed or never synced;
// only a staging DIRECTORY counts as in flight -- a leftover file that
// happens to carry the prefix is not a build, so it falls through to the
// terminal refusal.
func pymoduleVenvReadiness(role, name, dir string, reqs []string, hasVenv bool) error {
	if hasVenv || len(reqs) == 0 {
		return nil
	}
	staging, _ := filepath.Glob(filepath.Join(dir, ".rafiki-venv-staging-*"))
	for _, entry := range staging {
		if fi, err := os.Stat(entry); err == nil && fi.IsDir() {
			return fmt.Errorf("pymodule_run: %s %s: dependency venv build is in progress; retry shortly", role, name)
		}
	}
	return fmt.Errorf("pymodule_run: %s %s: dependencies not ready (venv missing — the build failed or has not synced; check the executor's sync or resave the module)", role, name)
}
