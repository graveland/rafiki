// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// venvDirName is the per-module virtualenv directory, a sibling of the
// module's own .py file inside <cache>/pymodules/<name>/.
const venvDirName = ".venv"

// requirementsHashFile marks a built venv with the requirements hash it was
// built from, so an unchanged module skips the rebuild entirely.
const requirementsHashFile = ".rafiki-requirements-hash"

// maxVenvErrorBytes caps the uv output surfaced in a PyModuleVenvResult, so
// a runaway pip traceback cannot blow up the SyncPyModulesResponse.
const maxVenvErrorBytes = 4096

// uvPath returns the uv binary this executor uses to build pymodule venvs:
// RAFIKI_PYMODULE_UV if set, otherwise "uv" resolved from PATH.
func uvPath() string {
	if p := os.Getenv("RAFIKI_PYMODULE_UV"); p != "" {
		return p
	}
	return "uv"
}

// pymodulePythonInterpreter returns the interpreter used to CREATE a module's
// venv: RAFIKI_PYMODULE_PYTHON if set, otherwise "python3". This is the same
// env var name pkg/fundi/tools/pymodule_run.go already reads for the no-venv
// execution path -- read independently here since pkg/executor must not
// import pkg/fundi/tools, but it must resolve to the SAME value on a given
// executor so every venv on that machine shares one interpreter/ABI
// (design doc §2).
func pymodulePythonInterpreter() string {
	if p := os.Getenv("RAFIKI_PYMODULE_PYTHON"); p != "" {
		return p
	}
	return "python3"
}

// buildModuleVenv (re)builds moduleDir's .venv from reqs if needed, and
// reports the result as a *executorpb.PyModuleVenvResult (Name is NOT set by
// this function -- callers fill it in, since this function doesn't know the
// module's name, only its directory).
//
// If len(reqs) == 0: any existing .venv in moduleDir is removed (a module
// whose requirements block was deleted loses its stale venv), and the result
// is {Ready: true, Error: ""}.
//
// Otherwise: compute pymodules.RequirementsHash(reqs). If moduleDir/.venv/
// bin/python3 exists AND moduleDir/.venv/.rafiki-requirements-hash contains
// exactly that hash, the venv is already current -- do nothing, return
// {Ready: true}. Otherwise, rebuild.
func buildModuleVenv(moduleDir string, reqs []string) *executorpb.PyModuleVenvResult {
	venvDir := filepath.Join(moduleDir, venvDirName)
	if len(reqs) == 0 {
		// A module whose requirements block was deleted loses its stale
		// venv; RemoveAll on an absent path is a no-op, so the fresh-module
		// case needs no existence check first.
		if err := os.RemoveAll(venvDir); err != nil {
			return &executorpb.PyModuleVenvResult{
				Ready: false,
				Error: fmt.Sprintf("remove stale venv: %v", err),
			}
		}
		return &executorpb.PyModuleVenvResult{Ready: true}
	}

	hash := pymodules.RequirementsHash(reqs)
	if _, err := os.Stat(filepath.Join(venvDir, "bin", "python3")); err == nil {
		if got, rerr := os.ReadFile(filepath.Join(venvDir, requirementsHashFile)); rerr == nil && string(got) == hash {
			return &executorpb.PyModuleVenvResult{Ready: true}
		}
	}

	// uv is required, with no pip fallback (design §2): a module with a
	// requirements block on an executor lacking uv fails loudly here, before
	// a single byte is written -- this branch must not touch the filesystem.
	uv, err := exec.LookPath(uvPath())
	if err != nil {
		return &executorpb.PyModuleVenvResult{
			Ready: false,
			Error: fmt.Sprintf("uv not found on this executor (required to install pymodule dependencies): %v", err),
		}
	}

	// Stage in a sibling temp dir so a child launching mid-build never
	// observes a half-written venv; the deferred RemoveAll is a no-op once
	// the rename below has moved the tree. Same idiom as writeNamespace.
	staged, err := os.MkdirTemp(moduleDir, ".rafiki-venv-staging-*")
	if err != nil {
		return &executorpb.PyModuleVenvResult{Ready: false, Error: fmt.Sprintf("stage venv: %v", err)}
	}
	defer os.RemoveAll(staged)
	stagedVenv := filepath.Join(staged, "venv")

	// venv creation doesn't need a working directory inside the module, so
	// Dir stays unset.
	out, err := exec.Command(uv, "venv", "--python", pymodulePythonInterpreter(), stagedVenv).CombinedOutput()
	if err != nil {
		return &executorpb.PyModuleVenvResult{Ready: false, Error: uvError(out, err)}
	}

	// One requirement per line, exactly as given -- pip/uv syntax, so a
	// "git+https://..." line passes through unchanged.
	reqFile := filepath.Join(staged, "requirements.txt")
	if err := os.WriteFile(reqFile, []byte(strings.Join(reqs, "\n")+"\n"), 0o644); err != nil {
		return &executorpb.PyModuleVenvResult{Ready: false, Error: fmt.Sprintf("write requirements: %v", err)}
	}

	// Install against the staged venv's own interpreter, with the working
	// directory set to moduleDir (not staged) so a relative local path inside
	// a requirements line, if anyone writes one, resolves against the
	// module's own cache directory, not somewhere surprising (design §1).
	cmd := exec.Command(uv, "pip", "install", "--python", filepath.Join(stagedVenv, "bin", "python3"), "-r", reqFile)
	cmd.Dir = moduleDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		// A failed rebuild leaves any previous working venv in place
		// untouched: only the staging tree is discarded.
		return &executorpb.PyModuleVenvResult{Ready: false, Error: uvError(out, err)}
	}

	if err := os.WriteFile(filepath.Join(stagedVenv, requirementsHashFile), []byte(hash), 0o644); err != nil {
		return &executorpb.PyModuleVenvResult{Ready: false, Error: fmt.Sprintf("write requirements hash: %v", err)}
	}

	// Swap into place. This is NOT a single atomic syscall (a directory
	// can't be atomically replaced the way writePyModule's single-file
	// rename is) -- there is a brief window where .venv does not exist,
	// matching the same acknowledged non-atomicity writeNamespace already
	// carries. Same remove-then-rename idiom, same safety argument: only
	// ever replace a path rafiki itself staged.
	if err := os.RemoveAll(venvDir); err != nil {
		return &executorpb.PyModuleVenvResult{Ready: false, Error: fmt.Sprintf("remove old venv: %v", err)}
	}
	if err := os.Rename(stagedVenv, venvDir); err != nil {
		return &executorpb.PyModuleVenvResult{Ready: false, Error: fmt.Sprintf("publish venv: %v", err)}
	}
	return &executorpb.PyModuleVenvResult{Ready: true}
}

// uvError renders a failed uv invocation as its captured combined
// stdout+stderr, falling back to the exit error itself when the command
// printed nothing at all, then truncating so a runaway pip traceback cannot
// blow up the SyncPyModulesResponse.
func uvError(out []byte, err error) string {
	msg := string(out)
	if strings.TrimSpace(msg) == "" {
		msg = err.Error()
	}
	if len(msg) <= maxVenvErrorBytes {
		return msg
	}
	// Back the cut off to the last rune boundary at or before the cap:
	// protobuf-go rejects invalid UTF-8 in proto3 string fields at marshal
	// time, so a mid-rune slice would fail the entire SyncPyModulesResponse
	// and lose every module's result.
	cut := maxVenvErrorBytes
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "... (truncated)"
}
