// SPDX-License-Identifier: Apache-2.0

package executor

// Script launch resolution: turning a daraja ScriptParams (repo + script name
// + modules) into the concrete interpreter, argv and PYTHONPATH the hosted
// process runs with.
//
// The resolution reads THIS executor's own synced pymodule cache and never
// runs anything the daemon shipped in the launch itself: blob modules land
// here through SyncPyModules (with their per-module venvs), repos
// through SyncRepo (with their checkout venv). An unsynced name
// is a Launch refusal with pymodule_run's own reasoning, not a runtime
// surprise after the child is up.
//
// The layout constants come from pkg/pymodules (BlobCacheDir) and pkg/repos
// (CacheDir/OpenRepoCheckout) and the venv helpers from pkg/fundi/tools
// (exported for exactly this consumer); the per-branch logic mirrors
// pymodule_run's Execute so a script launched on an executor runs like the
// same pymodule_run call on it would.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/repos"
)

// scriptLaunch is the resolution product: what the executor tells daraja to
// exec, and the PYTHONPATH the hosted process must run with.
type scriptLaunch struct {
	// interpreter is the binary that runs the script — the script's own venv
	// python when one was built for it (or the repo's, for a repo),
	// else the executor's configured fallback.
	interpreter string
	// argv is the full child argv after the interpreter: the script path,
	// then the spec's args verbatim.
	argv []string
	// pythonPath is the FINAL PYTHONPATH value: the computed entries first
	// (call order), then any PYTHONPATH the executor process already carried.
	// The executor hands it to daraja, which passes it through to the script
	// unchanged — the composition happens exactly once, here.
	pythonPath string
	// hold is the shared flock on the repo checkout this script resolves
	// inside (nil for a blob-sourced script). The launched process inherits
	// it through cmd.ExtraFiles, so the sweeper can never remove the commit
	// directory out from under a running script — not even after this
	// executor exits. The parent's own copy is closed once the child holds
	// one; the caller of resolveScriptLaunch owns that close.
	hold *os.File
}

// resolveScriptLaunch resolves sp against this executor's synced cache.
//
// Every name that becomes a path segment or an import is validated here,
// exactly like pymodule_run: each consumption point validates independently,
// never trusting an earlier check (the daemon's validateScriptSpecNames
// already ran, but the executor is the one whose filesystem is about to be
// touched).
func resolveScriptLaunch(sp *darajapb.ScriptParams) (scriptLaunch, error) {
	if sp == nil {
		return scriptLaunch{}, fmt.Errorf("script spec is required")
	}
	if err := pymodules.ValidName(sp.GetScript()); err != nil {
		return scriptLaunch{}, fmt.Errorf("script: %w", err)
	}
	for _, m := range sp.GetModules() {
		if err := pymodules.ValidName(m); err != nil {
			return scriptLaunch{}, fmt.Errorf("module %q: %w", m, err)
		}
	}

	var res scriptLaunch
	var err error
	if sp.GetRepo() == pymodules.LocalRepo {
		res, err = resolveScriptFromBlobCache(sp)
	} else {
		if err := repos.ValidateName(sp.GetRepo()); err != nil {
			return scriptLaunch{}, fmt.Errorf("repo %q: %w", sp.GetRepo(), err)
		}
		res, err = resolveScriptFromGitCheckout(sp)
	}
	if err != nil {
		return scriptLaunch{}, err
	}
	// Fold any pre-existing PYTHONPATH in last, mirroring pymodule_run: the
	// computed entries win, the inherited ones trail.
	if existing := os.Getenv("PYTHONPATH"); existing != "" && res.pythonPath != "" {
		res.pythonPath += string(os.PathListSeparator) + existing
	}
	return res, nil
}

