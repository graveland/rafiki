// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// pymoduleSyntaxCheck runs `python3 -m py_compile` against code (written to
// a temp file first, since py_compile takes a path) using the daemon's own
// local python3. Returns "" whenever no definite syntax error was found --
// this covers BOTH "python3 is not on PATH at all" (best-effort: no check
// happened) AND "python3 ran and compiled it cleanly": callers cannot
// distinguish the two, and must not need to -- both mean "nothing to
// block". Returns the SyntaxError text (combined output, trimmed) only when
// python3 was actually found and actually reported one. There is no error
// return: an infrastructure hiccup (e.g. failing to write the temp file)
// degrades to "" the same as "tool unavailable", never a blocking failure
// in its own right -- the same best-effort principle as pymoduleLintCheck.
func pymoduleSyntaxCheck(code string) string {
	py, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	src, err := writePymoduleCheckFile(code)
	if err != nil {
		return ""
	}
	defer os.Remove(src)
	out, err := exec.Command(py, "-m", "py_compile", src).CombinedOutput()
	if err != nil {
		// py_compile exits non-zero exactly when it reports a compile
		// error; its report (stderr, usually) is what we surface. No
		// output with the failure degrades to "" like any other hiccup.
		return strings.TrimSpace(string(out))
	}
	return ""
}

// pymoduleLintCheck runs `uv tool run ruff check` (the RAFIKI_PYMODULE_UV
// override applies here too, the same env var the executor-side venv builder
// resolves uv from, for consistency -- default "uv") against code written to
// a temp file. Returns "" whenever uv/ruff can't run at all for any reason
// (not found, fetch failed, any exec error) -- fully best-effort, never a
// blocking failure. Returns ruff's combined output (trimmed) when it runs
// and reports findings; empty string when it runs clean.
func pymoduleLintCheck(code string) string {
	uv := os.Getenv("RAFIKI_PYMODULE_UV")
	if uv == "" {
		uv = "uv"
	}
	src, err := writePymoduleCheckFile(code)
	if err != nil {
		return ""
	}
	defer os.Remove(src)
	out, err := exec.Command(uv, "tool", "run", "ruff", "check", src).CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		// uv/ruff never ran: not found, not executable. Best-effort, so
		// "tool unavailable" is never surfaced as a finding.
		return ""
	}
	// Ruff's convention: exit 0 clean, exit 1 findings. A clean run's output
	// ("All checks passed!") is not a finding, and neither is any other
	// exit code (a uv fetch failure, a ruff invocation error) -- a failed
	// check is not a finding and degrades to "" like an unavailable tool.
	if err == nil || exitErr.ExitCode() != 1 {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// writePymoduleCheckFile writes code to a temp file for a syntax/lint check
// subprocess to read, since py_compile and ruff both take a path. The caller
// removes the file when done.
func writePymoduleCheckFile(code string) (string, error) {
	f, err := os.CreateTemp("", "rafiki-pymodule-check-*.py")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.WriteString(code); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	return name, f.Close()
}
