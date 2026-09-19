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
)

// testPymoduleRunTool returns a materialized pymodule_run tool whose cwd is
// the calling agent's workspace, as the executor materializes it. Call
// t.Setenv("XDG_CACHE_HOME", ...) first when the test needs an isolated
// module cache.
func testPymoduleRunTool(t *testing.T, cwd string) Tool {
	t.Helper()
	tool, err := (&PyModuleRunBlueprint{}).Materialize(ToolOpts{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// seedPymoduleCache writes <cache>/pymodules/<name>/<name>.py directly, in
// the layout the executor's SyncPyModules receiver produces. Deliberately
// not going through the sync path: that is a different layer (the executor
// receiver), and these tests exercise pymodule_run's own cache-execution and
// PYTHONPATH logic.
func seedPymoduleCache(t *testing.T, name, code string) {
	t.Helper()
	dir := filepath.Join(paths.CacheDir(), "pymodules", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

// script is a bare Python identifier -- a saved module name -- so every
// path-like form is rejected by construction: traversal (`../sibling/x.py`),
// directory-prefixed (`sub/evil`), dot-relative (`./evil`), and the common
// file-with-extension habit (`analyze.py`), since `.` is not an identifier
// character.
func TestPymoduleRunRejectsPathLikeScriptNames(t *testing.T) {
	parent := t.TempDir()
	sibling := filepath.Join(parent, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// evil.py writes a marker only if it is actually executed, so the test
	// proves the guard stopped the call rather than the file merely missing.
	marker := filepath.Join(sibling, "marker.txt")
	evil := fmt.Sprintf("open(%q, \"w\").write(\"ran\")\n", marker)
	if err := os.WriteFile(filepath.Join(sibling, "evil.py"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tool := testPymoduleRunTool(t, t.TempDir())
	for _, bad := range []string{"../sibling/evil.py", "sub/evil", "./evil", "analyze.py"} {
		_, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"script": %q}`, bad)))
		if err == nil {
			t.Fatalf("want an error for the path-like script %q, got nil", bad)
		}
		if !strings.Contains(err.Error(), "Python identifier") {
			t.Fatalf("error for %q should say script must be a bare Python identifier, got: %v", bad, err)
		}
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("evil.py was executed: the name validation failed")
	}
}

func TestPymoduleRunRejectsPathInModuleName(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "main", "modules": ["../etc"]}`))
	if err == nil {
		t.Fatal("want an error for a path in a module name, got nil")
	}
	if !strings.Contains(err.Error(), "../etc") {
		t.Fatalf("error should name the offending module, got: %v", err)
	}
	// Names are all validated before anything runs: the managed cache root
	// must not have been created because of it.
	if _, statErr := os.Stat(filepath.Join(paths.CacheDir(), "pymodules", "etc")); !os.IsNotExist(statErr) {
		t.Fatal("something was written despite the invalid module name")
	}
}

func TestPymoduleRunMakesModuleImportable(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	seedPymoduleCache(t, "runme", "import mymod\nprint(mymod.VALUE)\n")
	workspace := t.TempDir()

	tool := testPymoduleRunTool(t, workspace)
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "42") {
		t.Fatalf("result should contain the module's printed value 42, got: %q", res.Text)
	}

	// The workspace stays untouched: modules are consumed in place from the
	// cache, never copied, and cwd is not on sys.path.
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the workspace should gain nothing from a run, got %v", entries)
	}

	// Bytecode from imports lands INSIDE the module's own cache dir -- the
	// point of running from the cache: it survives between runs, and the
	// sync prune sweeps at root level only, so it is left alone.
	stale, err := os.ReadDir(filepath.Join(paths.CacheDir(), "pymodules", "mymod"))
	if err != nil {
		t.Fatal(err)
	}
	var sawBytecode bool
	for _, e := range stale {
		if e.Name() == "__pycache__" {
			sawBytecode = true
		}
	}
	if !sawBytecode {
		t.Fatal("__pycache__ did not land inside the imported module's cache dir")
	}
}

// The entry script executes from the synced cache, never from a workspace
// file: a same-named decoy in the agent's own working directory must be
// ignored and left untouched -- the cwd picks the process's view of the
// world, never which copy of the script runs.
func TestPymoduleRunExecutesCacheCopy(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "runme", "print(42)\n")
	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, "runme.py")
	decoyCode := "print(7)\n"
	if err := os.WriteFile(decoy, []byte(decoyCode), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := testPymoduleRunTool(t, decoyDir)
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "42") {
		t.Fatalf("the cache copy should run (42, not the decoy's 7), got: %q", res.Text)
	}
	got, err := os.ReadFile(decoy)
	if err != nil || string(got) != decoyCode {
		t.Fatalf("decoy runme.py was touched: content=%q err=%v", got, err)
	}
}

// The subprocess runs with the calling agent's workspace as its cwd -- the
// same relative world a bash call would see -- not the script's cache dir.
func TestPymoduleRunRunsInAgentWorkspace(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "where", "import os\nprint(os.getcwd())\n")
	// EvalSymlinks: t.TempDir() may hand out a /var path whose /private/var
	// resolution is what os.getcwd() reports.
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tool := testPymoduleRunTool(t, workspace)
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "where"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, workspace) {
		t.Fatalf("the script should run in the agent's workspace (%s), got: %q", workspace, res.Text)
	}
}

