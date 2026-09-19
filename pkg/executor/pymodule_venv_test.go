// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// writeFakeUV writes an executable fake `uv` into a fresh temp dir and
// returns that dir. It implements exactly the two subcommands
// buildModuleVenv invokes, so the suite stays hermetic: no test here ever
// shells out to a real uv or the network.
//
//   - uv venv --python <interp> <path>  creates <path>/bin/ containing an
//     executable stub <path>/bin/python3.
//   - uv pip install --python <path> -r <reqfile>  exits 1 with
//     "simulated install failure" on stderr iff <reqfile> contains the
//     literal line FAIL_THIS_INSTALL.
//
// Every invocation appends one line to the file named by $FAKE_UV_LOG, so a
// test can assert how many times uv actually ran. The caller points
// RAFIKI_PYMODULE_UV at the script:
//
//	uvDir := writeFakeUV(t)
//	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
func writeFakeUV(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# Fake uv for rafiki's executor tests -- see writeFakeUV.
: "${FAKE_UV_LOG:=/dev/null}"
printf '%s\n' "$*" >> "$FAKE_UV_LOG"
case "$1" in
venv)
	# venv --python <interp> <path>
	path="$4"
	mkdir -p "$path/bin" || exit 1
	printf '#!/bin/sh\nexit 0\n' > "$path/bin/python3" || exit 1
	chmod +x "$path/bin/python3"
	;;
pip)
	# pip install --python <path> -r <reqfile>
	reqfile="$6"
	if [ -f "$reqfile" ] && grep -qx 'FAIL_THIS_INSTALL' "$reqfile"; then
		echo "simulated install failure" >&2
		exit 1
	fi
	;;
*)
	echo "fake uv: unexpected invocation: $*" >&2
	exit 64
	;;
