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