// resolveScriptFromBlobCache is the repo="local" branch: script and modules
// resolve against the synced blob cache, one directory per name, with
// per-module venvs consumed in place. The script's own directory stays off
// PYTHONPATH (it is sys.path[0]); each module's directory joins in call
// order, each preceded by nothing and followed by its venv's site-packages
// when it has one — pymodule_run's exact composition, reordered only by the
// script's site-packages joining first.
func resolveScriptFromBlobCache(sp *darajapb.ScriptParams) (scriptLaunch, error) {
	cache := pymodules.BlobCacheDir()

	scriptDir := filepath.Join(cache, sp.GetScript())
	scriptPath := filepath.Join(scriptDir, sp.GetScript()+".py")
	if _, err := os.Stat(scriptPath); err != nil {
		return scriptLaunch{}, fmt.Errorf(
			"script %q is not synced to this executor (has it been saved with pymodule_put yet?): %w",
			sp.GetScript(), err)
	}
	scriptCode, err := os.ReadFile(scriptPath)
	if err != nil {
		return scriptLaunch{}, fmt.Errorf("script %q: %w", sp.GetScript(), err)
	}
	scriptVenvPython := tools.PymoduleVenvPython(scriptDir)
	_, venvErr := os.Stat(scriptVenvPython)
	scriptHasVenv := venvErr == nil
	// A requirements block without a built venv runs against missing imports;
	// refuse with the same readiness reasoning pymodule_run gives (including
	// the build-in-progress retry case).
	if err := tools.PymoduleVenvReadiness("script", sp.GetScript(), scriptDir,
		pymodules.ParseRequirements(string(scriptCode)), scriptHasVenv); err != nil {
		return scriptLaunch{}, err
	}

	type entry struct {
		dir     string
		hasVenv bool
	}
	entries := make([]entry, 0, len(sp.GetModules()))
	for _, m := range sp.GetModules() {
		dir := filepath.Join(cache, m)
		path := filepath.Join(dir, m+".py")
		if _, err := os.Stat(path); err != nil {
			return scriptLaunch{}, fmt.Errorf(
				"module %q is not synced to this executor (has it been saved with pymodule_put yet?): %w", m, err)
		}
		_, venvErr := os.Stat(tools.PymoduleVenvPython(dir))
		hasVenv := venvErr == nil
		code, err := os.ReadFile(path)
		if err != nil {
			return scriptLaunch{}, fmt.Errorf("module %q: %w", m, err)
		}
		if err := tools.PymoduleVenvReadiness("module", m, dir,
			pymodules.ParseRequirements(string(code)), hasVenv); err != nil {
			return scriptLaunch{}, err
		}
		entries = append(entries, entry{dir: dir, hasVenv: hasVenv})
	}

	var pp []string
	if scriptHasVenv {
		if sitePkgs, ok := tools.PymoduleSitePackages(scriptDir); ok {
			pp = append(pp, sitePkgs)
		}
	}
	for _, e := range entries {
		pp = append(pp, e.dir)
		if e.hasVenv {
			if sitePkgs, ok := tools.PymoduleSitePackages(e.dir); ok {
				pp = append(pp, sitePkgs)
			}
		}
	}

	interpreter := scriptInterpreter()
	if scriptHasVenv {
		// A venv runs its OWN python: the run stays self-consistent with the
		// venv that was built, whatever the fallback resolves to today.
		interpreter = scriptVenvPython
	}
	return scriptLaunch{
		interpreter: interpreter,
		argv:        append([]string{scriptPath}, sp.GetArgs()...),
		pythonPath:  strings.Join(pp, string(os.PathListSeparator)),
	}, nil
}

// resolveScriptFromGitCheckout is the any-other-repo branch: the script and
// modules resolve within the named source's synced checkout — scripts under
// scripts/, importable packages as top-level directories, ONE repo-wide venv
// as the interpreter when its build succeeded. There is deliberately no
// requirements-block readiness check here: a checkout's dependencies come
// from its own manifest (uv sync's input), not from a per-name block, and a
// hard refusal would black out every script of the repo, ready or not — the
// refresh surface reports venv state for exactly this.
func resolveScriptFromGitCheckout(sp *darajapb.ScriptParams) (scriptLaunch, error) {
	// Resolve `current` to the commit directory it names RIGHT NOW and hold it
	// there: the launch runs out of the commit that was current when it
	// started, whatever a refresh does to `current` afterwards.
	checkoutDir, hold, err := repos.OpenRepoCheckout(sp.GetRepo())
	if err != nil {
		return scriptLaunch{}, fmt.Errorf(
			"script %q is not synced to this executor under repo %q: %w",
			sp.GetScript(), sp.GetRepo(), err)
	}
	// Every refusal below leaves nothing running, so it must also leave no
	// lock behind.
	resolved := false
	defer func() {
		if !resolved {
			hold.Close()
		}
	}()

	scriptPath := filepath.Join(checkoutDir, repos.ScriptsDirName, sp.GetScript()+".py")
	if _, err := os.Stat(scriptPath); err != nil {
		return scriptLaunch{}, fmt.Errorf(
			"script %q is not synced to this executor under repo %q (has the repo been refreshed on it yet?): %w",
			sp.GetScript(), sp.GetRepo(), err)
	}

	interpreter := scriptInterpreter()
	if repoVenv := tools.PymoduleVenvPython(checkoutDir); func() bool {
		_, err := os.Stat(repoVenv)
		return err == nil
	}() {
		interpreter = repoVenv
	}

	// The checkout root joins PYTHONPATH unconditionally — added once, not
	// once per module — so a script's own intra-repo imports resolve without
	// the caller naming which top-level packages the repo contains. Each
	// named module must be a directory of the SAME checkout; there is no
	// cross-source composition.
	pp := []string{checkoutDir}
	for _, m := range sp.GetModules() {
		dir := filepath.Join(checkoutDir, m)
		fi, err := os.Stat(dir)
		if err != nil {
			return scriptLaunch{}, fmt.Errorf(
				"module %q is not synced to this executor under repo %q (has the repo been refreshed on it yet?): %w",
				m, sp.GetRepo(), err)
		}
		if !fi.IsDir() {
			return scriptLaunch{}, fmt.Errorf("module %q under repo %q is not a package directory", m, sp.GetRepo())
		}
		pp = append(pp, dir)
	}

	resolved = true
	return scriptLaunch{
		interpreter: interpreter,
		argv:        append([]string{scriptPath}, sp.GetArgs()...),
		pythonPath:  strings.Join(pp, string(os.PathListSeparator)),
		hold:        hold,
	}, nil
}

// scriptInterpreter mirrors pymodule_run's Materialize: the operator's
// RAFIKI_PYMODULE_PYTHON on THIS executor, else python3. Read from this
// process's environment at launch time — the script's own environment
// deliberately carries no RAFIKI_* variable.
func scriptInterpreter() string {
	if p := os.Getenv("RAFIKI_PYMODULE_PYTHON"); p != "" {
		return p
	}
	return "python3"
}