esac
exit 0
`
	path := filepath.Join(dir, "uv")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setFakeUVLog points $FAKE_UV_LOG at a fresh file and returns its path, so
// a test can count the fake uv's invocations via fakeUVInvocations.
func setFakeUVLog(t *testing.T) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "fake-uv-calls.log")
	t.Setenv("FAKE_UV_LOG", log)
	return log
}

// fakeUVInvocations reports how many lines the fake uv has appended to its
// log -- one per invocation. A missing log means zero invocations.
func fakeUVInvocations(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func TestBuildVenvNoRequirementsIsReadyAndRemovesStaleVenv(t *testing.T) {
	moduleDir := t.TempDir()
	venv := filepath.Join(moduleDir, ".venv")
	if err := os.MkdirAll(filepath.Join(venv, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "bin", "python3"), []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := buildModuleVenv(moduleDir, nil)
	if !res.GetReady() || res.GetError() != "" {
		t.Errorf("got {Ready: %v, Error: %q}, want {Ready: true, Error: \"\"}", res.GetReady(), res.GetError())
	}
	if _, err := os.Stat(venv); !os.IsNotExist(err) {
		t.Errorf("stale .venv survived a no-requirements build (err=%v)", err)
	}
}

// Truncation must be UTF-8-safe: protobuf-go rejects invalid UTF-8 in proto3
// string fields at marshal time, so one garbled pip traceback sliced mid-rune
// would fail the entire SyncPyModulesResponse and lose every module's result.
// A bare repeat of the two-byte "é" would put the 4096-byte cut on a rune
// boundary by even parity, so a leading ASCII byte shifts the cut into the
// middle of a rune.
func TestUvErrorTruncationIsUTF8Safe(t *testing.T) {
	msg := "x" + strings.Repeat("é", 2500) // 5001 bytes; the cut straddles a rune
	got := uvError([]byte(msg), errors.New("boom"))
	if !utf8.ValidString(got) {
		t.Errorf("uvError output is not valid UTF-8: %q", got)
	}
	const suffix = "... (truncated)"
	if !strings.HasSuffix(got, suffix) {
		t.Errorf("uvError output %q lacks the truncation suffix %q", got, suffix)
	}
	if max := maxVenvErrorBytes + len(suffix); len(got) > max {
		t.Errorf("uvError output is %d bytes, want <= %d", len(got), max)
	}
}

func TestBuildVenvBuildsFromScratch(t *testing.T) {
	uvDir := writeFakeUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	moduleDir := t.TempDir()
	reqs := []string{"requests"}

	res := buildModuleVenv(moduleDir, reqs)
	if !res.GetReady() {
		t.Fatalf("build failed: %q", res.GetError())
	}
	if _, err := os.Stat(filepath.Join(moduleDir, ".venv", "bin", "python3")); err != nil {
		t.Errorf("built venv has no interpreter: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(moduleDir, ".venv", ".rafiki-requirements-hash"))
	if err != nil {
		t.Fatalf("read requirements hash: %v", err)
	}
	if want := pymodules.RequirementsHash(reqs); string(got) != want {
		t.Errorf("hash file = %q, want %q", got, want)
	}
}

func TestBuildVenvSkipsRebuildWhenHashUnchanged(t *testing.T) {
	uvDir := writeFakeUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	log := setFakeUVLog(t)
	moduleDir := t.TempDir()
	reqs := []string{"requests"}

	if res := buildModuleVenv(moduleDir, reqs); !res.GetReady() {
		t.Fatalf("first build failed: %q", res.GetError())
	}
	first := fakeUVInvocations(t, log)

	res := buildModuleVenv(moduleDir, reqs)
	if !res.GetReady() {
		t.Fatalf("second build failed: %q", res.GetError())
	}
	if got := fakeUVInvocations(t, log); got != first {
		t.Errorf("unchanged requirements re-invoked uv: log went %d -> %d lines", first, got)
	}
}

func TestBuildVenvRebuildsWhenRequirementsChange(t *testing.T) {
	uvDir := writeFakeUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	log := setFakeUVLog(t)
	moduleDir := t.TempDir()

	if res := buildModuleVenv(moduleDir, []string{"requests"}); !res.GetReady() {
		t.Fatalf("first build failed: %q", res.GetError())
	}
	before := fakeUVInvocations(t, log)

	newReqs := []string{"requests", "numpy"}
	res := buildModuleVenv(moduleDir, newReqs)
	if !res.GetReady() {
		t.Fatalf("rebuild failed: %q", res.GetError())
	}
	if after := fakeUVInvocations(t, log); after <= before {
		t.Errorf("changed requirements did not rebuild: log stayed at %d lines", after)
	}
	got, err := os.ReadFile(filepath.Join(moduleDir, ".venv", ".rafiki-requirements-hash"))
	if err != nil {
		t.Fatalf("read requirements hash: %v", err)
	}
	if want := pymodules.RequirementsHash(newReqs); string(got) != want {
		t.Errorf("hash file = %q, want %q (the new requirements)", got, want)
	}
}

func TestBuildVenvReportsInstallFailureWithoutTouchingPriorVenv(t *testing.T) {
	uvDir := writeFakeUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	moduleDir := t.TempDir()

	if res := buildModuleVenv(moduleDir, []string{"requests"}); !res.GetReady() {
		t.Fatalf("first build failed: %q", res.GetError())
	}
	py := filepath.Join(moduleDir, ".venv", "bin", "python3")
	prior, err := os.ReadFile(py)
	if err != nil {
		t.Fatalf("prior venv's interpreter missing: %v", err)
	}

	res := buildModuleVenv(moduleDir, []string{"FAIL_THIS_INSTALL"})
	if res.GetReady() {
		t.Fatal("a failed install reported Ready")
	}
	if !strings.Contains(res.GetError(), "simulated install failure") {
		t.Errorf("error %q does not name the install failure", res.GetError())
	}
	after, err := os.ReadFile(py)
	if err != nil {
		t.Fatalf("prior venv's interpreter vanished on a failed rebuild: %v", err)
	}
	if string(after) != string(prior) {
		t.Error("prior venv was modified by a failed rebuild")
	}
}

func TestBuildVenvMissingUvFailsLoudly(t *testing.T) {
	t.Setenv("RAFIKI_PYMODULE_UV", "/nonexistent/uv")
	moduleDir := t.TempDir()

	res := buildModuleVenv(moduleDir, []string{"requests"})
	if res.GetReady() {
		t.Fatal("a build without uv reported Ready")
	}
	if !strings.Contains(res.GetError(), "uv not found") {
		t.Errorf("error %q does not name the missing binary", res.GetError())
	}
	if _, err := os.Stat(filepath.Join(moduleDir, ".venv")); !os.IsNotExist(err) {
		t.Errorf("a missing-uv build created a .venv (err=%v)", err)
	}
}
