// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/pymodules"

	"github.com/multigres/testkit/assert"
)

// testPymoduleRunTool returns a materialized pymodule_run tool whose cwd is
// the calling agent's workspace, as the executor materializes it. Call
// t.Setenv("XDG_CACHE_HOME", ...) first when the test needs an isolated
// module cache.
func testPymoduleRunTool(t *testing.T, cwd string) Tool {
	t.Helper()
	tool, err := (&PyModuleRunBlueprint{}).Materialize(ToolOpts{Cwd: cwd})
	assert.NewAborting(t).NoError(err)
	return tool
}

// seedPymoduleCache writes <cache>/pymodules/<name>/<name>.py directly, in
// the layout the executor's SyncPyModules receiver produces. Deliberately
// not going through the sync path: that is a different layer (the executor
// receiver), and these tests exercise pymodule_run's own cache-execution and
// PYTHONPATH logic.
func seedPymoduleCache(t *testing.T, name, code string) {
	t.Helper()
	c := assert.NewAborting(t)
	dir := filepath.Join(paths.CacheDir(), "pymodules", name)
	c.NoError(os.MkdirAll(dir, 0o755))
	c.NoError(os.WriteFile(filepath.Join(dir, name+".py"), []byte(code), 0o644))
}

// seedGitPymoduleRepo writes <cache>/pymodule-repos/<name>/ directly, in the
// layout the executor's SyncPyModuleGitSource receiver produces: each script
// becomes scripts/<script>.py (a callable), each package becomes a top-level
// <pkg>/__init__.py (an importable package). Deliberately not going through
// the sync path, same philosophy as seedPymoduleCache: the git-source sync
// receiver is a different layer (and these tests must not need git or uv),
// and they exercise pymodule_run's own repo-path resolution, PYTHONPATH and
// interpreter logic.
func seedGitPymoduleRepo(t *testing.T, name string, scripts, packages map[string]string) {
	t.Helper()
	c := assert.NewAborting(t)
	repoDir := filepath.Join(paths.CacheDir(), "pymodule-repos", name)
	for script, code := range scripts {
		dir := filepath.Join(repoDir, "scripts")
		c.NoError(os.MkdirAll(dir, 0o755))
		c.NoError(os.WriteFile(filepath.Join(dir, script+".py"), []byte(code), 0o644))
	}
	for pkg, code := range packages {
		dir := filepath.Join(repoDir, pkg)
		c.NoError(os.MkdirAll(dir, 0o755))
		c.NoError(os.WriteFile(filepath.Join(dir, "__init__.py"), []byte(code), 0o644))
	}
}

// gitRepoDirOf is the synced checkout directory of a seeded git source, as
// pymodule_run itself computes it.
func gitRepoDirOf(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(paths.CacheDir(), "pymodule-repos", name)
}

// script is a bare Python identifier -- a saved module name -- so every
// path-like form is rejected by construction: traversal (`../sibling/x.py`),
// directory-prefixed (`sub/evil`), dot-relative (`./evil`), and the common
// file-with-extension habit (`analyze.py`), since `.` is not an identifier
// character.
func TestPymoduleRunRejectsPathLikeScriptNames(t *testing.T) {
	c := assert.NewAborting(t)
	parent := t.TempDir()
	sibling := filepath.Join(parent, "sibling")
	c.NoError(os.MkdirAll(sibling, 0o755))
	// evil.py writes a marker only if it is actually executed, so the test
	// proves the guard stopped the call rather than the file merely missing.
	marker := filepath.Join(sibling, "marker.txt")
	evil := fmt.Sprintf("open(%q, \"w\").write(\"ran\")\n", marker)
	c.NoError(os.WriteFile(filepath.Join(sibling, "evil.py"), []byte(evil), 0o644))

	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tool := testPymoduleRunTool(t, t.TempDir())
	for _, bad := range []string{"../sibling/evil.py", "sub/evil", "./evil", "analyze.py"} {
		_, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"repo": "local", "script": %q}`, bad)))
		c.Error(err, "want an error for the path-like script %q, got nil", bad)
		c.StrContains(err.Error(), "Python identifier", "error for %q should say script must be a bare Python identifier, got: %v", bad, err)
	}
	_, statErr := os.Stat(marker)
	c.True(os.IsNotExist(statErr), "evil.py was executed: the name validation failed")
}

func TestPymoduleRunRejectsPathInModuleName(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "main", "modules": ["../etc"]}`))
	c.Error(err, "want an error for a path in a module name, got nil")
	c.StrContains(err.Error(), "../etc", "error should name the offending module, got: %v", err)
	// Names are all validated before anything runs: the managed cache root
	// must not have been created because of it.
	_, statErr := os.Stat(filepath.Join(paths.CacheDir(), "pymodules", "etc"))
	c.True(os.IsNotExist(statErr), "something was written despite the invalid module name")
}

