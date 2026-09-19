// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"maps"
	"slices"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// connectPyModules adapts *Controller to connectapi.PymoduleManager. The
// owner comes from the request CONTEXT via spawnOwner -- never a request
// field -- exactly as connectLifecycle.Spawn resolves it. No credential
// gate is applied, deliberately (design doc §3): an anonymous unix-socket
// caller writes the shared unattributed bucket, which is the same owner its
// own children resolve to, and a child-attributed caller is a legitimate
// pymodule writer through its owner -- the authority the MCP face already
// grants.
type connectPyModules struct{ c *Controller }

func connectPymoduleRow(rec pymodules.Record) connectapi.PymoduleRow {
	return connectapi.PymoduleRow{
		Version:     rec.ID,
		Name:        rec.Name,
		Description: rec.Description,
		CreatedAt:   rec.CreatedAt.UTC().Format(time.RFC3339),
		Code:        rec.Code,
	}
}

// ListPymodules lists local rows and, when the filter allows, the git
// sources' cached discoveries alongside them. repo is a FILTER here, so an
// empty value legitimately means "everything" — the one place in the feature
// where empty is not the "local" addressing sentinel (design §7): "" and
// "local" both include the blob-store rows, "" and a specific git source's
// name both include that source's rows, and "local" alone excludes the git
// rows entirely.
func (m connectPyModules) ListPymodules(ctx context.Context, repo string) ([]connectapi.PymoduleRow, error) {
	owner := spawnOwner(ctx).UserID
	out := make([]connectapi.PymoduleRow, 0)
	if repo == "" || repo == "local" {
		recs, err := m.c.pymoduleStore.List(ctx, owner)
		if err != nil {
			return nil, err // pymodules.ErrNotFound is impossible here; let other errors surface
		}
		for _, r := range recs {
			row := connectPymoduleRow(r)
			// Blob rows are always the local scope — stamped explicitly now
			// that git-sourced rows share this list.
			row.Repo = "local"
			// The manager contract: code is populated only by Get and Put — an
			// inventory is not a document. The Connect handler blanks it again
			// as defense in depth; this keeps the adapter honest about the
			// contract it feeds.
			row.Code = ""
			out = append(out, row)
		}
	}
	out = append(out, m.gitRows(owner, repo)...)
	return out, nil
}

// gitRows lifts the pusher's cached inventories into list rows, each stamped
// with its own source name as the repo. repo=="" spans every source the
// owner has a snapshot for (sorted by source name, so the list is stable);
// any other non-"local" value narrows to that one source. A named source
// with no cached snapshot yields no rows — a filter matches what it matches,
// it does not invent an error for a name that has never been refreshed.
// Version/CreatedAt/Code stay at their zero values: git-sourced entries have
// neither a version nor a save time, and an inventory is not a document.
func (m connectPyModules) gitRows(owner, repo string) []connectapi.PymoduleRow {
	if repo == "local" || m.c.gitpymodulePusher == nil {
		return nil // "local" excludes git rows; no pusher means no git sources can exist
	}
	inventories := map[string]gitPymoduleInventory{}
	if repo == "" {
		inventories = m.c.gitpymodulePusher.allInventory(owner)
	} else if inv, ok := m.c.gitpymodulePusher.inventoryFor(owner, repo); ok {
		inventories[repo] = inv
	}
	out := make([]connectapi.PymoduleRow, 0)
	for _, name := range slices.Sorted(maps.Keys(inventories)) {
		inv := inventories[name]
		for _, s := range inv.Scripts {
			out = append(out, connectapi.PymoduleRow{
				Name: s.GetName(), Description: s.GetDescription(), Repo: name,
			})
		}
		for _, p := range inv.Packages {
			out = append(out, connectapi.PymoduleRow{
				Name: p.GetName(), Description: p.GetDescription(), Repo: name,
			})
		}
	}
	return out
}

func (m connectPyModules) GetPymodule(ctx context.Context, name string) (connectapi.PymoduleRow, error) {
	rec, err := m.c.pymoduleStore.Get(ctx, spawnOwner(ctx).UserID, name)
	if err != nil {
		return connectapi.PymoduleRow{}, err // unwrapped: connectapi maps pymodules.ErrNotFound
	}
	return connectPymoduleRow(rec), nil
}

func (m connectPyModules) PutPymodule(ctx context.Context, name, code, description string) (connectapi.PymoduleRow, error) {
	rec, err := m.c.pymoduleStore.Put(ctx, spawnOwner(ctx).UserID, name, code, description)
	if err != nil {
		return connectapi.PymoduleRow{}, err
	}
	if m.c.pymodulePusher != nil {
		m.c.pymodulePusher.pushAll(ctx)
	}
	return connectPymoduleRow(rec), nil
}

func (m connectPyModules) DeletePymodule(ctx context.Context, name string) error {
	if err := m.c.pymoduleStore.Delete(ctx, spawnOwner(ctx).UserID, name); err != nil {
		return err
	}
	if m.c.pymodulePusher != nil {
		m.c.pymodulePusher.pushAll(ctx)
	}
	return nil
}
