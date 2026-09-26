// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// gitPymoduleReposRoot is the parent cache root every git checkout lives
// under: <cache>/pymodule-repos -- a sibling cache root to pymoduleCacheDir(),
// not nested inside it, since a git checkout's own layout (including .git/)
// must not be interleaved with the flat per-module directories
// pymodule_sync.go manages and prunes by name.
func gitPymoduleReposRoot() string {
	return pymodules.GitCacheDir()
}

func gitPymoduleRepoDir(name string) string {
	return filepath.Join(gitPymoduleReposRoot(), name)
}

// ensureGitReposRoot establishes the pymodule-repos cache root exactly the
// way SyncPyModules establishes pymoduleCacheDir(): only ever write into a
// directory rafiki marked, or one that does not exist yet. Checked BEFORE
// MkdirAll: creating the directory first would make the absent case
// unrecognisable -- an empty dir is not a managed one. A root that exists
// without the marker (or is not a directory at all) is refused, never
// adopted: refreshGitCheckout would otherwise clone into, fetch, hard-reset
// and clean -fdx inside whatever an operator or another program left there.
func ensureGitReposRoot(root string) error {
	if err := assertManagedOrAbsent(root); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, managedMarker)); !os.IsNotExist(err) {
		return nil // exists and is already marked ours
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create pymodule-repos dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, managedMarker), []byte("pymodule-repos\n"), 0o644); err != nil {
		return fmt.Errorf("mark pymodule-repos dir: %w", err)
	}
	return nil
}

// requirementsOnlyVenvError is the venvError for a checkout whose only
// dependency manifest is a bare requirements.txt: `uv sync` cannot consume
// one, so the build is not attempted and venv_ready stays false rather than
// promising a venv that was never built.
const requirementsOnlyVenvError = "repo carries only requirements.txt; rafiki builds git-source venvs with `uv sync`, which needs a pyproject.toml — add one that declares the dependency"

// SyncPyModuleGitSource refreshes ONE named git source in this executor's
// rafiki-managed pymodule-repo cache: clone it fresh or fetch-and-reset an
// existing checkout onto ref's current tip, discover the scripts and packages
// the checkout contains, and build the repo's one shared venv.
//
// Per-source, not whole-corpus like SyncPyModules: a git source's own history
// is already the versioning and pruning mechanism, so there is no "prune
// what's absent" model to replicate here.
//
// A failed git operation does not error the RPC: it returns a response with
// VenvReady=false and the git output in VenvError and empty inventory. A git
// checkout left in an unknown state is not a state to discover or build
// against, but the daemon still wants the failure REPORTED rather than
// mistaking it for an unreachable executor.
func (s *Server) SyncPyModuleGitSource(
	_ context.Context,
	req *connect.Request[executorpb.SyncPyModuleGitSourceRequest],
) (*connect.Response[executorpb.SyncPyModuleGitSourceResponse], error) {
	if !s.opts.PymoduleGitSync {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("this executor does not accept pymodule git-source syncs"))
	}

	// The name becomes a path segment under the cache root: guard it before
	// touching disk, the same way SyncPyModules guards its module names.
	if err := validSegment(req.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("source name: %w", err))
	}

	// A leading dash makes git parse the argument as an option instead of a
	// value: `git fetch origin --upload-pack=<cmd>` executes <cmd> locally on
	// this executor. Both values arrive from the daemon's store, but the
	// executor re-checks at its own boundary, before any subprocess runs --
	// the same shape validSegment applies to the name. Guarded on BOTH url
	// and ref: the clone path is not exploitable in this shape today, but
	// the guard is cheaper than trusting that accident to hold.
	if url := req.Msg.GetUrl(); strings.HasPrefix(url, "-") {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("url %q begins with a dash and would parse as a git option, not a url", url))
	}
	if ref := req.Msg.GetRef(); strings.HasPrefix(ref, "-") {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("ref %q begins with a dash and would parse as a git option, not a ref", ref))
	}

	dir := gitPymoduleRepoDir(req.Msg.GetName())

	// Establish the checkout root the same way the blob path establishes its
	// cache dir, BEFORE any clone/fetch/clean: refuse rather than adopt a root
	// this executor never created.
	if err := ensureGitReposRoot(gitPymoduleReposRoot()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if out, err := refreshGitCheckout(dir, req.Msg.GetUrl(), req.Msg.GetRef()); err != nil {
		return connect.NewResponse(&executorpb.SyncPyModuleGitSourceResponse{
			VenvReady: false,
			VenvError: uvError(out, err), // same 4096-byte cap as a failed uv build
		}), nil
	}

	scripts, packages, err := gitpymodules.Discover(dir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &executorpb.SyncPyModuleGitSourceResponse{}
	for _, sc := range scripts {
		resp.Scripts = append(resp.Scripts,
			&executorpb.GitSourceScript{Name: sc.Name, Description: sc.Description})
	}
	for _, p := range packages {
		resp.Packages = append(resp.Packages,
			&executorpb.GitSourcePackage{Name: p.Name, Description: p.Description})
	}

	// A broken dependency build only fails the runs that need the venv: the
	// inventory still reports (design §5), the same posture that keeps a
	// failed blob-sourced rebuild from hiding the module itself.
	venvErr := buildRepoVenv(dir)
	resp.VenvReady = venvErr == ""
	resp.VenvError = venvErr
	return connect.NewResponse(resp), nil
}

// refreshGitCheckout establishes or updates dir's checkout of url at ref,
// running git as subprocesses and returning the failed command's combined
// output with its error so the caller can surface it verbatim. A checkout
// with no .git yet is cloned fresh (then checked out at ref); an existing
// one is fetched, hard-reset onto the fetched tip, and cleaned of everything
// untracked or ignored -- the closest equivalent to blob-sync's "whole corpus
// every time" built from git's own primitives.
func refreshGitCheckout(dir, url, ref string) ([]byte, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		// git clone takes the target directory as its own argument, so the
		// subprocess's working directory is the clone's PARENT; create it
		// first or exec fails before git ever runs.
		parent := filepath.Dir(dir)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", parent, err)
		}
		out, err := gitOutput(parent, "clone", url, dir)
		if err != nil {
			return out, fmt.Errorf("git clone %s: %w", url, err)
		}
		out, err = gitOutput(dir, "checkout", ref)
		if err != nil {
			return out, fmt.Errorf("git checkout %s: %w", ref, err)
		}
		return nil, nil
	}

	out, err := gitOutput(dir, "fetch", "origin", ref)
	if err != nil {
		return out, fmt.Errorf("git fetch origin %s: %w", ref, err)
	}
	out, err = gitOutput(dir, "reset", "--hard", "FETCH_HEAD")
	if err != nil {
		return out, fmt.Errorf("git reset --hard: %w", err)
	}
	out, err = gitOutput(dir, "clean", "-fdx")
	if err != nil {
		return out, fmt.Errorf("git clean -fdx: %w", err)
	}
	return nil, nil
}