func TestPymoduleRunMakesModuleImportable(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	seedPymoduleCache(t, "runme", "import mymod\nprint(mymod.VALUE)\n")
	workspace := t.TempDir()

	tool := testPymoduleRunTool(t, workspace)
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.NoError(err)
	c.StrContains(res.Text, "42", "result should contain the module's printed value 42, got")

	// The workspace stays untouched: modules are consumed in place from the
	// cache, never copied, and cwd is not on sys.path.
	entries, err := os.ReadDir(workspace)
	c.NoError(err)
	c.Empty(entries, "the workspace should gain nothing from a run, got")

	// Bytecode from imports lands INSIDE the module's own cache dir -- the
	// point of running from the cache: it survives between runs, and the
	// sync prune sweeps at root level only, so it is left alone.
	stale, err := os.ReadDir(filepath.Join(paths.CacheDir(), "pymodules", "mymod"))
	c.NoError(err)
	var sawBytecode bool
	for _, e := range stale {
		if e.Name() == "__pycache__" {
			sawBytecode = true
		}
	}
	c.True(sawBytecode, "__pycache__ did not land inside the imported module's cache dir")
}

// The entry script executes from the synced cache, never from a workspace
// file: a same-named decoy in the agent's own working directory must be
// ignored and left untouched -- the cwd picks the process's view of the
// world, never which copy of the script runs.
func TestPymoduleRunExecutesCacheCopy(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "runme", "print(42)\n")
	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, "runme.py")
	decoyCode := "print(7)\n"
	c.NoError(os.WriteFile(decoy, []byte(decoyCode), 0o644))

	tool := testPymoduleRunTool(t, decoyDir)
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme"}`))
	c.NoError(err)
	c.StrContains(res.Text, "42", "the cache copy should run (42, not the decoy's 7), got")
	got, err := os.ReadFile(decoy)
	c.False(err != nil || string(got) != decoyCode, "decoy runme.py was touched: content=%q err=%v", got, err)
}

// The subprocess runs with the calling agent's workspace as its cwd -- the
// same relative world a bash call would see -- not the script's cache dir.
func TestPymoduleRunRunsInAgentWorkspace(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "where", "import os\nprint(os.getcwd())\n")
	// EvalSymlinks: t.TempDir() may hand out a /var path whose /private/var
	// resolution is what os.getcwd() reports.
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	c.NoError(err)

	tool := testPymoduleRunTool(t, workspace)
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "where"}`))
	c.NoError(err)
	c.StrContains(res.Text, workspace, "the script should run in the agent's workspace (")
}

// An optional cwd input moves the process -- resolved by the same rules as
// the file tools (~-expanded, relative against the agent's workspace) --
// without ever changing which copy of the script runs.
func TestPymoduleRunOptionalCwd(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "where", "import os\nprint(os.getcwd())\n")
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	c.NoError(err)
	sub := filepath.Join(workspace, "sub")
	c.NoError(os.MkdirAll(sub, 0o755))

	tool := testPymoduleRunTool(t, workspace)

	// Relative: resolved against the agent's workspace.
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "where", "cwd": "sub"}`))
	c.NoError(err)
	c.StrContains(res.Text, sub, "a relative cwd should resolve against the workspace (")

	// Absolute: taken as-is.
	absCwd, err := json.Marshal(sub)
	c.NoError(err)
	res, err = tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"repo": "local", "script": "where", "cwd": %s}`, absCwd)))
	c.NoError(err)
	c.StrContains(res.Text, sub, "an absolute cwd should be taken as-is (")

	// Tilde: expanded to the home directory.
	res, err = tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "where", "cwd": "~"}`))
	c.NoError(err)
	home, err := os.UserHomeDir()
	c.NoError(err)
	homeReal, err := filepath.EvalSymlinks(home)
	c.NoError(err)
	c.StrContains(res.Text, homeReal, "~ should expand to the home directory (")
}

