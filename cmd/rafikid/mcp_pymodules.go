// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/users"
)

// mcpPyModuleStore adapts *Controller to tools.PyModuleStore for the MCP
// face, bound to one caller's owner user id at construction -- the same
// binding rule as agent_pymodules.go's pymoduleWriter, which this mirrors,
// but keyed off the MCP caller's users.Identity rather than a fundi child's
// resolved owner.
type mcpPyModuleStore struct {
	ctrl        *Controller
	ownerUserID string
}

// newMCPPyModuleStore binds owner.UserID. An empty UserID (should not occur
// on this face -- getServer only reaches here for a ProvenanceUser or
// ProvenanceChildToken identity, both of which carry one) is passed through
// unchanged, matching pymodules.Store's own unattributed-bucket convention.
func newMCPPyModuleStore(ctrl *Controller, owner users.Identity) *mcpPyModuleStore {
	return &mcpPyModuleStore{ctrl: ctrl, ownerUserID: owner.UserID}
}

// repo is always "local" by the time the tool layer calls in (it rejects
// every other value), but the parameter stays: tools.PyModuleStore requires
// it, and dropping it here would hide the addressing scheme the interface
// speaks.
func (w *mcpPyModuleStore) Put(ctx context.Context, repo, name, code, description string) (int64, string, error) {
	// Same ordering contract as pymoduleWriter.Put: syntax blocks, lint is
	// advisory and pre-write, the row is written next, and the push's venv
	// results are collected LAST, after the row exists.
	if syntaxErr := pymoduleSyntaxCheck(code); syntaxErr != "" {
		// No "pymodule_put: " prefix here: the tool layer wraps every store
		// error with that prefix, so adding it twice doubles it in the text
		// an MCP caller actually sees.
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

// repo is always "local" by the time the tool layer calls in (it rejects
// every other value), but the parameter stays: tools.PyModuleStore requires
// it.
func (w *mcpPyModuleStore) Delete(ctx context.Context, repo, name string) (string, error) {
	if err := w.ctrl.pymoduleStore.Delete(ctx, w.ownerUserID, name); err != nil {
		return "", err
	}
	var notice string
	if w.ctrl.pymodulePusher != nil {
		notice = formatVenvFailures(w.ctrl.pymodulePusher.pushAll(ctx))
	}
	return notice, nil
}

// Get reads the latest live version of a module under this store's bound
// owner. Read-only: no push, nothing to fan out.
// repo is always "local" by the time the tool layer calls in (it rejects
// every other value; serving a git-sourced get is deferred -- the daemon
// caches names and descriptions only, never file content), but the
// parameter stays: tools.PyModuleStore requires it.
func (w *mcpPyModuleStore) Get(ctx context.Context, repo, name string) (pymodules.Record, error) {
	return w.ctrl.pymoduleStore.Get(ctx, w.ownerUserID, name)
}
