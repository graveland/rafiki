// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// testPymoduleRunTool returns a materialized pymodule_run tool for tests.
// Call t.Setenv("XDG_CACHE_HOME", ...) first when the test needs an isolated
// module cache.
func testPymoduleRunTool(t *testing.T, cwd string) Tool {
	t.Helper()
	tool, err := (&PyModuleRunBlueprint{}).Materialize(ToolOpts{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

// seedPymoduleCache writes <cache>/pymodules/<name>.py directly, simulating a
// module already synced to this executor by wave 2's sync RPC. Deliberately
// not going through the sync path: that is a different layer (the executor
// receiver), and these tests exercise pymodule_run's own copy-and-run logic.
func seedPymoduleCache(t *testing.T, name, code string) {
	t.Helper()
	dir := filepath.Join(paths.CacheDir(), "pymodules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".py"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeScript writes a script file into cwd, as the write tool would have.
func writeScript(t *testing.T, cwd, name, code string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, name), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPymoduleRunRejectsPathInScript(t *testing.T) {
	parent := t.TempDir()
	cwd := filepath.Join(parent, "work")
	sibling := filepath.Join(parent, "sibling")
	for _, d := range []string{cwd, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// evil.py writes a marker only if it is actually executed, so the test
	// proves the guard stopped the call rather than the file merely missing.
	marker := filepath.Join(sibling, "marker.txt")
	evil := fmt.Sprintf("open(%q, \"w\").write(\"ran\")\n", marker)
	if err := os.WriteFile(filepath.Join(sibling, "evil.py"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := testPymoduleRunTool(t, cwd)
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "../sibling/evil.py"}`))
	if err == nil {
		t.Fatal("want an error for a path in script, got nil")
	}
	if !strings.Contains(err.Error(), "bare basename") {
		t.Fatalf("error should mention \"bare basename\", got: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("evil.py was executed: the path-traversal guard failed")
	}
}

func TestPymoduleRunRejectsPathInModuleName(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cwd := t.TempDir()
	writeScript(t, cwd, "main.py", "pass\n")

	tool := testPymoduleRunTool(t, cwd)
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "main.py", "modules": ["../etc"]}`))
	if err == nil {
		t.Fatal("want an error for a path in a module name, got nil")
	}
	if !strings.Contains(err.Error(), "../etc") {
		t.Fatalf("error should name the offending module, got: %v", err)
	}
	// The module name is checked before any file copy: nothing may have been
	// written into cwd for it ("../etc" would have landed as "etc.py").
	if _, statErr := os.Stat(filepath.Join(cwd, "etc.py")); !os.IsNotExist(statErr) {
		t.Fatal("a file was copied despite the invalid module name")
	}
}

func TestPymoduleRunCopiesModuleAndRuns(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedPymoduleCache(t, "mymod", "VALUE = 42\n")
	cwd := t.TempDir()
	writeScript(t, cwd, "script.py", "import mymod\nprint(mymod.VALUE)\n")

	tool := testPymoduleRunTool(t, cwd)
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "script.py", "modules": ["mymod"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "42") {
		t.Fatalf("result should contain the module's printed value 42, got: %q", res.Text)
	}
}

func TestPymoduleRunReportsMissingModuleClearly(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // an empty cache: nothing synced
	cwd := t.TempDir()
	writeScript(t, cwd, "main.py", "pass\n")

	tool := testPymoduleRunTool(t, cwd)
	_, err := tool.Execute(context.Background(), ToolInput(`{"script": "main.py", "modules": ["nonexistent"]}`))
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
	cwd := t.TempDir()
	writeScript(t, cwd, "main.py", "pass\n")

	// Materialized AFTER the env var is set: Materialize is where the
	// interpreter is resolved, so this proves the override is actually read.
	tool := testPymoduleRunTool(t, cwd)
	res, err := tool.Execute(context.Background(), ToolInput(`{"script": "main.py"}`))
	if err != nil {
		t.Fatalf("a failed run is reported in the result text, not as a tool error: %v", err)
	}
	if !strings.Contains(res.Text, "/nonexistent/interpreter") {
		t.Fatalf("output should reference the overridden interpreter path, got: %q", res.Text)
	}
}