// gitOutput runs one git command in dir and returns its combined output. It is
// deliberately NOT path-guarded: every caller passes a path built from a
// validSegment-validated name, and git's own ref resolution handles the
// arguments.
func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// buildRepoVenv builds the checkout's ONE shared venv at dir/.venv by running
// `uv sync` against the checkout's own manifest -- rafiki parses no manifest
// itself, it only points uv at the checkout root and lets uv figure out its
// own input. It returns "" when the venv is ready to use, or the capped
// combined output of the failed build when not.
//
// The manifest gate is file existence only, never parsing, and it is a
// three-way check:
//
//   - pyproject.toml present: `uv sync` builds against it (the normal path).
//   - only requirements.txt present: not ready, one clear sentence -- uv sync
//     cannot consume a bare requirements file, and reporting venv_ready=true
//     would promise a venv that was never built, its dependencies missing at
//     run time. The discovered inventory still rides the same response, the
//     same rule as any failed build.
//   - neither file: no dependencies declared at all -- nothing to build, the
//     venv is reported ready with no uv invocation, exactly as the response's
//     venv_ready contract promises.
func buildRepoVenv(dir string) string {
	_, err := os.Stat(filepath.Join(dir, "pyproject.toml"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Sprintf("stat pyproject.toml: %v", err)
	}
	if os.IsNotExist(err) {
		_, rerr := os.Stat(filepath.Join(dir, "requirements.txt"))
		switch {
		case rerr == nil:
			return requirementsOnlyVenvError
		case os.IsNotExist(rerr):
			return ""
		default:
			return fmt.Sprintf("stat requirements.txt: %v", rerr)
		}
	}

	// Same uv-required posture as buildModuleVenv: no pip fallback, and the
	// same resolution (RAFIKI_PYMODULE_UV, else PATH) -- do not reimplement.
	uv, err := exec.LookPath(uvPath())
	if err != nil {
		return fmt.Sprintf("uv not found on this executor (required to install pymodule dependencies): %v", err)
	}

	// Stage in a sibling temp dir so pymodule_run never observes a half-built
	// venv; the deferred RemoveAll is a no-op once the rename below has moved
	// the tree out. Same idiom as buildModuleVenv.
	staged, err := os.MkdirTemp(dir, ".rafiki-venv-staging-*")
	if err != nil {
		return fmt.Sprintf("stage venv: %v", err)
	}
	defer os.RemoveAll(staged)
	stagedVenv := filepath.Join(staged, "venv")

	// UV_PROJECT_ENVIRONMENT is how a single `uv sync` is pointed at a venv
	// location of the caller's choosing; without it uv builds into the
	// checkout's own .venv and the swap below would have nothing to move.
	cmd := exec.Command(uv, "sync", "--python", pymodulePythonInterpreter())
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+stagedVenv)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A failed build discards the staging tree and leaves any previous
		// venv in place untouched, exactly like a failed blob-sourced rebuild.
		return uvError(out, err)
	}

	// Swap into place. NOT a single atomic syscall -- there is a brief window
	// where .venv does not exist, the same acknowledged non-atomicity
	// buildModuleVenv carries; only ever replace a path rafiki itself staged.
	venvDir := filepath.Join(dir, venvDirName)
	if err := os.RemoveAll(venvDir); err != nil {
		return fmt.Sprintf("remove old venv: %v", err)
	}
	if err := os.Rename(stagedVenv, venvDir); err != nil {
		return fmt.Sprintf("publish venv: %v", err)
	}
	return ""
}