// A cwd that does not exist fails as a clear tool error naming the
// directory, not as an opaque subprocess failure.
func TestPymoduleRunBadCwdFailsClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "main", "pass\n")
	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "main", "cwd": "no/such/dir"}`))
	c.Error(err, "want an error for a nonexistent cwd, got nil")
	c.StrContains(err.Error(), "no/such/dir", "error should name the offending directory, got: %v", err)
}

func TestPymoduleRunReportsMissingScriptClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // an empty cache: nothing synced
	c := assert.NewAborting(t)

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "analyze"}`))
	c.Error(err, "want an error for a script missing from the cache, got nil")
	c.False(!strings.Contains(err.Error(), "analyze") || !strings.Contains(err.Error(), "synced"), "error should name the script and say it is not synced (not a bare os.ErrNotExist), got: %v", err)
}

func TestPymoduleRunReportsMissingModuleClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // an empty cache: nothing synced
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "main", "pass\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "main", "modules": ["nonexistent"]}`))
	c.Error(err, "want an error for a module missing from the cache, got nil")
	c.False(!strings.Contains(err.Error(), "nonexistent") || !strings.Contains(err.Error(), "synced"), "error should name the module and say it is not synced (not a bare os.ErrNotExist), got: %v", err)
}

func TestPymoduleRunInterpreterEnvOverride(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_PYMODULE_PYTHON", "/nonexistent/interpreter")
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "main", "pass\n")

	// Materialized AFTER the env var is set: Materialize is where the
	// interpreter is resolved, so this proves the override is actually read.
	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "main"}`))
	c.NoError(err, "a failed run is reported in the result text, not as a tool error")
	c.StrContains(res.Text, "/nonexistent/interpreter", "output should reference the overridden interpreter path, got")
}

// fakeVenv fabricates a minimal per-module dependency venv inside dir: an
// executable .venv/bin/python3 -- a /bin/sh script whose body the test
// supplies, since runSubprocess execs it like any interpreter and no test
// here may run real uv or touch the network -- plus, when withSitePackages
// is set, a <dir>/.venv/lib/python3.11/site-packages directory for the
// PYTHONPATH glob to find.
func fakeVenv(t *testing.T, dir, interpreterBody string, withSitePackages bool) {
	t.Helper()
	c := assert.NewAborting(t)
	bin := filepath.Join(dir, ".venv", "bin")
	c.NoError(os.MkdirAll(bin, 0o755))
	py := filepath.Join(bin, "python3")
	c.NoError(os.WriteFile(py, []byte("#!/bin/sh\n"+interpreterBody), 0o755))
	if !withSitePackages {
		return
	}
	site := filepath.Join(dir, ".venv", "lib", "python3.11", "site-packages")
	c.NoError(os.MkdirAll(site, 0o755))
}

// requirementsCode is a module body declaring one dependency: the marker
// line exactly, then contiguous comment lines carrying requirements.txt-style
// pins.
const requirementsCode = "# pymodule-requirements:\n# requests>=2.31\n\nVALUE = 42\n"

// echoInterpreter is a fake venv interpreter body: it prints the path it was
// execed as ($0) and the PYTHONPATH it was handed, so a test can assert both
// which interpreter ran and what was on the path from one subprocess.
const echoInterpreter = "echo \"interp:$0\"\necho \"pp:$PYTHONPATH\"\n"

// scriptDirOf is the synced cache directory of a seeded pymodule, as
// pymodule_run itself computes it.
func scriptDirOf(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(paths.CacheDir(), "pymodules", name)
}

// pythonPathLine extracts the "pp:<PYTHONPATH>" line the fake interpreters
// and printer scripts emit, so assertions are made against the path value
// alone.
func pythonPathLine(t *testing.T, out string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "pp:")
	assert.NewAborting(t).True(ok, "output has no pp: line, got: %q", out)
	line, _, _ := strings.Cut(rest, "\n")
	return line
}

// The entry script's own venv runs the process, and its site-packages joins
// PYTHONPATH: sys.path[0] (the script's own dir) brings no site-packages
// with it.
func TestPymoduleRunScriptVenvInterpreterAndSitePackages(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "") // keep the echoed PYTHONPATH exactly assertable
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "runme", "pass\n")
	scriptDir := scriptDirOf(t, "runme")
	fakeVenv(t, scriptDir, echoInterpreter, true)

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme"}`))
	c.NoError(err)
	venvPython := filepath.Join(scriptDir, ".venv", "bin", "python3")
	c.StrContains(res.Text, "interp:"+venvPython, "the script's own venv python should run the process, got")
	site := filepath.Join(scriptDir, ".venv", "lib", "python3.11", "site-packages")
	c.Eq(site, pythonPathLine(t, res.Text), "PYTHONPATH")
}