// An optional cwd input moves the process -- resolved by the same rules as
// the file tools (~-expanded, relative against the agent's workspace) --
// without ever changing which copy of the script runs.
func TestPymoduleRunOptionalCwd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "where", "import os\nprint(os.getcwd())\n")
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(workspace, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	tool := testPymoduleRunTool(t, workspace)

	// Relative: resolved against the agent's workspace.
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "where", "cwd": "sub"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, sub) {
		t.Fatalf("a relative cwd should resolve against the workspace (%s), got: %q", sub, res.Text)
	}

	// Absolute: taken as-is.
	absCwd, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	res, err = tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"script": "where", "cwd": %s}`, absCwd)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, sub) {
		t.Fatalf("an absolute cwd should be taken as-is (%s), got: %q", sub, res.Text)
	}

	// Tilde: expanded to the home directory.
	res, err = tool.Execute(context.Background(), ToolInput(`{"script": "where", "cwd": "~"}`))
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	homeReal, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, homeReal) {
		t.Fatalf("~ should expand to the home directory (%s), got: %q", homeReal, res.Text)
	}
}

// A cwd that does not exist fails as a clear tool error naming the
// directory, not as an opaque subprocess failure.
func TestPymoduleRunBadCwdFailsClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "main", "pass\n")
	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "main", "cwd": "no/such/dir"}`))
	if err == nil {
		t.Fatal("want an error for a nonexistent cwd, got nil")
	}
	if !strings.Contains(err.Error(), "no/such/dir") {
		t.Fatalf("error should name the offending directory, got: %v", err)
	}
}

func TestPymoduleRunReportsMissingScriptClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // an empty cache: nothing synced

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "analyze"}`))
	if err == nil {
		t.Fatal("want an error for a script missing from the cache, got nil")
	}
	if !strings.Contains(err.Error(), "analyze") || !strings.Contains(err.Error(), "synced") {
		t.Fatalf("error should name the script and say it is not synced (not a bare os.ErrNotExist), got: %v", err)
	}
}

func TestPymoduleRunReportsMissingModuleClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // an empty cache: nothing synced
	seedPymoduleCache(t, "main", "pass\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "main", "modules": ["nonexistent"]}`))
	if err == nil {
		t.Fatal("want an error for a module missing from the cache, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") || !strings.Contains(err.Error(), "synced") {
		t.Fatalf("error should name the module and say it is not synced (not a bare os.ErrNotExist), got: %v", err)
	}
}

func TestPymoduleRunInterpreterEnvOverride(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_PYMODULE_PYTHON", "/nonexistent/interpreter")
	seedPymoduleCache(t, "main", "pass\n")

	// Materialized AFTER the env var is set: Materialize is where the
	// interpreter is resolved, so this proves the override is actually read.
	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "main"}`))
	if err != nil {
		t.Fatalf("a failed run is reported in the result text, not as a tool error: %v", err)
	}
	if !strings.Contains(res.Text, "/nonexistent/interpreter") {
		t.Fatalf("output should reference the overridden interpreter path, got: %q", res.Text)
	}
}

// fakeVenv fabricates a minimal per-module dependency venv inside dir: an
// executable .venv/bin/python3 -- a /bin/sh script whose body the test
// supplies, since runSubprocess execs it like any interpreter and no test
// here may run real uv or touch the network -- plus, when withSitePackages
// is set, a <dir>/.venv/lib/python3.11/site-packages directory for the
// PYTHONPATH glob to find.
func fakeVenv(t *testing.T, dir, interpreterBody string, withSitePackages bool) {
	t.Helper()
	bin := filepath.Join(dir, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(bin, "python3")
	if err := os.WriteFile(py, []byte("#!/bin/sh\n"+interpreterBody), 0o755); err != nil {
		t.Fatal(err)
	}
	if !withSitePackages {
		return
	}
	site := filepath.Join(dir, ".venv", "lib", "python3.11", "site-packages")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
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
	if !ok {
		t.Fatalf("output has no pp: line, got: %q", out)
	}
	line, _, _ := strings.Cut(rest, "\n")
	return line
}

// The entry script's own venv runs the process, and its site-packages joins
// PYTHONPATH: sys.path[0] (the script's own dir) brings no site-packages
// with it.
func TestPymoduleRunScriptVenvInterpreterAndSitePackages(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "") // keep the echoed PYTHONPATH exactly assertable
	seedPymoduleCache(t, "runme", "pass\n")
	scriptDir := scriptDirOf(t, "runme")
	fakeVenv(t, scriptDir, echoInterpreter, true)

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme"}`))
	if err != nil {
		t.Fatal(err)
	}
	venvPython := filepath.Join(scriptDir, ".venv", "bin", "python3")
	if !strings.Contains(res.Text, "interp:"+venvPython) {
		t.Fatalf("the script's own venv python should run the process, got: %q", res.Text)
	}
	site := filepath.Join(scriptDir, ".venv", "lib", "python3.11", "site-packages")
	if got := pythonPathLine(t, res.Text); got != site {
		t.Fatalf("PYTHONPATH = %q, want exactly the script's site-packages %q (its code dir is sys.path[0], not PYTHONPATH)", got, site)
	}
}

