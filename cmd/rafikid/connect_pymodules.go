// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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

func (m connectPyModules) ListPymodules(ctx context.Context) ([]connectapi.PymoduleRow, error) {
	recs, err := m.c.pymoduleStore.List(ctx, spawnOwner(ctx).UserID)
	if err != nil {
		return nil, err // pymodules.ErrNotFound is impossible here; let other errors surface
	}
	out := make([]connectapi.PymoduleRow, 0, len(recs))
	for _, r := range recs {
		row := connectPymoduleRow(r)
		// The manager contract: code is populated only by Get and Put — an
		// inventory is not a document. The Connect handler blanks it again
		// as defense in depth; this keeps the adapter honest about the
		// contract it feeds.
		row.Code = ""
		out = append(out, row)
	}
	return out, nil
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