// A module's venv never runs the process -- the fallback interpreter does --
// but its code dir AND its site-packages both join PYTHONPATH, code dir
// first.
func TestPymoduleRunModuleVenvSitePackagesOnPath(t *testing.T) {
	c := assert.NewAborting(t)
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "") // keep the printed PYTHONPATH exactly assertable
	seedPymoduleCache(t, "mymod", requirementsCode)
	modDir := scriptDirOf(t, "mymod")
	fakeVenv(t, modDir, "pass\n", true)
	// The fallback interpreter echoes its own path (so the fallback
	// selection is assertable) and then execs real python3 by ABSOLUTE path
	// -- independent of PATH resolution -- so the entry script can print its
	// PYTHONPATH.
	fallback := filepath.Join(t.TempDir(), "fake-python")
	fallbackBody := "#!/bin/sh\necho \"interp:$0\"\nexec " + python3 + " \"$@\"\n"
	c.NoError(os.WriteFile(fallback, []byte(fallbackBody), 0o755))
	t.Setenv("RAFIKI_PYMODULE_PYTHON", fallback) // must precede Materialize
	seedPymoduleCache(t, "runme", "import os\nprint(\"pp:\" + os.environ.get(\"PYTHONPATH\", \"\"))\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.NoError(err)
	c.StrContains(res.Text, "interp:"+fallback, "the fallback interpreter should run a script without a venv, got")
	codeDir := modDir
	site := filepath.Join(modDir, ".venv", "lib", "python3.11", "site-packages")
	c.Eq(codeDir+":"+site, pythonPathLine(t, res.Text), "PYTHONPATH")
}

