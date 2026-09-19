// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// pymoduleWriter adapts *Controller to tools.PyModuleStore, bound to one
// caller's owner user id at construction -- the same reasoning as
// conversationReader being bound to one Scope.
type pymoduleWriter struct {
	ctrl        *Controller
	ownerUserID string
}

// newControllerPyModuleWriter binds a fundi child's own owner. An empty
// ownerUserID (anonymous spawn) is passed through unchanged: pymodules.Store
// treats an empty owner as one shared "unattributed" bucket, never global.
func newControllerPyModuleWriter(c *Controller, ownerUserID string) *pymoduleWriter {
	return &pymoduleWriter{ctrl: c, ownerUserID: ownerUserID}
}

// repo is always "local" by the time the tool layer calls in (it rejects
// every other value), but the parameter stays: tools.PyModuleStore requires
// it, and dropping it here would hide the addressing scheme the interface
// speaks.
func (w *pymoduleWriter) Put(ctx context.Context, repo, name, code, description string) (int64, string, error) {
	// ORDER: block on a definite syntax error before anything else; then run
	// the best-effort lint (its finding is about the code being saved, so it
	// must see the exact text); then write the row; then push and collect
	// venv results LAST, after the row exists. Lint and venv findings only
	// ever land in the advisory notice -- they never block the save.
	if syntaxErr := pymoduleSyntaxCheck(code); syntaxErr != "" {
		// No "pymodule_put: " prefix here: the tool layer wraps every store
		// error with that prefix, so adding it twice doubles it in the text
		// the calling agent actually sees.
		return 0, "", fmt.Errorf("%s does not parse as Python: %s", name, syntaxErr)
	}
	lint := pymoduleLintCheck(code)

	rec, err := w.ctrl.pymoduleStore.Put(ctx, w.ownerUserID, name, code, description)
	if err != nil {
		return 0, "", err
	}
	var notice string
	if lint != "" {
		notice += "ruff found:\n" + lint
	}
	if w.ctrl.pymodulePusher != nil {
		results := w.ctrl.pymodulePusher.pushAll(ctx)
		if failMsg := formatVenvFailures(results); failMsg != "" {
			if notice != "" {
				notice += "\n\n"
			}
			notice += failMsg
		}
	}
	return rec.ID, notice, nil
}

// Delete soft-deletes the module under this writer's bound owner and pushes
// the owner's corpus so executors prune it promptly, reporting any venv
// build failures the sync surfaced as the advisory notice. Nothing is pushed
// when the delete reports not-found -- the corpus did not change. There is
// no syntax/lint check here: there is nothing left to check.
// repo is always "local" by the time the tool layer calls in (it rejects
// every other value), but the parameter stays: tools.PyModuleStore requires
// it, and dropping it here would hide the addressing scheme the interface
// speaks.
func (w *pymoduleWriter) Delete(ctx context.Context, repo, name string) (string, error) {
	if err := w.ctrl.pymoduleStore.Delete(ctx, w.ownerUserID, name); err != nil {
		return "", err
	}
	var notice string
	if w.ctrl.pymodulePusher != nil {
		notice = formatVenvFailures(w.ctrl.pymodulePusher.pushAll(ctx))
	}
	return notice, nil
}

// Get reads the latest live version of a module under this writer's bound
// owner. Read-only: no push, nothing to fan out.
// repo is always "local" by the time the tool layer calls in (it rejects
// every other value; serving a git-sourced get is deferred -- the daemon
// caches names and descriptions only, never file content), but the
// parameter stays: tools.PyModuleStore requires it.
func (w *pymoduleWriter) Get(ctx context.Context, repo, name string) (pymodules.Record, error) {
	return w.ctrl.pymoduleStore.Get(ctx, w.ownerUserID, name)
}

// pymoduleInventory renders ownerUserID's saved modules as "name —
// description" lines, one per line, for the dynamic skill body. After the
// local rows it appends every entry from the owner's git sources' cached
// inventory, each labeled "reponame/name" so the agent can tell which repo
// value to pass back; a nil gitpymodulePusher (no exec pool) skips that
// section entirely rather than erroring. Returns a clear "nothing saved
// yet" line when BOTH scopes are empty rather than an empty string, since
// an empty tool result reads as an error to a model, not as an empty list.
func pymoduleInventory(ctrl *Controller, ownerUserID string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		recs, err := ctrl.pymoduleStore.List(ctx, ownerUserID)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, r := range recs {
			fmt.Fprintf(&b, "%s — %s\n", r.Name, r.Description)
		}
		if ctrl.gitpymodulePusher != nil {
			for _, info := range gitPymoduleInfos(ctrl.gitpymodulePusher, ownerUserID) {
				fmt.Fprintf(&b, "%s/%s — %s\n", info.Repo, info.Name, info.Description)
			}
		}
		if b.Len() == 0 {
			return "No pymodules saved yet. Use pymodule_put to save one.", nil
		}
		return b.String(), nil
	}
}

// gitPymoduleInfos flattens the git pusher's cached per-source inventory for
// ownerUserID into one labeled entry per discovered script and package,
// each entry's Repo naming its source (the same "reponame/name" labeling the
// skill body renders). Sorted by repo then name: allInventory's map has no
// stable order, and the same two calls should render the same list.
func gitPymoduleInfos(gp *gitPymodulePusher, ownerUserID string) []tools.PyModuleInfo {
	out := make([]tools.PyModuleInfo, 0)
	for repo, inv := range gp.allInventory(ownerUserID) {
		out = append(out, gitSourcePymoduleInfos(repo, inv)...)
	}
	sortPyModuleInfos(out)
	return out
}

// gitSourcePymoduleInfos labels ONE source's inventory with its own name --
// the per-scope slice of gitPymoduleInfos' whole-owner flattening, for the
// filtered listing.
func gitSourcePymoduleInfos(repo string, inv gitPymoduleInventory) []tools.PyModuleInfo {
	out := make([]tools.PyModuleInfo, 0)
	for _, s := range inv.Scripts {
		out = append(out, tools.PyModuleInfo{Repo: repo, Name: s.GetName(), Description: s.GetDescription()})
	}
	for _, p := range inv.Packages {
		out = append(out, tools.PyModuleInfo{Repo: repo, Name: p.GetName(), Description: p.GetDescription()})
	}
	return out
}

// sortPyModuleInfos orders entries by repo then name, so a listing does not
// shuffle between two identical calls.
func sortPyModuleInfos(out []tools.PyModuleInfo) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Name < out[j].Name
	})
}
