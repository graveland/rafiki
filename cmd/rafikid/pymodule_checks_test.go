// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeFakeLintUV writes an executable fake `uv` whose `tool run ruff check
// <file>` subcommand ignores its argument, prints one fixed finding line on
// stdout, and exits 1 (ruff's real convention for "findings present"). Any
// other invocation is a loud exit 64. It returns the directory containing the
// script; point RAFIKI_PYMODULE_UV at filepath.Join(dir, "uv"). Hermetic: no
// test using this helper ever shells out to a real uv or the network. The
// printed finding is fakeRuffFinding.
func writeFakeLintUV(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# Fake uv for rafiki's daemon-side lint-check tests -- see writeFakeLintUV.
if [ "$1" = "tool" ] && [ "$2" = "run" ] && [ "$3" = "ruff" ] && [ "$4" = "check" ]; then
	echo "` + fakeRuffFinding + `"
	exit 1
fi
echo "fake uv: unexpected invocation: $*" >&2
exit 64
`
	path := filepath.Join(dir, "uv")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

const fakeRuffFinding = "fake.py:1:1: F401 'os' imported but unused"

// disableLint points RAFIKI_PYMODULE_UV at a path that cannot exist, so a
// test that saves a module but does not care about the lint outcome never
// shells out to the developer's real uv (whose first `tool run ruff` may
// fetch ruff from the network -- never acceptable in a unit test). Lint is
// best-effort, so a missing uv is simply no findings.
func disableLint(t *testing.T) {
	t.Helper()
	t.Setenv("RAFIKI_PYMODULE_UV", "/nonexistent/uv")
}

// A deliberate syntax error must come back as the (non-empty) SyntaxError
// text, which is what blocks a save in pymoduleWriter.Put.
func TestPymoduleChecksSyntaxErrorReported(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	got := pymoduleSyntaxCheck("def f(:\n    pass\n")
	if got == "" {
		t.Error(`pymoduleSyntaxCheck(bad code) = "", want the non-empty SyntaxError text`)
	}
}

// Clean code reports no syntax error: "" is the pass-through for "nothing to
// block", not a signal the check failed to run.
func TestPymoduleChecksCleanCodeReportsNoError(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not found: %v", err)
	}
	if got := pymoduleSyntaxCheck("def f():\n    pass\n"); got != "" {
		t.Errorf("pymoduleSyntaxCheck(clean code) = %q, want \"\"", got)
	}
}

// With python3 absent from PATH the check is fully best-effort: "" for ANY
// code content, including code that would definitely fail to parse. This is
// the inverse of the two tests above and pins the "unavailable tool" branch
// never surfacing as a false-positive block.
func TestPymoduleChecksSkipsSilentlyWithoutPython3(t *testing.T) {
	if _, err := exec.LookPath("python3"); err == nil {
		t.Skip("python3 is on PATH; the unavailable-tool branch needs it absent")
	}
	t.Setenv("PATH", t.TempDir()) // a PATH with nothing on it
	for _, code := range []string{"def f():\n    pass\n", "def f(:\n    pass\n"} {
		if got := pymoduleSyntaxCheck(code); got != "" {
			t.Errorf("pymoduleSyntaxCheck(%q) = %q with no python3 on PATH, want \"\"", code, got)
		}
	}
}

// A RAFIKI_PYMODULE_UV pointing nowhere must degrade to "" -- no panic, no
// surfaced error (the function has no error return by design): a missing uv
// never blocks a save.
func TestPymoduleChecksLintSkipsSilentlyWithoutUv(t *testing.T) {
	t.Setenv("RAFIKI_PYMODULE_UV", "/nonexistent/uv")
	if got := pymoduleLintCheck("import os\n"); got != "" {
		t.Errorf("pymoduleLintCheck with a missing uv = %q, want \"\"", got)
	}
}

// The lint check reports exactly what the (fake) ruff printed, hermetically:
// the fake uv exits 1 with one fixed finding, and pymoduleLintCheck returns
// exactly that line.
func TestPymoduleChecksLintReturnsRuffFindings(t *testing.T) {
	uvDir := writeFakeLintUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	got := pymoduleLintCheck("import os\n")
	if got != fakeRuffFinding {
		t.Errorf("pymoduleLintCheck = %q, want the fake ruff finding %q", got, fakeRuffFinding)
	}
}