// The no-regression pin: requirements-free code with no .venv runs exactly
// as before -- no error, and PYTHONPATH is exactly the module's code dir.
func TestPymoduleRunNoVenvAddsCodeDirOnly(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	modDir := scriptDirOf(t, "mymod")
	seedPymoduleCache(t, "runme", "import os\nimport mymod\nprint(\"pp:\" + os.environ.get(\"PYTHONPATH\", \"\"))\nprint(mymod.VALUE)\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.NoError(err)
	c.Eq(modDir, pythonPathLine(t, res.Text), "PYTHONPATH")
	c.StrContains(res.Text, "42", "the module should still be importable, got")
}

// An entry that declares dependencies but has no usable venv refuses the
// run up front -- nothing is executed -- instead of running against missing
// or half-installed packages.
func TestPymoduleRunNotReadyRefusesWhenVenvMissing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	// The entry script writes a marker only if executed, so the test proves
	// the refusal happened before any process ran.
	marker := filepath.Join(t.TempDir(), "marker.txt")
	seedPymoduleCache(t, "runme", fmt.Sprintf("open(%q, \"w\").write(\"ran\")\n", marker))
	seedPymoduleCache(t, "mymod", requirementsCode) // no .venv, no staging dir

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.Error(err, "want an error for a module that declares dependencies with no venv, got nil")
	c.False(!strings.Contains(err.Error(), "dependencies not ready") || !strings.Contains(err.Error(), "module mymod"), "error should name the module and say dependencies are not ready, got: %v", err)
	_, statErr := os.Stat(marker)
	c.True(os.IsNotExist(statErr), "the entry script was executed despite the dependencies-not-ready refusal")
}

// The readiness refusal covers the entry script too: a script that itself
// declares dependencies but has no venv refuses the run -- naming role
// "script" -- and nothing executes, exactly as for a module.
func TestPymoduleRunNotReadyRefusesWhenScriptVenvMissing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	// The entry script writes a marker only if executed, so the test proves
	// the refusal happened before any process ran.
	marker := filepath.Join(t.TempDir(), "marker.txt")
	seedPymoduleCache(t, "runme", fmt.Sprintf("%s\n# requests>=2.31\n\nopen(%q, \"w\").write(\"ran\")\n", pymodules.RequirementsMarker, marker)) // no .venv, no staging dir

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme"}`))
	c.Error(err, "want an error for a script that declares dependencies with no venv, got nil")
	c.False(!strings.Contains(err.Error(), "dependencies not ready") || !strings.Contains(err.Error(), "script runme"), "error should name the script and say dependencies are not ready, got: %v", err)
	_, statErr := os.Stat(marker)
	c.True(os.IsNotExist(statErr), "the entry script was executed despite the dependencies-not-ready refusal")
}

// A staging directory beside the code means a venv build is still in
// flight: the refusal says so, so the caller can retry shortly rather than
// conclude the build failed.
func TestPymoduleRunNotReadyReportsBuildInProgress(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "runme", "pass\n")
	seedPymoduleCache(t, "mymod", requirementsCode)
	c.NoError(os.MkdirAll(filepath.Join(scriptDirOf(t, "mymod"), ".rafiki-venv-staging-abc"), 0o755))

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.Error(err, "want an error for a module whose venv build is in progress, got nil")
	c.False(!strings.Contains(err.Error(), "build is in progress") || !strings.Contains(err.Error(), "module mymod"), "error should name the module and report the build as in progress, got: %v", err)
}

// Interpreter selection and PYTHONPATH composition are independent: a
// script with a venv runs under its own venv python even when the named
// modules are plain, and those modules' code dirs still join PYTHONPATH
// (after the script's site-packages, which leads as the first entry in call
// order).
func TestPymoduleRunScriptVenvUsedEvenWhenModulesPlain(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "runme", "pass\n") // the fake venv interpreter echoes; the body never runs
	scriptDir := scriptDirOf(t, "runme")
	fakeVenv(t, scriptDir, echoInterpreter, true)
	seedPymoduleCache(t, "mymod", "VALUE = 42\n") // plain: no requirements, no venv
	modDir := scriptDirOf(t, "mymod")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.NoError(err)
	venvPython := filepath.Join(scriptDir, ".venv", "bin", "python3")
	c.StrContains(res.Text, "interp:"+venvPython, "the script's venv python should be the interpreter even with plain modules, got")
	site := filepath.Join(scriptDir, ".venv", "lib", "python3.11", "site-packages")
	c.Eq(site+":"+modDir, pythonPathLine(t, res.Text), "PYTHONPATH")
}