// A module's venv never runs the process -- the fallback interpreter does --
// but its code dir AND its site-packages both join PYTHONPATH, code dir
// first.
func TestPymoduleRunModuleVenvSitePackagesOnPath(t *testing.T) {
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
	// -- the subprocess env carries only PYTHONPATH, so a bare `python3`
	// would not resolve -- so the entry script can print its PYTHONPATH.
	fallback := filepath.Join(t.TempDir(), "fake-python")
	fallbackBody := "#!/bin/sh\necho \"interp:$0\"\nexec " + python3 + " \"$@\"\n"
	if err := os.WriteFile(fallback, []byte(fallbackBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_PYMODULE_PYTHON", fallback) // must precede Materialize
	seedPymoduleCache(t, "runme", "import os\nprint(\"pp:\" + os.environ.get(\"PYTHONPATH\", \"\"))\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "interp:"+fallback) {
		t.Fatalf("the fallback interpreter should run a script without a venv, got: %q", res.Text)
	}
	codeDir := modDir
	site := filepath.Join(modDir, ".venv", "lib", "python3.11", "site-packages")
	if got := pythonPathLine(t, res.Text); got != codeDir+":"+site {
		t.Fatalf("PYTHONPATH = %q, want %q (module code dir, then its site-packages)", got, codeDir+":"+site)
	}
}

// The no-regression pin: requirements-free code with no .venv runs exactly
// as before -- no error, and PYTHONPATH is exactly the module's code dir.
func TestPymoduleRunNoVenvAddsCodeDirOnly(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	modDir := scriptDirOf(t, "mymod")
	seedPymoduleCache(t, "runme", "import os\nimport mymod\nprint(\"pp:\" + os.environ.get(\"PYTHONPATH\", \"\"))\nprint(mymod.VALUE)\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := pythonPathLine(t, res.Text); got != modDir {
		t.Fatalf("PYTHONPATH = %q, want exactly the module's code dir %q (no venv, so no site-packages entry)", got, modDir)
	}
	if !strings.Contains(res.Text, "42") {
		t.Fatalf("the module should still be importable, got: %q", res.Text)
	}
}

// An entry that declares dependencies but has no usable venv refuses the
// run up front -- nothing is executed -- instead of running against missing
// or half-installed packages.
func TestPymoduleRunNotReadyRefusesWhenVenvMissing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// The entry script writes a marker only if executed, so the test proves
	// the refusal happened before any process ran.
	marker := filepath.Join(t.TempDir(), "marker.txt")
	seedPymoduleCache(t, "runme", fmt.Sprintf("open(%q, \"w\").write(\"ran\")\n", marker))
	seedPymoduleCache(t, "mymod", requirementsCode) // no .venv, no staging dir

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err == nil {
		t.Fatal("want an error for a module that declares dependencies with no venv, got nil")
	}
	if !strings.Contains(err.Error(), "dependencies not ready") || !strings.Contains(err.Error(), "module mymod") {
		t.Fatalf("error should name the module and say dependencies are not ready, got: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("the entry script was executed despite the dependencies-not-ready refusal")
	}
}

// The readiness refusal covers the entry script too: a script that itself
// declares dependencies but has no venv refuses the run -- naming role
// "script" -- and nothing executes, exactly as for a module.
func TestPymoduleRunNotReadyRefusesWhenScriptVenvMissing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	// The entry script writes a marker only if executed, so the test proves
	// the refusal happened before any process ran.
	marker := filepath.Join(t.TempDir(), "marker.txt")
	seedPymoduleCache(t, "runme", fmt.Sprintf("%s\n# requests>=2.31\n\nopen(%q, \"w\").write(\"ran\")\n", pymodules.RequirementsMarker, marker)) // no .venv, no staging dir

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme"}`))
	if err == nil {
		t.Fatal("want an error for a script that declares dependencies with no venv, got nil")
	}
	if !strings.Contains(err.Error(), "dependencies not ready") || !strings.Contains(err.Error(), "script runme") {
		t.Fatalf("error should name the script and say dependencies are not ready, got: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("the entry script was executed despite the dependencies-not-ready refusal")
	}
}

// A staging directory beside the code means a venv build is still in
// flight: the refusal says so, so the caller can retry shortly rather than
// conclude the build failed.
func TestPymoduleRunNotReadyReportsBuildInProgress(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "runme", "pass\n")
	seedPymoduleCache(t, "mymod", requirementsCode)
	if err := os.MkdirAll(filepath.Join(scriptDirOf(t, "mymod"), ".rafiki-venv-staging-abc"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := testPymoduleRunTool(t, t.TempDir())
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err == nil {
		t.Fatal("want an error for a module whose venv build is in progress, got nil")
	}
	if !strings.Contains(err.Error(), "build is in progress") || !strings.Contains(err.Error(), "module mymod") {
		t.Fatalf("error should name the module and report the build as in progress, got: %v", err)
	}
}

// Interpreter selection and PYTHONPATH composition are independent: a
// script with a venv runs under its own venv python even when the named
// modules are plain, and those modules' code dirs still join PYTHONPATH
// (after the script's site-packages, which leads as the first entry in call
// order).
func TestPymoduleRunScriptVenvUsedEvenWhenModulesPlain(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	seedPymoduleCache(t, "runme", "pass\n") // the fake venv interpreter echoes; the body never runs
	scriptDir := scriptDirOf(t, "runme")
	fakeVenv(t, scriptDir, echoInterpreter, true)
	seedPymoduleCache(t, "mymod", "VALUE = 42\n") // plain: no requirements, no venv
	modDir := scriptDirOf(t, "mymod")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err != nil {
		t.Fatal(err)
	}
	venvPython := filepath.Join(scriptDir, ".venv", "bin", "python3")
	if !strings.Contains(res.Text, "interp:"+venvPython) {
		t.Fatalf("the script's venv python should be the interpreter even with plain modules, got: %q", res.Text)
	}
	site := filepath.Join(scriptDir, ".venv", "lib", "python3.11", "site-packages")
	if got := pythonPathLine(t, res.Text); got != site+":"+modDir {
		t.Fatalf("PYTHONPATH = %q, want %q (script site-packages leads, module code dir follows)", got, site+":"+modDir)
	}
}

// A venv whose lib layout matches no python3.* (malformed or partially
// built) degrades silently to code-dir-only participation: the run
// proceeds, with no site-packages entry and no error.
func TestPymoduleRunVenvSitePackagesGlobMissStillRuns(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PYTHONPATH", "")
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	modDir := scriptDirOf(t, "mymod")
	// bin/python3 exists, so the venv counts as present, but lib holds no
	// python3.*/site-packages the glob could resolve.
	fakeVenv(t, modDir, "pass\n", false)
	if err := os.MkdirAll(filepath.Join(modDir, ".venv", "lib", "python2.7"), 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(t.TempDir(), "fake-python")
	if err := os.WriteFile(fallback, []byte("#!/bin/sh\necho \"pp:$PYTHONPATH\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAFIKI_PYMODULE_PYTHON", fallback) // must precede Materialize
	seedPymoduleCache(t, "runme", "pass\n")

	tool := testPymoduleRunTool(t, t.TempDir())
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "runme", "modules": ["mymod"]}`))
	if err != nil {
		t.Fatalf("a venv with no resolvable site-packages should not fail the run: %v", err)
	}
	if got := pythonPathLine(t, res.Text); got != modDir {
		t.Fatalf("PYTHONPATH = %q, want exactly the module's code dir %q (glob miss: no site-packages entry, no error)", got, modDir)
	}
}

// The run description must tell an agent what a requirements block does at
// run time, on every face the tool is served from.
func TestPymoduleRunDescriptionMentionsRequirementsBehavior(t *testing.T) {
	for _, want := range []string{pymodules.RequirementsMarker, "PYTHONPATH", "venv interpreter"} {
		if !strings.Contains(pymoduleRunDescription, want) {
			t.Errorf("pymodule_run description should mention %q, got: %q", want, pymoduleRunDescription)
		}
	}
}