// A venv whose lib layout matches no python3.* (malformed or partially
// built) degrades silently to code-dir-only participation: the run
// proceeds, with no site-packages entry and no error.
func TestPymoduleRunVenvSitePackagesGlobMissStillRuns(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	modDir := scriptDirOf(t, "mymod")
	// bin/python3 exists, so the venv counts as present, but lib holds no
	// python3.*/site-packages the glob could resolve.
	fakeVenv(t, modDir, "pass\n", false)
	c.NoError(os.MkdirAll(filepath.Join(modDir, ".venv", "lib", "python2.7"), 0o755))
	fallback := filepath.Join(t.TempDir(), "fake-python")
	c.NoError(os.WriteFile(fallback, []byte("#!/bin/sh\necho \"pp:$PYTHONPATH\"\n"), 0o755))
	t.Setenv("RAFIKI_PYMODULE_PYTHON", fallback) // must precede Materialize
	seedPymoduleCache(t, "runme", "pass\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.NoError(err, "a venv with no resolvable site-packages should not fail the run")
	c.Eq(modDir, pythonPathLine(t, res.Text), "PYTHONPATH")
}

// lineAfter extracts the text between the first marker occurrence and the
// end of its line, so a test can assert one printed value in isolation.
func lineAfter(t *testing.T, out, marker string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, marker)
	assert.NewAborting(t).True(ok, "output has no %q line, got: %q", marker, out)
	line, _, _ := strings.Cut(rest, "\n")
	return line
}

// A run that names modules sets PYTHONPATH -- and must keep the REST of the
// process environment too: the subprocess env is the caller's environment
// with PYTHONPATH swapped for the computed value, so PATH, HOME and anything
// the caller set reach the script. (It previously got an environment
// containing ONLY PYTHONPATH, which broke shelling out and any library
// reading HOME.)
func TestPymoduleRunKeepsProcessEnvWithModules(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_TEST_PYMODULE_MARKER", "present")
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	seedPymoduleCache(t, "runme", "import os\nimport mymod\n"+
		"print(mymod.VALUE)\n"+
		"print(\"path:\" + (os.environ.get(\"PATH\") or \"\"))\n"+
		"print(\"marker:\" + (os.environ.get(\"RAFIKI_TEST_PYMODULE_MARKER\") or \"\"))\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "runme", "modules": ["mymod"]}`))
	c.NoError(err)
	c.StrContains(res.Text, "42", "the module should still be importable, got")
	c.NotEq("", lineAfter(t, res.Text, "path:"), "the script should see PATH, got: %q", res.Text)
	got := lineAfter(t, res.Text, "marker:")
	c.Eq("present", got, "the script should see the caller's marker, got marker %q in: %q", got, res.Text)
}

// The same environment guarantee on the git-repo path: a checkout run also
// keeps the full process environment alongside its computed PYTHONPATH.
func TestPymoduleRunRepoKeepsProcessEnvWithModules(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_TEST_PYMODULE_MARKER", "present")
	seedGitPymoduleRepo(t, "ops_tools",
		map[string]string{"main": "import os\n" +
			"print(\"path:\" + (os.environ.get(\"PATH\") or \"\"))\n" +
			"print(\"marker:\" + (os.environ.get(\"RAFIKI_TEST_PYMODULE_MARKER\") or \"\"))\n"},
		nil)

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "main"}`))
	c.NoError(err)
	c.NotEq("", lineAfter(t, res.Text, "path:"), "the script should see PATH, got: %q", res.Text)
	got := lineAfter(t, res.Text, "marker:")
	c.Eq("present", got, "the script should see the caller's marker, got marker %q in: %q", got, res.Text)
}

// envWithPythonPath must hand back the full process environment with
// PYTHONPATH replaced: a pre-existing PYTHONPATH entry ends up exactly once,
// with the computed value, and every other entry is kept.
func TestEnvWithPythonPath(t *testing.T) {
	t.Setenv("PYTHONPATH", "stale/entry")
	t.Setenv("RAFIKI_TEST_PYMODULE_KEPT", "preserved")
	c := assert.NewAborting(t)
	env := envWithPythonPath("computed/dir")

	var pp []string
	for _, e := range env {
		if strings.HasPrefix(e, "PYTHONPATH=") {
			pp = append(pp, e)
		}
	}
	c.False(len(pp) != 1 || pp[0] != "PYTHONPATH=computed/dir", "PYTHONPATH should appear exactly once as the computed value, got %v in: %v", pp, env)
	kept := false
	for _, e := range env {
		if e == "RAFIKI_TEST_PYMODULE_KEPT=preserved" {
			kept = true
		}
	}
	c.True(kept, "the rest of the process environment must be kept, got: %v", env)
}

// The run description must tell an agent what a requirements block does at
// run time, on every face the tool is served from.
func TestPymoduleRunDescriptionMentionsRequirementsBehavior(t *testing.T) {
	for _, want := range []string{pymodules.RequirementsMarker, "PYTHONPATH", "venv interpreter"} {
		assert.NewCollecting(t).StrContains(pymoduleRunDescription, want, "pymodule_run description should mention")
	}
}

// TestPymoduleRunRepoLocalUnchanged pins the default path: repo="local" runs
// the blob-sourced logic exactly as before this task -- the seeded blob
// module still runs from <cache>/pymodules/, and a same-named git checkout
// is irrelevant to a local call.
func TestPymoduleRunRepoLocalUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "main", "print(\"ok-local\")\n")
	seedGitPymoduleRepo(t, "main", map[string]string{"main": "print(\"ok-repo\")\n"}, nil)

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "local", "script": "main"}`))
	c.NoError(err)
	c.False(!strings.Contains(res.Text, "ok-local") || strings.Contains(res.Text, "ok-repo"), "repo=local should run the blob-sourced cache copy, got: %q", res.Text)
}

// TestPymoduleRunRepoMissingFieldErrors: repo is required -- omitting it (or
// passing an empty string) fails with a message that says so, never a silent
// default to the blob store.
func TestPymoduleRunRepoMissingFieldErrors(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	seedPymoduleCache(t, "main", "pass\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	for _, input := range []string{"{\"script\": \"main\"}", `{"repo": "", "script": "main"}`} {
		_, err := tool.Execute(context.Background(), ToolInput(input))
		c.Error(err, "want an error for a missing repo field (%s), got nil", input)
		c.StrContains(err.Error(), "repo is required", "error for %s should say repo is required, got: %v", input, err)
	}
}

// TestPymoduleRunRepoRunsScriptFromRepo: a git-sourced call runs the
// checkout's scripts/<script>.py, not a blob-sourced copy.
func TestPymoduleRunRepoRunsScriptFromRepo(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedGitPymoduleRepo(t, "ops_tools", map[string]string{"rotate": "print(\"rotated\")\n"}, nil)

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "rotate"}`))
	c.NoError(err)
	c.StrContains(res.Text, "rotated", "the repo's script should run, got")
}

// TestPymoduleRunRepoAutoJoinsOwnPackages proves the auto-PYTHONPATH
// mechanism: the checkout root joins PYTHONPATH unconditionally, so a
// script's own intra-repo import resolves with NO modules argument at all --
// the caller never has to know or name which packages the repo contains.
func TestPymoduleRunRepoAutoJoinsOwnPackages(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedGitPymoduleRepo(t, "ops_tools",
		map[string]string{"main": "import ops_tools\nprint(\"got\", ops_tools.VALUE)\n"},
		map[string]string{"ops_tools": "VALUE = 99\n"})

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "main"}`))
	c.NoError(err)
	c.StrContains(res.Text, "got 99", "the repo's own package should be importable without modules (got 99), got")
}

// TestPymoduleRunRepoMissingScriptNamesRepoAndScript: the not-synced error
// names BOTH the script and the repo, since the same script name can exist
// in another source (names are scoped per repo).
func TestPymoduleRunRepoMissingScriptNamesRepoAndScript(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	seedGitPymoduleRepo(t, "ops_tools", map[string]string{"rotate": "pass\n"}, nil)

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "nonexistent"}`))
	c.Error(err, "want an error for a script missing from the repo, got nil")
	c.False(!strings.Contains(err.Error(), "nonexistent") || !strings.Contains(err.Error(), "ops_tools") || !strings.Contains(err.Error(), "synced"), "error should name the script and the repo and say it is not synced, got: %v", err)
}

// TestPymoduleRunRepoVenvInterpreterAndRootOnPath pins the interpreter and
// PYTHONPATH composition for a repo that has its venv: the repo's ONE shared
// .venv/bin/python3 runs the process, and PYTHONPATH is exactly the checkout
// root -- no site-packages entry (the venv python brings its own) and no
// module entries when none were named.
func TestPymoduleRunRepoVenvInterpreterAndRootOnPath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "") // keep the echoed PYTHONPATH exactly assertable
	c := assert.NewAborting(t)
	seedGitPymoduleRepo(t, "ops_tools", map[string]string{"main": "pass\n"}, nil)
	repoDir := gitRepoDirOf(t, "ops_tools")
	fakeVenv(t, repoDir, echoInterpreter, true)

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "main"}`))
	c.NoError(err)
	venvPython := filepath.Join(repoDir, ".venv", "bin", "python3")
	c.StrContains(res.Text, "interp:"+venvPython, "the repo's own venv python should run the process, got")
	c.Eq(repoDir, pythonPathLine(t, res.Text), "PYTHONPATH")
}

// TestPymoduleRunRepoModulesResolveWithinRepo: a modules entry resolves
// within the SAME repo as script -- repoDir/<name> -- and its directory
// joins PYTHONPATH after the checkout root; there is no way to name another
// source's package. Also pins the missing-module error shape.
func TestPymoduleRunRepoModulesResolveWithinRepo(t *testing.T) {
	c := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	seedGitPymoduleRepo(t, "ops_tools",
		map[string]string{"main": "import os\nimport ops_tools\nprint(\"pp:\" + os.environ.get(\"PYTHONPATH\", \"\"))\nprint(\"got\", ops_tools.VALUE)\n"},
		map[string]string{"ops_tools": "VALUE = 7\n"})

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "main", "modules": ["ops_tools"]}`))
	c.NoError(err)
	repoDir := gitRepoDirOf(t, "ops_tools")
	wantPP := repoDir + string(filepath.ListSeparator) + filepath.Join(repoDir, "ops_tools")
	c.Eq(wantPP, pythonPathLine(t, res.Text), "PYTHONPATH")
	c.StrContains(res.Text, "got 7", "the named module should still be importable, got")

	// A module name the checkout does not contain fails with the same
	// not-synced shape, naming the module and the repo.
	_, err = tool.Execute(context.Background(), ToolInput(`{"repo": "ops_tools", "script": "main", "modules": ["nope"]}`))
	c.Error(err, "want an error for a module missing from the repo, got nil")
	c.False(!strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "ops_tools") || !strings.Contains(err.Error(), "synced"), "error should name the module and the repo and say it is not synced, got: %v", err)
}

// TestPymoduleRunRepoRejectsPathLikeRepoNames pins the repo-name guard: a
// repo value becomes a path segment under the cache root, so traversal,
// directory-prefixed and file-extension forms are all rejected before
// anything on disk is touched.
func TestPymoduleRunRepoRejectsPathLikeRepoNames(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	tool := testPymoduleRunTool(t, t.TempDir())
	for _, bad := range []string{"../sibling/evil", "sub/evil", "./evil", "evil.py"} {
		_, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"repo": %q, "script": "main"}`, bad)))
		c.Error(err, "want an error for the path-like repo %q, got nil", bad)
		c.False(!strings.Contains(err.Error(), "git source name") || !strings.Contains(err.Error(), "repo"), "error for %q should be the repo's git source name rule, got: %v", bad, err)
	}
	// Nothing escaped the guard into the managed cache root.
	_, statErr := os.Stat(filepath.Join(paths.CacheDir(), "pymodule-repos", "sibling"))
	c.True(os.IsNotExist(statErr), "something was written despite the invalid repo name")
}
